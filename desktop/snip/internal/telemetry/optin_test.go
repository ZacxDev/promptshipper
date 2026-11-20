package telemetry

import (
	"os"
	"path/filepath"
	"testing"
)

// spoolEnv points every spool-resolution input at a fresh temp dir and returns
// (state home, explicit spool dir). Neither exists until something writes.
func spoolEnv(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	spool := filepath.Join(root, "spool")
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("ACTIVITY_SPOOL_DIR", "")
	t.Setenv("HOME", filepath.Join(root, "home"))
	return state, spool
}

func fire() Payload {
	return Payload{Method: "search", Query: "eos", Outcome: "select", Label: "End of session", App: "Alacritty"}
}

// 🔴 BY DEFAULT NOTHING IS WRITTEN — not the file, not the directory — through
// either way a caller names the spool (the default resolution the picker and
// the paste helper use, an $ACTIVITY_SPOOL_DIR, or an explicit dir), and for
// every value of the switch that is not exactly "1".
func TestNothingIsWrittenUnlessOptedIn(t *testing.T) {
	for _, val := range []string{"<unset>", "", "0", "true", "yes", " 1", "1 "} {
		t.Run(val, func(t *testing.T) {
			state, spool := spoolEnv(t)
			if val == "<unset>" {
				t.Setenv(EnableEnv, "")
				os.Unsetenv(EnableEnv)
			} else {
				t.Setenv(EnableEnv, val)
			}
			EmitFireSource(fire(), SourceSnip, "")    // default resolution
			EmitFireSource(fire(), SourceClip, spool) // explicit dir
			t.Setenv("ACTIVITY_SPOOL_DIR", spool)
			EmitFireSource(fire(), SourceSnip, "") // the dir override alone is not an opt-in
			for _, p := range []string{state, spool} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s exists (err=%v): telemetry wrote with %s=%q", p, err, EnableEnv, val)
				}
			}
		})
	}
}

// Opting in restores the pre-change behaviour exactly: the default resolution
// lands in $XDG_STATE_HOME/activity/spool/current.log, and the bytes are the
// line Emit returned.
func TestOptingInWritesTheSameRow(t *testing.T) {
	state, _ := spoolEnv(t)
	t.Setenv(EnableEnv, "1")
	line := EmitFireSource(fire(), SourceSnip, "")
	raw, err := os.ReadFile(filepath.Join(state, "activity", "spool", CurrentName))
	if err != nil {
		t.Fatalf("opted in, but no spool file: %v", err)
	}
	if string(raw) != line+"\n" {
		t.Fatalf("spool = %q, want %q", raw, line+"\n")
	}
}
