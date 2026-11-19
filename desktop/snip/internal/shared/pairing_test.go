package shared

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🔴 NO TEST HERE OPENS THE OPERATOR'S LIVE CONFIG: every case points
// $SNIP_PAIRING / $SNIP_SHARED_ENV / $XDG_CONFIG_HOME at a temp dir, and every
// URL and token is synthetic. The two tokens are pairwise distinct so a test
// can tell which one went on the wire.
var (
	clipTok = strings.Repeat("c1", 32)
	sttTok  = strings.Repeat("57", 32)
)

// payload renders a v1 payload; mutate edits the decoded map first.
func payload(t *testing.T, clipURL string, mutate func(map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"v":    1,
		"type": "promptshipper-pairing",
		"name": "test-box",
		"stt":  map[string]any{"url": "http://192.0.2.7:8118", "token": sttTok},
		"clip": map[string]any{"url": clipURL, "token": clipTok},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SNIP_PAIRING", "")
	t.Setenv("SNIP_SHARED_ENV", "")
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "snip"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A valid payload yields the CLIP endpoint (never stt's), from raw JSON and
// from the `PAIRING_PAYLOAD=` line ./setup prints, with unknown fields at
// every level ignored (the v1 compatibility rule).
func TestParsePairingTakesTheClipEndpoint(t *testing.T) {
	withExtras := payload(t, "http://192.0.2.7:8139", func(m map[string]any) {
		m["mcp"] = map[string]any{"url": "http://192.0.2.7:9000"}
		m["clip"].(map[string]any)["future"] = true
	})
	for name, in := range map[string][]byte{
		"raw json":     withExtras,
		"setup line":   append([]byte("PAIRING_PAYLOAD="), withExtras...),
		"padded":       []byte("\n  " + string(withExtras) + "\n"),
		"no name":      payload(t, "http://192.0.2.7:8139", func(m map[string]any) { delete(m, "name") }),
		"ipv6 + slash": payload(t, "http://[fd7a:115c:a1e0::1]:8139/", nil),
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParsePairing(in, "/p/pairing.json")
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if cfg.Token != clipTok {
				t.Errorf("token is not clip.token (stt's leaked in?)")
			}
			if !strings.Contains(cfg.URL, ":8139") {
				t.Errorf("URL = %q, want the clip URL", cfg.URL)
			}
			if cfg.TokenName != "clip.token in /p/pairing.json" {
				t.Errorf("TokenName = %q", cfg.TokenName)
			}
		})
	}
}

// Every refusal in docs/pairing.md, with LITERAL expected text, and no
// message may carry either token.
func TestParsePairingRefuses(t *testing.T) {
	const ok = "http://192.0.2.7:8139"
	setClip := func(k string, v any) func(map[string]any) {
		return func(m map[string]any) { m["clip"].(map[string]any)[k] = v }
	}
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"not json", []byte("{" + clipTok), "not a pairing payload"},
		{"wrong type", payload(t, ok, func(m map[string]any) { m["type"] = "wifi" }), `type must be "promptshipper-pairing"`},
		{"no type", payload(t, ok, func(m map[string]any) { delete(m, "type") }), `type must be "promptshipper-pairing"`},
		{"v2", payload(t, ok, func(m map[string]any) { m["v"] = 2 }), "version 2; this snip understands version 1 only — update snip"},
		{"v0", payload(t, ok, func(m map[string]any) { m["v"] = 0 }), "version 0; this snip understands version 1 only"},
		{"no v", payload(t, ok, func(m map[string]any) { delete(m, "v") }), `no version field "v"`},
		{"no stt", payload(t, ok, func(m map[string]any) { delete(m, "stt") }), `no "stt" object — refusing to pair half a server`},
		{"no clip", payload(t, ok, func(m map[string]any) { delete(m, "clip") }), `no "clip" object`},
		{"no clip url", payload(t, ok, setClip("url", "")), "clip.url is missing"},
		{"no clip token", payload(t, ok, setClip("token", nil)), "clip.token is missing"},
		{"ftp", payload(t, "ftp://192.0.2.7:8139", nil), "must be http:// or https://"},
		{"path", payload(t, "http://192.0.2.7:8139/clip", nil), "must not have a path"},
		{"no port", payload(t, "http://192.0.2.7", nil), "has no port"},
		{"query", payload(t, "http://192.0.2.7:8139?x=1", nil), "must not have a query"},
		{"fragment", payload(t, "http://192.0.2.7:8139#x", nil), "must not have a fragment"},
		{"userinfo", payload(t, "http://u:p@192.0.2.7:8139", nil), "must not carry credentials"},
		{"short token", payload(t, ok, setClip("token", strings.Repeat("a", 23))), "clip.token is shorter than 24 characters"},
		{"bad char", payload(t, ok, setClip("token", clipTok+"+")), "clip.token has a character outside [A-Za-z0-9._~-]"},
		{"bad stt url", payload(t, ok, func(m map[string]any) { m["stt"].(map[string]any)["url"] = "http://x:1/v1" }), "stt.url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ParsePairing(c.in, "/p/pairing.json")
			if err == nil {
				t.Fatalf("accepted: %+v", cfg)
			}
			msg := err.Error()
			if !strings.Contains(msg, c.want) {
				t.Errorf("message = %q, want it to contain %q", msg, c.want)
			}
			if !strings.Contains(msg, "/p/pairing.json") {
				t.Errorf("message %q does not name the file", msg)
			}
			if strings.Contains(msg, clipTok) || strings.Contains(msg, sttTok) {
				t.Errorf("message carries a token: %q", msg)
			}
		})
	}
	// POSITIVE CONTROL: exactly 24 allowed characters passes, so the floor is
	// 24 and not "anything long".
	edge := payload(t, ok, setClip("token", strings.Repeat("aZ9._~-x", 3)))
	if _, err := ParsePairing(edge, "/p"); err != nil {
		t.Fatalf("a 24-character token from the allowed set was refused: %v", err)
	}
}

