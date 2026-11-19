package main

import (
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func TestTheToolLedgerIsExactlyTheseTools(t *testing.T) {
	// Pins the tool surface on both sides: a tool added (say, a delete) or
	// removed fails here, so a change to what an agent can do to the user's
	// clipboard is a visible, reviewed act.
	f := newFakeClip(testToken)
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))
	res, err := s.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil {
			t.Errorf("%s has no annotations", tl.Name)
		}
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "get_clip,push_clip,recent_clips"; got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}
	for _, tl := range res.Tools {
		ro := tl.Annotations.ReadOnlyHint
		if (tl.Name == "push_clip") == ro {
			t.Errorf("%s readOnlyHint = %v", tl.Name, ro)
		}
	}
}

func TestRecentClipsIsNewestFirstWithLimitAndQuery(t *testing.T) {
	f := newFakeClip(testToken)
	f.add("alpha one", "pixel")
	f.add("bravo two", "laptop")
	f.add("charlie alpha", "pixel")
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))

	res, _ := s.call(t, "recent_clips", map[string]any{})
	if res.IsError {
		t.Fatalf("error: %s", textOf(res))
	}
	got := structured[recentOut](t, res)
	if len(got.Clips) != 3 || got.Clips[0].Body != "charlie alpha" || got.Clips[2].Body != "alpha one" {
		t.Fatalf("clips = %+v", got.Clips)
	}
	if got.Clips[1].Device != "laptop" || got.Clips[0].CreatedAt != "2026-01-02T03:04:08Z" {
		t.Fatalf("fields not carried: %+v", got.Clips[1])
	}

	res, _ = s.call(t, "recent_clips", map[string]any{"limit": 1})
	if got := structured[recentOut](t, res); len(got.Clips) != 1 || got.Clips[0].Body != "charlie alpha" {
		t.Fatalf("limit 1 = %+v", got.Clips)
	}

	res, _ = s.call(t, "recent_clips", map[string]any{"query": "ALPHA"})
	got = structured[recentOut](t, res)
	if len(got.Clips) != 2 || got.Clips[0].Body != "charlie alpha" || got.Clips[1].Body != "alpha one" {
		t.Fatalf("query = %+v", got.Clips)
	}
}

func TestRecentClipsDefaultLimitIsTwentyNotTheServersHundred(t *testing.T) {
	f := newFakeClip(testToken)
	for i := range 37 {
		f.add(fmt.Sprintf("clip %d", i), "pixel")
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))
	res, _ := s.call(t, "recent_clips", nil)
	if got := structured[recentOut](t, res); len(got.Clips) != 20 {
		t.Fatalf("default returned %d clips, want 20", len(got.Clips))
	}
}

func TestEmptyStoreIsAnEmptyArrayNotNull(t *testing.T) {
	srv := httptest.NewServer(newFakeClip(testToken))
	defer srv.Close()
	s := startSession(t, envFor(srv))
	res, raw := s.call(t, "recent_clips", nil)
	if res.IsError || !strings.Contains(raw, `"clips":[]`) {
		t.Fatalf("empty store = %s", raw)
	}
}

func TestClipServerErrorsAreToolErrorsWithTheirCode(t *testing.T) {
	f := newFakeClip(testToken)
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))

	res, _ := s.call(t, "recent_clips", map[string]any{"limit": 501})
	if !res.IsError || !strings.Contains(textOf(res), "400 invalid_limit") {
		t.Fatalf("limit 501 = %v %s", res.IsError, textOf(res))
	}

	f.mu.Lock()
	f.listStatus, f.listBody = 503, `{"error":"store_unreadable","detail":"the clip store could not be read"}`
	f.mu.Unlock()
	res, _ = s.call(t, "recent_clips", nil)
	if !res.IsError || !strings.Contains(textOf(res), "503 store_unreadable") {
		t.Fatalf("503 = %v %s", res.IsError, textOf(res))
	}

	// A 200 that is not the clip server's shape must not read as an empty list.
	f.mu.Lock()
	f.listStatus, f.listBody = 200, `{"status":"ok"}`
	f.mu.Unlock()
	res, _ = s.call(t, "recent_clips", nil)
	if !res.IsError || !strings.Contains(textOf(res), `without a "clips" array`) {
		t.Fatalf("shapeless 200 = %v %s", res.IsError, textOf(res))
	}
}

