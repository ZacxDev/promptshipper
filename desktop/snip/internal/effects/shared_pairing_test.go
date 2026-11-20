package effects

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The picker's clip source (Live.FetchShared / Live.DeleteClip, the
// picker.Launcher seam ctrl+g and ctrl+x call) configured ONLY by a
// promptshipper v1 pairing payload, against a fake clip server that implements
// the routes snip uses (GET /v1/clips, DELETE /v1/clips/{id}) and refuses any
// bearer token but the payload's clip.token. Synthetic URL and tokens only;
// the operator's config is never read (XDG_CONFIG_HOME and both overrides are
// pointed at a temp dir).
func TestTheClipSourceIsConfiguredFromThePairingPayload(t *testing.T) {
	clipTok := strings.Repeat("c1", 32)
	sttTok := strings.Repeat("57", 32)

	var mu sync.Mutex
	clips := map[string]string{"3": "older synthetic", "9": "newest synthetic"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+clipTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/clips":
			var rows []string
			for _, id := range []string{"9", "3"} {
				if b, ok := clips[id]; ok {
					rows = append(rows, `{"id":`+id+`,"body":"`+b+`","device":"phone","created_at":1,"bump_count":0}`)
				}
			}
			_, _ = w.Write([]byte(`{"clips":[` + strings.Join(rows, ",") + `],"limit":100,"query":""}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/clips/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/clips/")
			if _, ok := clips[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(clips, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("SNIP_SHARED_ENV", "")
	t.Setenv("SNIP_PAIRING", "")
	if err := os.MkdirAll(filepath.Join(dir, "snip"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := `PAIRING_PAYLOAD={"v":1,"type":"promptshipper-pairing","name":"test-box",` +
		`"stt":{"url":"http://100.101.102.103:8118","token":"` + sttTok + `"},` +
		`"clip":{"url":"` + srv.URL + `","token":"` + clipTok + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "snip", "pairing.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}

	rows, err := Live{}.FetchShared()
	if err != nil {
		t.Fatalf("FetchShared: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != 9 || rows[0].Body != "newest synthetic" || rows[1].ID != 3 {
		t.Fatalf("rows = %+v, want [9 newest, 3 older] in the server's order", rows)
	}
	if err := (Live{}).DeleteClip(9); err != nil {
		t.Fatalf("DeleteClip: %v", err)
	}
	rows, err = Live{}.FetchShared()
	if err != nil || len(rows) != 1 || rows[0].ID != 3 {
		t.Fatalf("after delete: rows = %+v, err = %v", rows, err)
	}
}
