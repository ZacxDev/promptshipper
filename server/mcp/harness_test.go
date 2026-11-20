package main

// Test harness: an in-process fake of the clip server's HTTP contract
// (server/clip/README.md), and a driver that runs the REAL run() wiring —
// flags, config, redacting writers, stdio transport — against an MCP client
// over pipes, capturing the raw bytes of both output streams.
//
// The fake is checked against the real clipstore by realclip_test.go, which
// runs the same tool calls against a built clipstore binary in CI.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testToken is pairwise distinct from every other fixture string and from the
// redaction marker, and long enough that a prefix of it is itself distinctive.
const testToken = "Tk9q-unit-test-token.2f7b~c0ffee-8d1e4a"

type fakeClip struct {
	mu     sync.Mutex
	token  string
	clips  []clip // newest first
	nextID int64
	now    int64
	// hooks for failure-mode tests
	listStatus int    // non-zero: GET answers this status with listBody
	listBody   string // raw body for the forced status
	authSeen   int    // requests that carried the right token (positive control)
}

func newFakeClip(token string) *fakeClip {
	return &fakeClip{token: token, nextID: 1, now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli()}
}

func (f *fakeClip) add(body, device string) clip {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putLocked(body, device)
}

func (f *fakeClip) putLocked(body, device string) clip {
	f.now += 1000
	for i, c := range f.clips {
		if c.Body == body {
			c.CreatedAt, c.Device, c.BumpCount = f.now, device, c.BumpCount+1
			f.clips = append(append([]clip{c}, f.clips[:i]...), f.clips[i+1:]...)
			return c
		}
	}
	c := clip{ID: f.nextID, Body: body, Device: device, CreatedAt: f.now}
	f.nextID += 2 // ids have gaps in the real server; never let a test assume they don't
	f.clips = append([]clip{c}, f.clips...)
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeClip) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		writeJSON(w, 401, map[string]string{"error": "unauthorized", "detail": "a valid token is required"})
		return
	}
	f.authSeen++
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/clips":
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			_, _ = io.WriteString(w, f.listBody)
			return
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 500 {
				writeJSON(w, 400, map[string]string{"error": "invalid_limit", "detail": "`limit` must be an integer in 1..500"})
				return
			}
			limit = n
		}
		q := strings.Fields(strings.ToLower(r.URL.Query().Get("q")))
		out := []clip{}
		for _, c := range f.clips {
			ok := true
			for _, t := range q {
				ok = ok && strings.Contains(strings.ToLower(c.Body), t)
			}
			if ok && len(out) < limit {
				out = append(out, c)
			}
		}
		writeJSON(w, 200, map[string]any{"clips": out, "limit": limit})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/clips":
		var req struct{ Body, Device string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid_json", "detail": "not JSON"})
			return
		}
		if req.Body == "" {
			writeJSON(w, 400, map[string]string{"error": "empty_body", "detail": "`body` is required"})
			return
		}
		if strings.TrimSpace(req.Device) == "" {
			writeJSON(w, 400, map[string]string{"error": "missing_device", "detail": "`device` is required"})
			return
		}
		before := len(f.clips)
		c := f.putLocked(req.Body, req.Device)
		deduped := len(f.clips) == before
		status := 201
		if deduped {
			status = 200
		}
		writeJSON(w, status, map[string]any{"id": c.ID, "body": c.Body, "device": c.Device,
			"created_at": c.CreatedAt, "bump_count": c.BumpCount, "deduped": deduped})
	default:
		writeJSON(w, 404, map[string]string{"error": "not_found", "detail": "no such route"})
	}
}

// syncBuf is a goroutine-safe byte sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type readCloser struct {
	io.Reader
	io.Closer
}

// session is a running run() with a connected MCP client.
type session struct {
	cs     *mcp.ClientSession
	rawOut *syncBuf // every byte run() wrote to stdout
	rawErr *syncBuf // every byte run() wrote to stderr
	done   chan int
	stop   func()
}

// startSession runs the real run() with env and args, and connects a client.
func startSession(t *testing.T, env map[string]string, args ...string) *session {
	t.Helper()
	inR, inW := io.Pipe()   // client -> server stdin
	outR, outW := io.Pipe() // server stdout -> client
	s := &session{rawOut: &syncBuf{}, rawErr: &syncBuf{}, done: make(chan int, 1)}
	getenv := func(k string) string { return env[k] }
	go func() { s.done <- run(args, getenv, inR, outW, s.rawErr); _ = outW.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.IOTransport{
		Reader: readCloser{io.TeeReader(outR, s.rawOut), outR},
		Writer: inW,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v (stderr: %s)", err, s.rawErr.String())
	}
	s.cs = cs
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			_ = cs.Close()
			_ = inW.Close()
			select {
			case <-s.done:
			case <-time.After(5 * time.Second):
				t.Errorf("server did not exit after stdin closed")
			}
		})
	}
	t.Cleanup(s.stop)
	return s
}

// call invokes a tool and returns the result and its full JSON encoding.
func (s *session) call(t *testing.T, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	b, _ := json.Marshal(res)
	return res, string(b)
}

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var v T
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("structured content %s: %v", b, err)
	}
	return v
}

func envFor(srv *httptest.Server) map[string]string {
	return map[string]string{envClipURL: srv.URL, envClipToken: testToken}
}
