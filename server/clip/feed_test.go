package main

// The change feed (GET /v1/clips?after=&wait=) and the exact fetch
// (GET /v1/clips/{id}), driven over real HTTP against a real Store, so the
// waiter, the store and the routing are exercised together.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const feedToken = "feed-test-token-0123456789abcdef"

func feedServer(t *testing.T) (*httptest.Server, *API) {
	t.Helper()
	api := NewAPI(newTestStore(t), feedToken, time.Now)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, api
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewBufferString(body)
	}
	// A hard client deadline above every wait these tests use (max 30 s), so a
	// waiter that never returns FAILS this test by name instead of hanging the
	// whole suite into go test's global timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+feedToken)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func postClip(t *testing.T, srv *httptest.Server, body, device string) Clip {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"body": body, "device": device})
	code, raw := do(t, srv, "POST", "/v1/clips", string(b), true)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("POST %q: %d %s", body, code, raw)
	}
	var c Clip
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

type feedBody struct {
	Clips *[]Clip `json:"clips"`
	Limit int     `json:"limit"`
}

func feed(t *testing.T, srv *httptest.Server, query string) (int, feedBody, time.Duration) {
	t.Helper()
	t0 := time.Now()
	code, raw := do(t, srv, "GET", "/v1/clips?"+query, "", true)
	var fb feedBody
	if code == http.StatusOK {
		if err := json.Unmarshal(raw, &fb); err != nil {
			t.Fatalf("feed body: %v: %s", err, raw)
		}
	}
	return code, fb, time.Since(t0)
}

