package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{envClipURL: "http://100.101.102.103:8139/", envClipToken: testToken}
	cfg, err := loadConfig(func(k string) string { return env[k] }, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClipURL != "http://100.101.102.103:8139" || cfg.Token != testToken {
		t.Fatalf("cfg = %+v", cfg)
	}
	if strings.Contains(cfg.String(), testToken) {
		t.Fatalf("String() includes the token")
	}
}

func TestConfigFromAPairingV1File(t *testing.T) {
	// The v1 shape from docs/pairing.md WITH the stt half (the phone form) and
	// WITHOUT it (the desktop form), plus an unknown field. stt is never
	// required and never read; the clip half is all this server uses.
	dir := t.TempDir()
	clipHalf := `"clip":{"url":"http://[fd7a:115c:a1e0::1]:8139","token":"` + testToken + `"}`
	bodies := map[string]string{
		"with stt": `{"v":1,"type":"promptshipper-pairing","name":"box","future":{"x":1},` +
			`"stt":{"url":"http://[fd7a:115c:a1e0::1]:8118","token":"stt-token-not-this-one-0000000"},` + clipHalf + `}`,
		"without stt": `{"v":1,"type":"promptshipper-pairing","name":"box",` + clipHalf + `}`,
		"stt null":    `{"v":1,"type":"promptshipper-pairing","stt":null,` + clipHalf + `}`,
	}
	for form, body := range bodies {
		p := filepath.Join(dir, strings.ReplaceAll(form, " ", "-")+".json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, getenv := range map[string]func(string) string{
			"flag": func(string) string { return "" },
			"env": func(k string) string {
				if k == envPairingFile {
					return p
				}
				return ""
			},
		} {
			flag := ""
			if name == "flag" {
				flag = p
			}
			cfg, err := loadConfig(getenv, flag)
			if err != nil {
				t.Fatalf("%s via %s: %v", form, name, err)
			}
			if cfg.ClipURL != "http://[fd7a:115c:a1e0::1]:8139" || cfg.Token != testToken {
				t.Fatalf("%s via %s: cfg = %+v", form, name, cfg)
			}
		}
	}
}

func TestConfigRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := `"clip":{"url":"http://h:8139","token":"` + testToken + `"}`
	cases := []struct {
		name, want string
		env        map[string]string
		flag       string
	}{
		{"nothing", "no clip server configured", nil, ""},
		{"url only", "must be set together", map[string]string{envClipURL: "http://h:1"}, ""},
		{"both", "use exactly one", map[string]string{envClipURL: "http://h:1"}, write("a.json", "{}")},
		{"missing file", "reading pairing file", nil, filepath.Join(dir, "nope.json")},
		{"wrong type", `"type" is not`, nil, write("b.json", `{"v":1,"type":"wifi",`+ok+`}`)},
		{"no v", `"v" is not 1`, nil, write("c.json", `{"type":"promptshipper-pairing",`+ok+`}`)},
		{"v2", `"v" is not 1`, nil, write("d.json", `{"v":2,"type":"promptshipper-pairing",`+ok+`}`)},
		{"no clip", `"clip.url" and "clip.token" are required`, nil, write("e.json", `{"v":1,"type":"promptshipper-pairing"}`)},
		{"ftp", "scheme must be http or https", map[string]string{envClipURL: "ftp://h:1", envClipToken: testToken}, ""},
		{"path", "no path", map[string]string{envClipURL: "http://h:1/v1/clips", envClipToken: testToken}, ""},
		{"no host", "no host", map[string]string{envClipURL: "http://", envClipToken: testToken}, ""},
		{"short", "shorter than 24", map[string]string{envClipURL: "http://h:1", envClipToken: strings.Repeat("a", 23)}, ""},
		{"space", "outside [A-Za-z0-9._~-]", map[string]string{envClipURL: "http://h:1", envClipToken: strings.Repeat("a", 24) + " "}, ""},
	}
	for _, c := range cases {
		_, err := loadConfig(func(k string) string { return c.env[k] }, c.flag)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	// Boundary: exactly 24 characters is accepted.
	if _, err := loadConfig(func(k string) string {
		return map[string]string{envClipURL: "http://h:1", envClipToken: strings.Repeat("a", 24)}[k]
	}, ""); err != nil {
		t.Errorf("24-char token refused: %v", err)
	}
}

func TestRunExitsTwoOnBadConfigWithoutStartingAServer(t *testing.T) {
	var out, errb syncBuf
	rc := run(nil, func(string) string { return "" }, nil, &out, &errb)
	if rc != 2 || !strings.Contains(errb.String(), "no clip server configured") || out.String() != "" {
		t.Fatalf("rc=%d stdout=%q stderr=%q", rc, out.String(), errb.String())
	}
}
