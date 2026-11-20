package main

// 🔴 THE SEARCH-ORACLE GUARDS (round-1 audit finding on PR #3).
//
// The attack: the clip server's search is a case-insensitive substring match,
// so if the agent's `query` reaches it, "does any clip contain X?" is a yes/no
// answer the agent can read off the result list EVEN WHEN every body it sees is
// redacted. With a clip `my clip token <token> keep safe`, extending a query one
// hex character at a time recovered a 64-hex token exactly in 575 calls. Output
// redaction cannot stop it, because nothing secret is ever OUTPUT.
//
// Both tests here were run RED against 3e5ef8d (the code before the fix), which
// sent the query to the clip server as `q=`.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Two setup-shaped tokens (64 lowercase hex, like ./setup generates) that
// differ at every position, so any substring of one is (almost surely) not a
// substring of the other at the same offset.
const (
	oracleTokenA = "3f9a0c71e4b28d56a7c3e19f0b6d4285c1e7a39f50d2b8e64a1c7f3092be5d81"
	oracleTokenB = "c4105e98d2a7f3c0b915d6e8a4f2097d6b3c8e1f74a9d05b2e8f61c473a09e2f"
)

func oracleFixture(token string) *fakeClip {
	f := newFakeClip(token)
	f.add("deploy notes for friday", "laptop")
	f.add("my clip token "+token+" keep safe", "pixel")
	f.add("buy milk and 2 eggs", "pixel")
	return f
}

// oracleQueries is every query an attacker would build from the token: each
// prefix (the attack as demonstrated), every 1-, 2-, 3- and 8-character window
// (both of the server's search paths: short tokens take the LIKE scan, >= 3
// take the trigram index), upper-cased variants, and multi-word queries that
// pair a token fragment with words from the clip around it.
func oracleQueries(token string) []string {
	seen := map[string]bool{}
	var qs []string
	add := func(q string) {
		if !seen[q] {
			seen[q] = true
			qs = append(qs, q)
		}
	}
	for i := 1; i <= len(token); i++ {
		add(token[:i])
	}
	for _, w := range []int{1, 2, 3, 8} {
		for i := 0; i+w <= len(token); i++ {
			add(token[i : i+w])
		}
	}
	add(strings.ToUpper(token[:12]))
	add("token " + token[:5])
	add(token[:5] + " keep")
	add("my clip token " + token[:20])
	return qs
}

// TestSearchIsNotATokenOracle: two stores identical except for the token value
// inside one clip. Every token-derived query must produce byte-identical
// results from both — i.e. NOTHING the agent can observe depends on the token.
// (Comparing against a "query that cannot match the token" within ONE store is
// the same claim, but the fixture's own words share letters with hex digits, so
// the two-store form is the one that isolates the token as the only variable.)
func TestSearchIsNotATokenOracle(t *testing.T) {
	results := func(token string, queries []string) []string {
		srv := httptest.NewServer(oracleFixture(token))
		defer srv.Close()
		s := startSession(t, map[string]string{envClipURL: srv.URL, envClipToken: token})
		defer s.stop()
		out := make([]string, len(queries))
		for i, q := range queries {
			res, _ := s.call(t, "recent_clips", map[string]any{"query": q, "limit": 50})
			b, _ := json.Marshal(res.StructuredContent)
			out[i] = string(b)
			if res.IsError {
				out[i] += " ERROR " + textOf(res)
			}
		}
		return out
	}
	const control = "keep safe"
	queries := append(oracleQueries(oracleTokenA), oracleQueries(oracleTokenB)...)
	queries = append(queries, control)
	a := results(oracleTokenA, queries)
	b := results(oracleTokenB, queries)
	differ := 0
	for i, q := range queries {
		if a[i] != b[i] {
			differ++
			if differ <= 3 {
				t.Errorf("query %q is a token oracle: store A -> %s ; store B -> %s", q, a[i], b[i])
			}
		}
	}
	if differ > 0 {
		t.Fatalf("%d of %d token-derived queries gave an observable difference", differ, len(queries))
	}
	// Positive control: a plain-word query DOES find the token-bearing clip
	// (shown redacted), so a token query finding it would have been observable.
	last := a[len(a)-1]
	if !strings.Contains(last, "my clip token") || !strings.Contains(last, redactedMarker) {
		t.Fatalf("positive control: %q did not return the redacted token clip: %s", control, last)
	}
}

// recordingTransport captures every request the process makes to the clip
// server, at the RoundTripper — below every layer of this program's code.
type recordingTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	reqs []string // method + full URL + body
	urls []*url.URL
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Method+" "+req.URL.String()+" "+string(body))
	u := *req.URL
	r.urls = append(r.urls, &u)
	r.mu.Unlock()
	return r.next.RoundTrip(req)
}

// TestTheQueryNeverLeavesTheProcess: no request to the clip server carries
// the agent's query, in any form. Asserted two ways: structurally (a GET has
// no parameter but `limit`), and by content (a distinctive query string is in
// no request's URL or body, raw or decoded).
func TestTheQueryNeverLeavesTheProcess(t *testing.T) {
	rec := &recordingTransport{next: http.DefaultTransport}
	orig := http.DefaultTransport
	http.DefaultTransport = rec
	t.Cleanup(func() { http.DefaultTransport = orig })

	srv := httptest.NewServer(oracleFixture(oracleTokenA))
	defer srv.Close()
	s := startSession(t, map[string]string{envClipURL: srv.URL, envClipToken: oracleTokenA})
	const sentinel = "zq-sentinel-query-7781"
	queries := []string{sentinel, oracleTokenA[:9], "keep " + oracleTokenA[:4]}
	for _, q := range queries {
		s.call(t, "recent_clips", map[string]any{"query": q})
	}
	s.stop()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) == 0 {
		t.Fatal("positive control: the RoundTripper recorded no requests, so it observed nothing")
	}
	for i, u := range rec.urls {
		for k := range u.Query() {
			if k != "limit" {
				t.Errorf("request %d carries parameter %q: %s", i, k, rec.reqs[i])
			}
		}
	}
	for _, r := range rec.reqs {
		dec, _ := url.QueryUnescape(r)
		for _, q := range queries {
			if strings.Contains(r, q) || strings.Contains(dec, q) {
				t.Errorf("query %q left the process: %s", q, r)
			}
		}
	}
}
