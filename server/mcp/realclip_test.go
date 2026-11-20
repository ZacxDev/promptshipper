package main

// The SEAM test: the same tool calls against the REAL clipstore binary rather
// than the in-process fake, so a drift between harness_test.go's fake and
// server/clip's actual contract (field names, dedup status, the limit cap, the
// query semantics) fails here instead of shipping.
//
// It needs a built clipstore: CLIPSTORE_BIN=/path/to/clipstore. Without it the
// test SKIPS locally — but CI sets REQUIRE_REAL_CLIP=1, which turns the skip
// into a failure, so the CI run cannot go green without having executed it.

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startRealClip(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("CLIPSTORE_BIN")
	if bin == "" {
		if os.Getenv("REQUIRE_REAL_CLIP") != "" {
			t.Fatal("REQUIRE_REAL_CLIP is set but CLIPSTORE_BIN is not")
		}
		t.Skip("CLIPSTORE_BIN not set; the real-clipstore seam test runs in CI")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cmd := exec.Command(bin, "-addr", addr, "-db", filepath.Join(t.TempDir(), "c.db"))
	cmd.Env = append(os.Environ(), "CLIPSTORE_TOKEN="+testToken)
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var logs syncBuf
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			_, _ = logs.Write([]byte(sc.Text() + "\n"))
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	base := "http://" + addr
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return base
			}
		}
	}
	t.Fatalf("clipstore did not become healthy: %s", logs.String())
	return ""
}

func TestAgainstTheRealClipstore(t *testing.T) {
	base := startRealClip(t)
	s := startSession(t, map[string]string{envClipURL: base, envClipToken: testToken})

	res, _ := s.call(t, "recent_clips", nil)
	if got := structured[recentOut](t, res); res.IsError || len(got.Clips) != 0 {
		t.Fatalf("empty store: %s", textOf(res))
	}

	var first pushOut
	for i, body := range []string{"the password is in the vault", "second clip", "the password is in the vault"} {
		res, _ := s.call(t, "push_clip", map[string]any{"text": body})
		p := structured[pushOut](t, res)
		if res.IsError || p.ID == 0 {
			t.Fatalf("push %d: %s", i, textOf(res))
		}
		if i == 0 {
			first = p
		}
		// created_at is in milliseconds and ties break on id DESC, so a re-push
		// in the same millisecond as the previous push would NOT move to the
		// front. Measured: without this sleep the ordering assertion below
		// flaked. A human never captures twice in one millisecond.
		time.Sleep(5 * time.Millisecond)
		if i == 2 && (!p.Deduped || p.ID != first.ID) {
			t.Fatalf("re-push of an identical body should dedup to id %d: %+v", first.ID, p)
		}
	}

	res, _ = s.call(t, "recent_clips", map[string]any{"query": "assw"})
	got := structured[recentOut](t, res)
	if len(got.Clips) != 1 || got.Clips[0].Body != "the password is in the vault" ||
		got.Clips[0].Device != "mcp" || got.Clips[0].BumpCount != 1 {
		t.Fatalf("local substring query over the real store's rows: %+v", got.Clips)
	}

	res, _ = s.call(t, "recent_clips", nil)
	if got := structured[recentOut](t, res); len(got.Clips) != 2 || got.Clips[0].ID != first.ID {
		t.Fatalf("dedup bump should move the clip to the front: %+v", got.Clips)
	}

	// The search window is the REAL server's cap: asking it for 500 must be
	// accepted (a 400 invalid_limit here means searchWindow drifted above it).
	if got := structured[recentOut](t, res); got.Searched != 2 {
		t.Fatalf("searched = %d, want 2 (every stored clip)", got.Searched)
	}
	if strings.Contains(s.rawOut.String()+s.rawErr.String(), testToken) {
		t.Fatal("token in output")
	}
}
