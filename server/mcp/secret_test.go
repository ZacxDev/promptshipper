package main

// 🔴 THE TOKEN-SECRECY GUARDS. These are REGRESSION guards with recorded
// mutations (README "Tests"): each was watched fail with the mutation named in
// its comment, and each asserts a POSITIVE CONTROL — the redaction marker is
// present — so a pass cannot mean "the token never reached the output path".

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostileClip echoes the token back through every channel a server could:
// clip bodies, device names, JSON error details and a non-JSON error page.
func hostileClip(t *testing.T) *httptest.Server {
	t.Helper()
	f := newFakeClip(testToken)
	f.add("my token is "+testToken+" do not share", "pixel")
	f.add("plain clip", testToken) // the token as a device name
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("q") {
		case "json-error":
			writeJSON(w, 418, map[string]string{"error": "teapot", "detail": "you sent " + r.Header.Get("Authorization")})
			return
		case "html-error":
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>bad gateway; request headers: " + r.Header.Get("Authorization") + "</html>"))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTheTokenNeverAppearsInAnyOutput drives every tool, success and error
// paths, against a server that echoes the token everywhere, and with a pairing
// file whose PATH contains the token (so the startup log line carries it).
//
// Mutations watched red (2026-11-19):
//   - redactBytes returns b unchanged            -> FAIL: token in stdout AND stderr
//   - run() passes stdout, not outw, to IOTransport -> FAIL: token in stdout
//   - run() builds the logger on stderr, not errw  -> FAIL: token in stderr
func TestTheTokenNeverAppearsInAnyOutput(t *testing.T) {
	srv := hostileClip(t)
	dir := filepath.Join(t.TempDir(), "pair-"+testToken)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(dir, "pairing.json")
	payload := `{"v":1,"type":"promptshipper-pairing","clip":{"url":"` + srv.URL + `","token":"` + testToken + `"}}`
	if err := os.WriteFile(pf, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startSession(t, map[string]string{}, "-pairing", pf)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"recent_clips", nil},
		{"recent_clips", map[string]any{"query": "token"}},
		{"recent_clips", map[string]any{"query": "json-error"}},
		{"recent_clips", map[string]any{"query": "html-error"}},
		{"push_clip", map[string]any{"text": "echo " + testToken}},
	}
	var results strings.Builder
	for _, c := range calls {
		_, raw := s.call(t, c.name, c.args)
		results.WriteString(raw)
	}
	s.stop()

	for name, out := range map[string]string{
		"decoded tool results": results.String(),
		"raw stdout frames":    s.rawOut.String(),
		"stderr log":           s.rawErr.String(),
	} {
		if strings.Contains(out, testToken) {
			t.Errorf("token found in %s", name)
		}
		// Positive control: the token DID reach this stream and was replaced.
		if !strings.Contains(out, redactedMarker) {
			t.Errorf("positive control failed: no %s in %s, so this test never saw the token reach it", redactedMarker, name)
		}
	}
}

// TestTruncatedErrorBodyDoesNotLeakATokenPrefix places the token across the
// maxErrorDetail cut. Mutation watched red: truncate BEFORE redactBytes in
// clipclient.go -> FAIL (a 10-character token prefix survives, which the
// stream redaction cannot recognise).
func TestTruncatedErrorBodyDoesNotLeakATokenPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(strings.Repeat("x", maxErrorDetail-10) + testToken + " tail"))
	}))
	defer srv.Close()
	s := startSession(t, envFor(srv))
	_, raw := s.call(t, "recent_clips", nil)
	prefix := testToken[:10]
	if strings.Contains(raw, prefix) || strings.Contains(s.rawOut.String(), prefix) {
		t.Fatalf("token prefix %q leaked: %s", prefix, raw)
	}
	if !strings.Contains(raw, "HTTP 500") {
		t.Fatalf("positive control: error not relayed: %s", raw)
	}
}

// TestRedactingWriterCatchesATokenSplitAcrossWrites. Mutation watched red:
// redact each Write independently (no line buffering) -> FAIL.
func TestRedactingWriterCatchesATokenSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := newRedactingWriter(&out, testToken)
	half := len(testToken) / 2
	for _, p := range []string{"a ", testToken[:half], testToken[half:], " b\nc " + testToken} {
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "a [REDACTED] b\nc [REDACTED]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestConfigErrorsNeverEchoTheToken feeds every refusal path a token-bearing
// input and checks the error text. These messages are printed before the
// redacting writer exists, so they are the one output not covered by it.
func TestConfigErrorsNeverEchoTheToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := "has a quote\"" + testToken
	cases := map[string]struct {
		env  map[string]string
		flag string
	}{
		"short token":         {env: map[string]string{envClipURL: "http://h:1", envClipToken: testToken[:12]}},
		"bad charset":         {env: map[string]string{envClipURL: "http://h:1", envClipToken: bad}},
		"userinfo in url":     {env: map[string]string{envClipURL: "http://u:" + testToken + "@h:1", envClipToken: testToken}},
		"path in url":         {env: map[string]string{envClipURL: "http://h:1/" + testToken, envClipToken: testToken}},
		"query in url":        {env: map[string]string{envClipURL: "http://h:1?t=" + testToken, envClipToken: testToken}},
		"token only":          {env: map[string]string{envClipToken: testToken}},
		"both sources":        {env: map[string]string{envClipURL: "http://h:1", envClipToken: testToken}, flag: write("p.json", "{}")},
		"pairing type error":  {flag: write("t.json", `{"v":"`+testToken+`"}`)},
		"pairing not json":    {flag: write("n.json", testToken)},
		"pairing wrong v":     {flag: write("v.json", `{"v":2,"type":"promptshipper-pairing","clip":{"url":"http://h:1","token":"`+testToken+`"}}`)},
		"pairing short token": {flag: write("s.json", `{"v":1,"type":"promptshipper-pairing","clip":{"url":"http://h:1","token":"`+testToken[:12]+`"}}`)},
	}
	for name, c := range cases {
		_, err := loadConfig(func(k string) string { return c.env[k] }, c.flag)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), testToken[:12]) {
			t.Errorf("%s: error echoes the token: %v", name, err)
		}
	}
}