func TestAWrongTokenIsA401ToolErrorNotAnEmptyList(t *testing.T) {
	f := newFakeClip("a-different-token-the-server-holds-000")
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))
	res, _ := s.call(t, "recent_clips", nil)
	if !res.IsError || !strings.Contains(textOf(res), "401 unauthorized") {
		t.Fatalf("wrong token = %v %s", res.IsError, textOf(res))
	}
}

func TestGetClipFindsByIDAndDistinguishesAbsentFromOutOfReach(t *testing.T) {
	f := newFakeClip(testToken)
	a := f.add("first", "pixel")
	f.add("second", "laptop")
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))

	res, _ := s.call(t, "get_clip", map[string]any{"id": a.ID})
	if got := structured[clipOut](t, res); res.IsError || got.Body != "first" || got.ID != a.ID {
		t.Fatalf("get = %s", textOf(res))
	}

	res, _ = s.call(t, "get_clip", map[string]any{"id": 999})
	if !res.IsError || !strings.Contains(textOf(res), "no clip with id 999") {
		t.Fatalf("absent = %s", textOf(res))
	}

	res, _ = s.call(t, "get_clip", map[string]any{"id": 0})
	if !res.IsError || !strings.Contains(textOf(res), "positive integer") {
		t.Fatalf("id 0 = %s", textOf(res))
	}

	// 503 more clips push `a` out of the 500-row window. It still EXISTS in the
	// store, so the answer must say "out of reach", never "no clip".
	for i := range 503 {
		f.add(fmt.Sprintf("filler %d", i), "pixel")
	}
	res, _ = s.call(t, "get_clip", map[string]any{"id": a.ID})
	if !res.IsError || !strings.Contains(textOf(res), "not among the 500 newest") {
		t.Fatalf("out of window = %s", textOf(res))
	}
}

func TestPushClipStoresWithTheConfiguredDeviceAndReportsDedup(t *testing.T) {
	f := newFakeClip(testToken)
	srv := httptest.NewServer(f)
	defer srv.Close()
	env := envFor(srv)
	env[envDevice] = "agent-box"
	s := startSession(t, env)

	res, _ := s.call(t, "push_clip", map[string]any{"text": "kubectl get pods"})
	got := structured[pushOut](t, res)
	if res.IsError || got.ID == 0 || got.Deduped || !strings.Contains(got.Note, "NOT written to the phone's system clipboard") {
		t.Fatalf("push = %s", textOf(res))
	}
	f.mu.Lock()
	stored := f.clips[0]
	f.mu.Unlock()
	if stored.Body != "kubectl get pods" || stored.Device != "agent-box" {
		t.Fatalf("stored = %+v", stored)
	}

	res, _ = s.call(t, "push_clip", map[string]any{"text": "kubectl get pods"})
	if again := structured[pushOut](t, res); !again.Deduped || again.ID != got.ID {
		t.Fatalf("second push = %+v", again)
	}

	res, _ = s.call(t, "push_clip", map[string]any{"text": ""})
	if !res.IsError {
		t.Fatalf("empty push accepted")
	}
}

func TestDefaultDeviceIsMCP(t *testing.T) {
	f := newFakeClip(testToken)
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := startSession(t, envFor(srv))
	s.call(t, "push_clip", map[string]any{"text": "hello"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clips[0].Device != "mcp" {
		t.Fatalf("device = %q", f.clips[0].Device)
	}
}
