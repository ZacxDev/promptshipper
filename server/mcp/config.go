package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
)

// Environment variables. The token has no flag on purpose: a flag value is
// visible in argv (`ps`, /proc/<pid>/cmdline) to every user on the machine.
const (
	envPairingFile = "PROMPTSHIPPER_PAIRING_FILE"
	envClipURL     = "PROMPTSHIPPER_CLIP_URL"
	envClipToken   = "PROMPTSHIPPER_CLIP_TOKEN"
)

// Bounds that mirror the clip server's own contract (server/clip/README.md and
// docs/pairing.md). They are checked HERE, at startup, so a misconfiguration is
// an exit-2 with a named reason rather than a 401 on the agent's first call.
const minTokenLen = 24

// tokenRe is the pairing-v1 token alphabet. 🔴 IT IS ALSO WHAT MAKES THE
// OUTPUT REDACTION COMPLETE: every character in it is one a JSON encoder emits
// verbatim (never \u-escaped), so a token inside any JSON string in an outbound
// frame appears there as its exact bytes and a byte-level replace finds it. A
// token with `"`, `\` or a control character would not have that property;
// refusing it is what lets redact.go be a byte replace. See redact.go.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// config is everything the server needs. String() never includes the token.
type config struct {
	ClipURL string // base URL, no path, no trailing slash
	Token   string
	Source  string // human description of where the config came from
}

func (c config) String() string {
	return fmt.Sprintf("clip=%s source=%s", c.ClipURL, c.Source)
}

// pairingV1 is the subset of docs/pairing.md this server reads. Unknown fields
// (name, a future mcp object) are ignored, as the format requires, and so is
// `stt`: it is optional here and never required — the desktop form of the
// payload omits it, and this server never talks to the STT server.
// TestConfigFromAPairingV1File covers the payload with and without it.
type pairingV1 struct {
	V    *int   `json:"v"`
	Type string `json:"type"`
	Clip *struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"clip"`
}

// loadConfig resolves the configuration from a pairing file (flag or env) OR
// from the URL+token env pair — exactly one of the two.
//
// 🔴 NO ERROR MESSAGE HERE MAY CONTAIN THE TOKEN, OR ANY PART OF A REJECTED
// ONE. These errors are printed before the redacting logger exists (it needs a
// valid token to redact). TestConfigErrorsNeverEchoTheToken pins it.
func loadConfig(getenv func(string) string, pairingFlag string) (config, error) {
	pairingPath := pairingFlag
	if pairingPath == "" {
		pairingPath = getenv(envPairingFile)
	}
	envURL, envTok := getenv(envClipURL), getenv(envClipToken)

	var cfg config
	switch {
	case pairingPath != "" && (envURL != "" || envTok != ""):
		// Two sources is a refusal, not a precedence rule: a stale env var
		// silently overriding a freshly re-paired file (or the reverse) is a
		// 401 whose cause is invisible.
		return config{}, fmt.Errorf("both a pairing file and %s/%s are set; use exactly one", envClipURL, envClipToken)
	case pairingPath != "":
		raw, err := os.ReadFile(pairingPath)
		if err != nil {
			return config{}, fmt.Errorf("reading pairing file: %w", err)
		}
		var p pairingV1
		if err := json.Unmarshal(raw, &p); err != nil {
			// Not %w: a json.SyntaxError carries no input, but a type error
			// can quote a value, and a value here may be a token.
			return config{}, errors.New("pairing file is not a JSON object (see docs/pairing.md)")
		}
		if p.Type != "promptshipper-pairing" {
			return config{}, errors.New(`pairing file: "type" is not "promptshipper-pairing"`)
		}
		if p.V == nil || *p.V != 1 {
			return config{}, errors.New(`pairing file: "v" is not 1; this server reads pairing format v1 only`)
		}
		if p.Clip == nil || p.Clip.URL == "" || p.Clip.Token == "" {
			return config{}, errors.New(`pairing file: "clip.url" and "clip.token" are required`)
		}
		cfg = config{ClipURL: p.Clip.URL, Token: p.Clip.Token, Source: "pairing file " + pairingPath}
	case envURL != "" || envTok != "":
		if envURL == "" || envTok == "" {
			return config{}, fmt.Errorf("%s and %s must be set together", envClipURL, envClipToken)
		}
		cfg = config{ClipURL: envURL, Token: envTok, Source: "environment"}
	default:
		return config{}, fmt.Errorf("no clip server configured: set %s (or pass -pairing), or set %s and %s",
			envPairingFile, envClipURL, envClipToken)
	}

	u, err := normaliseBaseURL(cfg.ClipURL)
	if err != nil {
		return config{}, err
	}
	cfg.ClipURL = u

	if len(cfg.Token) < minTokenLen {
		return config{}, fmt.Errorf("clip token is shorter than %d characters", minTokenLen)
	}
	if !tokenRe.MatchString(cfg.Token) {
		return config{}, errors.New("clip token has characters outside [A-Za-z0-9._~-]")
	}
	return cfg, nil
}

// normaliseBaseURL enforces the pairing-v1 URL rule: scheme http(s), a host,
// and nothing else. A single trailing "/" is accepted and dropped (it is
// unambiguous); any other path is refused because this server appends
// /v1/clips itself and a path would break every request.
//
// The URL is never echoed back: a user who pasted `http://user:secret@…` would
// otherwise see the secret in the error.
func normaliseBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("clip url does not parse")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("clip url scheme must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("clip url has no host")
	}
	if u.User != nil {
		return "", errors.New("clip url must not carry user info; the token goes in the token field")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("clip url must be scheme://host:port with no path, query or fragment")
	}
	return u.Scheme + "://" + u.Host, nil
}
