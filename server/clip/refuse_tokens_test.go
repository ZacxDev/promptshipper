package main

// CLIPSTORE_REFUSE_TOKENS: the clip server refuses to store OTHER credentials
// it is told about — in a default install, the STT server's token — exactly as
// it refuses its own (token_guard_test.go). Before this existed, a copied STT
// token was stored like any other text (the README recorded it as open).

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sttToken is distinct from feedToken and from every other fixture token.
const sttToken = "stt-refused-token-fedcba9876543210"

func refuseServer(t *testing.T, refuse ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewAPI(newTestStore(t), feedToken, frozenClock, refuse...).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestAClipContainingARefusedTokenIsRefusedAndNotStored(t *testing.T) {
	srv := refuseServer(t, sttToken)
	for _, tc := range []struct{ name, body, device string }{
		{"the STT token alone", sttToken, "phone"},
		{"the STT token inside other text", "stt token: " + sttToken + " ok", "phone"},
		{"the STT token as the device", "harmless", sttToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"body": tc.body, "device": tc.device})
			code, raw := do(t, srv, "POST", "/v1/clips", string(b), true)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("POST of a clip containing a refused token answered %d %s, want 422", code, raw)
			}
			if !bytes.Contains(raw, []byte(`"contains_server_token"`)) {
				t.Fatalf("422 without the stable code: %s", raw)
			}
			if bytes.Contains(raw, []byte(sttToken)) {
				t.Fatalf("the refusal echoed the token: %s", raw)
			}
		})
	}
	code, raw := do(t, srv, "GET", "/v1/clips", "", true)
	if code != 200 || !bytes.Contains(raw, []byte(`"clips":[]`)) {
		t.Fatalf("something was stored after the refusals: %d %s", code, raw)
	}
	// The server's OWN token is still refused alongside the extra one.
	b, _ := json.Marshal(map[string]string{"body": feedToken, "device": "phone"})
	if code, raw := do(t, srv, "POST", "/v1/clips", string(b), true); code != http.StatusUnprocessableEntity {
		t.Fatalf("own token no longer refused with an extra configured: %d %s", code, raw)
	}
}

// Positive control: the SAME body is stored when nothing is configured, so the
// 422 above is caused by the refuse list and not by the fixture.
func TestWithoutARefuseListTheSameValueIsStored(t *testing.T) {
	srv := refuseServer(t)
	if c := postClip(t, srv, sttToken, "phone"); c.Body != sttToken {
		t.Fatalf("not stored as-is without a refuse list: %+v", c)
	}
}

func TestRefusedTokensParsing(t *testing.T) {
	long2 := "second-refused-token-0123456789ab"
	var warn bytes.Buffer
	got := refusedTokens(" "+sttToken+" ,, "+long2+",short-one,", &warn)
	if strings.Join(got, "|") != sttToken+"|"+long2 {
		t.Fatalf("parsed %q", got)
	}
	w := warn.String()
	if !strings.Contains(w, "CLIPSTORE_REFUSE_TOKENS entry 4 is 9 bytes") {
		t.Fatalf("short entry not reported by position and length: %q", w)
	}
	if strings.Contains(w, "short-one") {
		t.Fatalf("the warning printed the value: %q", w)
	}
	// Unset/blank — compose's ${STT_API_TOKEN:-} with no STT token — is "none", silently.
	var quiet bytes.Buffer
	for _, raw := range []string{"", "  ", ","} {
		if got := refusedTokens(raw, &quiet); len(got) != 0 {
			t.Fatalf("%q parsed to %q", raw, got)
		}
	}
	if quiet.Len() != 0 {
		t.Fatalf("a blank list warned: %q", quiet.String())
	}
}

func TestTheStartupScanCountsRefusedTokenClipsByPosition(t *testing.T) {
	s := newTestStore(t)
	seedRows(t, s,
		[2]string{"pasted " + sttToken, "phone"},
		[2]string{"nothing to see", "laptop"})
	var out bytes.Buffer
	reportRefusedTokenClips(context.Background(), s, []string{"never-stored-token-000000000000", sttToken}, &out)
	msg := out.String()
	if !strings.Contains(msg, "1 stored clip(s) contain the credential in CLIPSTORE_REFUSE_TOKENS entry 2") {
		t.Fatalf("scan did not report entry 2 once: %q", msg)
	}
	if strings.Contains(msg, "entry 1") || strings.Contains(msg, sttToken) || strings.Contains(msg, "pasted") {
		t.Fatalf("scan reported a clean entry or printed content: %q", msg)
	}
}

// 🔴 THE SEAM: main() must read CLIPSTORE_REFUSE_TOKENS and hand it to the API.
// The unit tests above construct the API directly and cannot see main forget.
func TestMainRefusesAClipContainingATokenFromTheEnvironment(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-addr", addr, "-db", filepath.Join(t.TempDir(), "c.db"))
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, tokenEnv+"=") || strings.HasPrefix(kv, reexecEnv+"=") ||
			strings.HasPrefix(kv, refuseEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, reexecEnv+"=1", tokenEnv+"="+feedToken, refuseEnv+"="+sttToken)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	base := "http://" + addr
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := http.Get(base + "/healthz")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("clipstore never became healthy: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	post := func(body string) int {
		b, _ := json.Marshal(map[string]string{"body": body, "device": "phone"})
		req, _ := http.NewRequest("POST", base+"/v1/clips", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+feedToken)
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := post("copied: " + sttToken); code != http.StatusUnprocessableEntity {
		t.Fatalf("main() stored a clip containing the CLIPSTORE_REFUSE_TOKENS value: %d\n%s", code, stderr.String())
	}
	// Positive control through the same process: an ordinary clip IS stored.
	if code := post("an ordinary clip"); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("an ordinary clip was not stored: %d\n%s", code, stderr.String())
	}
}

// 🔴 THE SEAM, startup half: main() must RUN the refused-token scan, not just
// pass the list to the API. TestTheStartupScanCountsRefusedTokenClipsByPosition
// calls reportRefusedTokenClips directly and cannot see main stop calling it.
// The scan runs before the listener opens, so once /healthz answers, its line
// is already on stderr.
func TestMainRunsTheStartupRefusedTokenScan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "c.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seedRows(t, s, [2]string{"pasted " + sttToken, "phone"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-addr", addr, "-db", dbPath)
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, tokenEnv+"=") || strings.HasPrefix(kv, reexecEnv+"=") ||
			strings.HasPrefix(kv, refuseEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, reexecEnv+"=1", tokenEnv+"="+feedToken, refuseEnv+"="+sttToken)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait() // joins the stderr copier, so the buffer is safe to read
		}
	}
	t.Cleanup(stop)

	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("clipstore never became healthy: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	msg := stderr.String()
	if !strings.Contains(msg, "1 stored clip(s) contain the credential in CLIPSTORE_REFUSE_TOKENS entry 1") {
		t.Fatalf("main() did not run the startup refused-token scan:\n%s", msg)
	}
	if strings.Contains(msg, sttToken) || strings.Contains(msg, "pasted") {
		t.Fatalf("the startup scan printed the token or the clip:\n%s", msg)
	}
}
