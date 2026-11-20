package main

// The clip server must never STORE its own credential. If the bearer token is
// ever copied on a device and captured as a clip, anything that can read clips
// (an agent through the MCP server) could read the token back out of the store.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAClipContainingTheServersOwnTokenIsRefusedAndNotStored(t *testing.T) {
	srv, _ := feedServer(t)
	for _, tc := range []struct{ name, body, device string }{
		{"the token alone", feedToken, "phone"},
		{"the token inside other text", "my clip server token is " + feedToken + " ok", "phone"},
		{"the token as the device", "harmless", feedToken[:20] + feedToken[20:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(map[string]string{"body": tc.body, "device": tc.device})
			code, raw := do(t, srv, "POST", "/v1/clips", string(b), true)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("POST of a clip containing the server token answered %d %s, want 422", code, raw)
			}
			if !bytes.Contains(raw, []byte(`"contains_server_token"`)) {
				t.Fatalf("422 without the stable code: %s", raw)
			}
			if bytes.Contains(raw, []byte(feedToken)) {
				t.Fatalf("the refusal echoed the token: %s", raw)
			}
		})
	}
	code, raw := do(t, srv, "GET", "/v1/clips", "", true)
	if code != 200 || !bytes.Contains(raw, []byte(`"clips":[]`)) {
		t.Fatalf("something was stored after the refusals: %d %s", code, raw)
	}
}

func TestAnUnrelatedHexSecretIsStillStored(t *testing.T) {
	srv, _ := feedServer(t)
	other := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // 64 hex, not the token
	c := postClip(t, srv, other, "laptop")
	if c.Body != other {
		t.Fatalf("an unrelated 64-hex value was not stored as-is: %+v", c)
	}
}

// seedRows writes rows straight into the store, as an older server without
// the POST guard would have.
func seedRows(t *testing.T, s *Store, rows ...[2]string) {
	t.Helper()
	for i, r := range rows {
		if _, _, err := s.Put(context.Background(), r[0], r[1], int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTheStartupScanCountsTokenClipsWithoutPrintingThem(t *testing.T) {
	s := newTestStore(t)
	seedRows(t, s,
		[2]string{"pasted " + feedToken, "phone"},
		[2]string{"harmless", feedToken},
		[2]string{"nothing to see", "laptop"},
		[2]string{"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "laptop"})
	var out bytes.Buffer
	reportTokenClips(context.Background(), s, feedToken, &out)
	msg := out.String()
	if !strings.Contains(msg, "2 stored clip(s) contain this server's own access token") {
		t.Fatalf("scan did not report exactly 2: %q", msg)
	}
	if strings.Contains(msg, feedToken) || strings.Contains(msg, "pasted") || strings.Contains(msg, "harmless") {
		t.Fatalf("the scan printed clip content or the token: %q", msg)
	}
	if !strings.Contains(msg, "rotate the token with ./setup --rotate") {
		t.Fatalf("the scan does not tell the operator to rotate the token: %q", msg)
	}

	var clean bytes.Buffer
	reportTokenClips(context.Background(), newTestStore(t), feedToken, &clean)
	if clean.Len() != 0 {
		t.Fatalf("a clean store produced a warning: %q", clean.String())
	}
}