// Resolution: overrides beat defaults; two sources at one level is a named
// failure; neither names both files and both keys.
func TestLoadResolution(t *testing.T) {
	envBody := []byte("SNIP_SHARED_URL=http://192.0.2.9:1\nSNIP_SHARED_TOKEN=envtoken-abcdefgh\n")

	t.Run("default pairing only", func(t *testing.T) {
		dir := isolate(t)
		write(t, filepath.Join(dir, "snip", "pairing.json"), payload(t, "http://192.0.2.7:8139", nil))
		cfg, err := Load()
		if err != nil || cfg.Token != clipTok {
			t.Fatalf("Load = %+v, %v", cfg.URL, err)
		}
	})
	t.Run("default env only", func(t *testing.T) {
		dir := isolate(t)
		write(t, filepath.Join(dir, "snip", "shared.env"), envBody)
		cfg, err := Load()
		if err != nil || cfg.Token != "envtoken-abcdefgh" {
			t.Fatalf("Load = %+v, %v", cfg.URL, err)
		}
	})
	t.Run("both defaults", func(t *testing.T) {
		dir := isolate(t)
		pp := filepath.Join(dir, "snip", "pairing.json")
		ep := filepath.Join(dir, "snip", "shared.env")
		write(t, pp, payload(t, "http://192.0.2.7:8139", nil))
		write(t, ep, envBody)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "both "+pp+" and "+ep+" exist — remove one") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("both overrides", func(t *testing.T) {
		isolate(t)
		t.Setenv("SNIP_PAIRING", "/a/pairing.json")
		t.Setenv("SNIP_SHARED_ENV", "/b/shared.env")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "both SNIP_PAIRING (/a/pairing.json) and SNIP_SHARED_ENV (/b/shared.env) are set") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("override beats a default file", func(t *testing.T) {
		dir := isolate(t)
		write(t, filepath.Join(dir, "snip", "pairing.json"), payload(t, "http://192.0.2.7:8139", nil))
		ep := filepath.Join(t.TempDir(), "shared.env")
		write(t, ep, envBody)
		t.Setenv("SNIP_SHARED_ENV", ep)
		cfg, err := Load()
		if err != nil || cfg.Token != "envtoken-abcdefgh" {
			t.Fatalf("Load = %+v, %v", cfg.URL, err)
		}
	})
	t.Run("missing override pairing file", func(t *testing.T) {
		isolate(t)
		p := filepath.Join(t.TempDir(), "absent.json")
		t.Setenv("SNIP_PAIRING", p)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "no "+p+" — save the pairing payload from `./setup --pairing` into it (mode 0600)") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("neither", func(t *testing.T) {
		dir := isolate(t)
		_, err := Load()
		if err == nil {
			t.Fatal("no config must be an error")
		}
		for _, want := range []string{filepath.Join(dir, "snip", "pairing.json"),
			filepath.Join(dir, "snip", "shared.env"), KeyURL, KeyToken, "0600", "./setup --pairing"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message %q does not name %q", err, want)
			}
		}
	})
}

// END TO END against a fake clip server: a pairing file is the only config,
// and the client sends clip.token (not stt.token) to /v1/clips, reads the
// envelope, and on a 401 names the pairing file's clip.token.
func TestAPairedClientTalksToTheClipServer(t *testing.T) {
	var gotAuth, gotPath, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotLimit = r.Header.Get("Authorization"), r.URL.Path, r.URL.Query().Get("limit")
		if gotAuth != "Bearer "+clipTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"clips":[{"id":7,"body":"synthetic clip","device":"phone","created_at":1700000000000,"bump_count":0}],"limit":100,"query":""}`))
	}))
	defer srv.Close()

	dir := isolate(t)
	pp := filepath.Join(dir, "snip", "pairing.json")
	write(t, pp, append([]byte("PAIRING_PAYLOAD="), payload(t, srv.URL, nil)...))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows, err := New(cfg).Rows(0)
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if gotPath != "/v1/clips" || gotLimit != "100" {
		t.Errorf("request = %s?limit=%s", gotPath, gotLimit)
	}
	if len(rows) != 1 || rows[0].ID != 7 || rows[0].Body != "synthetic clip" {
		t.Errorf("rows = %+v", rows)
	}

	// The fake server refuses anything but clip.token: pair with a payload
	// whose clip.token is wrong and the 401 must name where the token came from.
	write(t, pp, payload(t, srv.URL, func(m map[string]any) {
		m["clip"].(map[string]any)["token"] = sttTok
	}))
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(cfg).Rows(0)
	if err == nil || err.Error() != "clip store refused (401): clip.token in "+pp+" is not accepted" {
		t.Fatalf("err = %v", err)
	}
}