func ids(cs []Clip) []int64 {
	out := []int64{}
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func TestTheFeedReturnsClipsAfterTheCursorOldestFirst(t *testing.T) {
	srv, _ := feedServer(t)
	a := postClip(t, srv, "first", "phone")
	b := postClip(t, srv, "second", "mcp")
	c := postClip(t, srv, "third", "laptop")

	code, fb, _ := feed(t, srv, "after=0")
	if code != 200 || fb.Clips == nil {
		t.Fatalf("after=0: %d %+v", code, fb)
	}
	if got, want := ids(*fb.Clips), []int64{a.ID, b.ID, c.ID}; !equalIDs(got, want) {
		t.Fatalf("after=0 returned %v, want oldest-first %v", got, want)
	}
	code, fb, _ = feed(t, srv, "after="+strconv.FormatInt(a.ID, 10))
	if got, want := ids(*fb.Clips), []int64{b.ID, c.ID}; code != 200 || !equalIDs(got, want) {
		t.Fatalf("after=%d returned %d %v, want %v", a.ID, code, got, want)
	}
	if (*fb.Clips)[0].Device != "mcp" || (*fb.Clips)[0].Body != "second" {
		t.Fatalf("feed rows must carry body and device: %+v", (*fb.Clips)[0])
	}
}

func TestAWaiterIsWokenByAConcurrentPost(t *testing.T) {
	srv, _ := feedServer(t)
	seed := postClip(t, srv, "already here", "phone")

	type result struct {
		code int
		fb   feedBody
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		code, fb, took := feed(t, srv, "after="+strconv.FormatInt(seed.ID, 10)+"&wait=20")
		done <- result{code, fb, took}
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("the waiter returned before anything was posted: %d %+v", r.code, r.fb)
	default:
	}
	pushed := postClip(t, srv, "pushed by an agent", "mcp")

	select {
	case r := <-done:
		if r.code != 200 || r.fb.Clips == nil || !equalIDs(ids(*r.fb.Clips), []int64{pushed.ID}) {
			t.Fatalf("woken waiter got %d %+v, want exactly clip %d", r.code, r.fb, pushed.ID)
		}
		if r.took > 5*time.Second {
			t.Fatalf("the waiter took %v: it was not woken by the POST, it timed out", r.took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter was not woken by a concurrent POST within 10s (wait was 20s)")
	}
}

func TestAWaitThatExpiresReturnsAnEmptyListNotNull(t *testing.T) {
	srv, _ := feedServer(t)
	c := postClip(t, srv, "only one", "phone")
	code, fb, took := feed(t, srv, "after="+strconv.FormatInt(c.ID, 10)+"&wait=1")
	if code != 200 || fb.Clips == nil || len(*fb.Clips) != 0 {
		t.Fatalf("expired wait: %d %+v, want 200 with clips: []", code, fb)
	}
	if took < 900*time.Millisecond || took > 5*time.Second {
		t.Fatalf("wait=1 returned after %v, want ~1s", took)
	}
}

func TestFeedParametersAreValidatedNotClamped(t *testing.T) {
	srv, _ := feedServer(t)
	for _, q := range []string{"after=-1", "after=x", "after=0&wait=61", "after=0&wait=-1",
		"after=0&wait=x", "wait=5", "after=0&q=foo"} {
		if code, _, _ := feed(t, srv, q); code != http.StatusBadRequest {
			t.Errorf("?%s answered %d, want 400", q, code)
		}
	}
}

func TestFetchingOneClipByID(t *testing.T) {
	srv, _ := feedServer(t)
	c := postClip(t, srv, "exact fetch", "mcp")
	code, raw := do(t, srv, "GET", "/v1/clips/"+strconv.FormatInt(c.ID, 10), "", true)
	var got Clip
	_ = json.Unmarshal(raw, &got)
	if code != 200 || got != c {
		t.Fatalf("GET /v1/clips/%d: %d %s, want %+v", c.ID, code, raw, c)
	}
	code, raw = do(t, srv, "GET", "/v1/clips/999999", "", true)
	if code != http.StatusNotFound || !bytes.Contains(raw, []byte(`"no_such_clip"`)) {
		t.Fatalf("absent id: %d %s, want 404 no_such_clip", code, raw)
	}
	if code, _ = do(t, srv, "GET", "/v1/clips/abc", "", true); code != http.StatusBadRequest {
		t.Fatalf("non-numeric id: %d, want 400", code)
	}
}

func TestBothNewRoutesRequireTheToken(t *testing.T) {
	srv, _ := feedServer(t)
	c := postClip(t, srv, "secret clip", "mcp")
	for _, path := range []string{"/v1/clips?after=0&wait=1", "/v1/clips/" + strconv.FormatInt(c.ID, 10)} {
		code, raw := do(t, srv, "GET", path, "", false)
		if code != http.StatusUnauthorized || bytes.Contains(raw, []byte("secret clip")) {
			t.Errorf("GET %s without a token: %d %s, want 401 and no body", path, code, raw)
		}
	}
}

func TestADedupBumpDoesNotReappearInTheFeed(t *testing.T) {
	// CHARACTERIZATION, documented in README.md: the cursor is the id, and a
	// re-capture of an identical body bumps the EXISTING row rather than
	// creating one, so it is not "after" a cursor that already passed it.
	srv, _ := feedServer(t)
	a := postClip(t, srv, "same text", "mcp")
	postClip(t, srv, "other", "phone")
	_, fb, _ := feed(t, srv, "after=0")
	cursor := (*fb.Clips)[len(*fb.Clips)-1].ID
	again := postClip(t, srv, "same text", "mcp")
	if again.ID != a.ID {
		t.Fatalf("dedup bump changed the id: %d -> %d", a.ID, again.ID)
	}
	_, fb, _ = feed(t, srv, "after="+strconv.FormatInt(cursor, 10))
	if len(*fb.Clips) != 0 {
		t.Fatalf("a bump reappeared in the feed: %+v", *fb.Clips)
	}
}

func TestTheNewestFirstListIsUnchanged(t *testing.T) {
	srv, _ := feedServer(t)
	a := postClip(t, srv, "older", "phone")
	b := postClip(t, srv, "newer", "phone")
	code, raw := do(t, srv, "GET", "/v1/clips", "", true)
	var lr listClipsResponse
	_ = json.Unmarshal(raw, &lr)
	if code != 200 || !equalIDs(ids(lr.Clips), []int64{b.ID, a.ID}) || lr.Limit != DefaultLimit {
		t.Fatalf("plain list: %d %s", code, raw)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
