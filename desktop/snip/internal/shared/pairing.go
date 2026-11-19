package shared

// The promptshipper pairing payload, version 1 (docs/pairing.md at the repo
// root is the contract; ./setup is the producer). snip is a SECOND consumer of
// it next to the Android app: the operator saves the payload once — the raw
// JSON, or the whole `PAIRING_PAYLOAD=<json>` line `./setup --pairing` prints —
// to ~/.config/snip/pairing.json (mode 0600), and the shared view reads its
// `clip` object.
//
// 🔴 THE CONTRACT'S REFUSALS ARE KEPT EVEN WHERE SNIP DOES NOT NEED THE FIELD.
// snip only talks to the clip server, but a payload missing `stt` is malformed
// in v1 ("must not pair half a server silently"), and a different `v` is
// rejected rather than parsed on a best-effort basis. A lenient second consumer
// is how a producer change that breaks the app ships unnoticed.
//
// 🔴 NO MESSAGE CARRIES A TOKEN. A refusal names the field and the rule; the
// URL may be quoted (the operator needs to know which endpoint), a token never.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const pairingFileName = "pairing.json"

// PairingVersion is the only payload version this build accepts.
const PairingVersion = 1

// PairingType is the payload's fixed discriminator.
const PairingType = "promptshipper-pairing"

// PairingMinTokenLen is the v1 consumer floor (clip refuses to start with a
// shorter token, so anything below it cannot be a real credential).
const PairingMinTokenLen = 24

// pairingLinePrefix is how ./setup prints the payload as text.
const pairingLinePrefix = "PAIRING_PAYLOAD="

type pairingEndpoint struct {
	URL   *string `json:"url"`
	Token *string `json:"token"`
}

type pairingPayload struct {
	V    *int             `json:"v"`
	Type *string          `json:"type"`
	Name string           `json:"name"`
	STT  *pairingEndpoint `json:"stt"`
	Clip *pairingEndpoint `json:"clip"`
}

// PairingPath resolves the pairing file: $SNIP_PAIRING overrides, else
// $XDG_CONFIG_HOME/snip/pairing.json, else ~/.config/snip/pairing.json — the
// same resolution EnvPath uses, so both config files sit together.
func PairingPath() (string, error) {
	if p := os.Getenv("SNIP_PAIRING"); p != "" {
		return p, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "snip", pairingFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("snip: resolve home for %s: %w", pairingFileName, err)
	}
	return filepath.Join(home, ".config", "snip", pairingFileName), nil
}

// loadPairing reads and parses the pairing file at path.
func loadPairing(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("no %s — save the pairing payload from `./setup --pairing` into it (mode 0600)", path)
		}
		return Config{}, fmt.Errorf("snip: %s: %w", path, err)
	}
	return ParsePairing(data, path)
}

// ParsePairing turns a v1 payload into the clip endpoint's Config. The input
// is the raw JSON object, or one `PAIRING_PAYLOAD=<json>` line; surrounding
// whitespace is ignored. path names the source in every message.
func ParsePairing(data []byte, path string) (Config, error) {
	text := strings.TrimSpace(string(data))
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.TrimPrefix(text, pairingLinePrefix)

	var p pairingPayload
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		// The decoder's error can quote input bytes, and the input holds the
		// tokens — so only its class is reported, never its text.
		return Config{}, fmt.Errorf("%s: not a pairing payload (want the JSON object ./setup prints)", path)
	}
	if p.Type == nil || *p.Type != PairingType {
		return Config{}, fmt.Errorf("%s: not a promptshipper pairing payload (type must be %q)", path, PairingType)
	}
	if p.V == nil {
		return Config{}, fmt.Errorf("%s: pairing payload has no version field \"v\"", path)
	}
	if *p.V != PairingVersion {
		return Config{}, fmt.Errorf("%s: pairing payload is version %d; this snip understands version %d only — update snip",
			path, *p.V, PairingVersion)
	}
	for _, ep := range []struct {
		name string
		e    *pairingEndpoint
	}{{"stt", p.STT}, {"clip", p.Clip}} {
		if err := validateEndpoint(path, ep.name, ep.e); err != nil {
			return Config{}, err
		}
	}
	return Config{
		URL:       *p.Clip.URL,
		Token:     *p.Clip.Token,
		TokenName: "clip.token in " + path,
	}, nil
}

func validateEndpoint(path, name string, e *pairingEndpoint) error {
	if e == nil {
		return fmt.Errorf("%s: pairing payload has no %q object — refusing to pair half a server", path, name)
	}
	if e.URL == nil || *e.URL == "" {
		return fmt.Errorf("%s: %s.url is missing", path, name)
	}
	if e.Token == nil || *e.Token == "" {
		return fmt.Errorf("%s: %s.token is missing", path, name)
	}
	if err := validateBaseURL(*e.URL); err != nil {
		return fmt.Errorf("%s: %s.url %q %v", path, name, *e.URL, err)
	}
	if err := validatePairingToken(*e.Token); err != nil {
		return fmt.Errorf("%s: %s.token %v", path, name, err)
	}
	return nil
}

// validateBaseURL: http or https, a host and a port, and nothing else. The
// client appends /v1/clips itself, so a path in the base breaks every request.
// A lone trailing "/" is tolerated (the client trims it); any other path is not.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("is not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("must be http:// or https://")
	case u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return fmt.Errorf("has no host")
	case u.Port() == "":
		return fmt.Errorf("has no port")
	case u.User != nil:
		return fmt.Errorf("must not carry credentials")
	case u.Path != "" && u.Path != "/":
		return fmt.Errorf("must not have a path")
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("must not have a query")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("must not have a fragment")
	}
	return nil
}

// validatePairingToken: at least PairingMinTokenLen characters from
// [A-Za-z0-9._~-]. The value is never part of the message.
func validatePairingToken(tok string) error {
	if len(tok) < PairingMinTokenLen {
		return fmt.Errorf("is shorter than %d characters", PairingMinTokenLen)
	}
	for _, r := range tok {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '~' || r == '-'
		if !ok {
			return fmt.Errorf("has a character outside [A-Za-z0-9._~-]")
		}
	}
	return nil
}
