package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pasteCatalog is the CLI fixture: a row with no inputs, a row with a required
// text input plus a select, and a row with TWO required inputs (so a repeated
// --set flag is observable).
const pasteCatalog = `[
 {"label":"plain","body":"nothing to fill","search_terms":[],"category":"prompt"},
 {"label":"deploy","body":"Deploy {{service}} to {{env}}.","search_terms":[],"category":"prompt",
  "inputs":[
    {"name":"service","type":"text","prompt":"Service","required":true},
    {"name":"env","type":"select","prompt":"Environment","options":["staging","prod"],"default":"staging"}
  ]},
 {"label":"two req","body":"{{a}} and {{b}}","search_terms":[],"category":"prompt",
  "inputs":[
    {"name":"a","type":"text","required":true},
    {"name":"b","type":"text","required":true}
  ]}
]`

// pasteFixture points the CLI at the catalog and isolates every side effect: the
// telemetry spool, the recency file and the toast. The paste paths under test
// abort before any X call (no origin window), so no X server is needed.
func pasteFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "snippets.json")
	if err := os.WriteFile(path, []byte(pasteCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SNIP_CONFIG", path)
	t.Setenv("ACTIVITY_SPOOL_DIR", filepath.Join(dir, "spool"))
	t.Setenv("SNIP_ORIGIN_WINDOW", "")
	// Keep dunstify from doing anything real if a notification daemon is present.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent-snip-test")
}

// captureStdout runs fn with stdout redirected and returns what it wrote and
// fn's exit code.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	rc := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), rc
}

// captureStderr is captureStdout for stderr.
func captureStderr(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	rc := fn()
	w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out), rc
}

// 🔴 A MALFORMED --set IS A USAGE ERROR (exit 2), NOT A SILENT EMPTY VALUE.
// A name-less or value-less spelling would otherwise paste a body with the
// placeholder unresolved.
func TestPasteSetUsageErrors(t *testing.T) {
	pasteFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{"no value", []string{"--label", "deploy", "--set"}},
		{"no equals", []string{"--label", "deploy", "--set", "novalue"}},
		{"empty name", []string{"--label", "deploy", "--set", "=v"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, rc := captureStderr(t, func() int { return runPaste(c.args) }); rc != 2 {
				t.Fatalf("exit code = %d, want 2 (usage)", rc)
			}
		})
	}
}

// 🔴 --set IS REPEATABLE, AND name=value SPLITS ON THE FIRST '='. Both are
// observed through the required-input check: a row with two required inputs gets
// past it only when BOTH flags are parsed, and a value carrying '=' keeps its
// name intact (so the check sees input "a", not "a=x").
func TestPasteSetRepeatableAndFirstEquals(t *testing.T) {
	pasteFixture(t)

	// Both flags parsed: the row clears the required check and aborts for the
	// missing origin window instead.
	out, rc := captureStderr(t, func() int {
		return runPaste([]string{"--label", "two req", "--set", "a=1", "--set", "b=2"})
	})
	if rc != 1 {
		t.Fatalf("exit code = %d, want 1", rc)
	}
	if !strings.Contains(out, "no origin window") {
		t.Fatalf("stderr = %q, want the no-origin-window abort — the row should have "+
			"cleared the required-input check because BOTH --set flags were parsed", out)
	}
	// Positive control: with only ONE flag the check still refuses and NAMES the
	// other, so the success above is about the second flag and not a check that
	// never runs.
	out, _ = captureStderr(t, func() int {
		return runPaste([]string{"--label", "two req", "--set", "a=1"})
	})
	if !strings.Contains(out, "required inputs with no values: b") {
		t.Fatalf("control stderr = %q, want it to name the missing b — this control proves nothing", out)
	}

	// The first '=' splits: "a=x=y" must be name "a", value "x=y". A last-'='
	// split would leave name "a=x" and the required input "a" missing.
	out, _ = captureStderr(t, func() int {
		return runPaste([]string{"--label", "two req", "--set", "a=x=y", "--set", "b=2"})
	})
	if !strings.Contains(out, "no origin window") {
		t.Fatalf("stderr = %q, want the no-origin-window abort: --set a=x=y must parse as "+
			"name a, value x=y (split on the FIRST =)", out)
	}
}

// A row with required inputs and NO --set values refuses loudly, exit 1, naming
// EVERY missing input — never pasting unresolved placeholders.
func TestPasteMissingRequiredNamesEveryMissingInput(t *testing.T) {
	pasteFixture(t)
	out, rc := captureStderr(t, func() int { return runPaste([]string{"--label", "two req"}) })
	if rc != 1 {
		t.Fatalf("exit code = %d, want 1", rc)
	}
	if !strings.Contains(out, "required inputs with no values: a, b") {
		t.Fatalf("stderr = %q, want it to name BOTH missing inputs", out)
	}
}

// expand --set substitutes via the same Apply; a value carrying '=' survives.
func TestExpandSetSubstitutes(t *testing.T) {
	pasteFixture(t)
	out, rc := captureStdout(t, func() int {
		return runExpand([]string{"--set", "service=a=b", "--set", "env=prod", "deploy"})
	})
	if rc != 0 {
		t.Fatalf("exit code = %d, want 0", rc)
	}
	if !strings.Contains(out, "Deploy a=b to prod.") {
		t.Fatalf("stdout = %q, want the substituted body with the value a=b intact", out)
	}
}

// expand WITHOUT --set on a row with a required input exits 1 naming it.
func TestExpandWithoutSetExits1(t *testing.T) {
	pasteFixture(t)
	out, rc := captureStderr(t, func() int { return runExpand([]string{"deploy"}) })
	if rc != 1 {
		t.Fatalf("exit code = %d, want 1", rc)
	}
	if !strings.Contains(out, "service") {
		t.Fatalf("stderr = %q, want it to name the required input service", out)
	}
}

// get stays RAW: {{name}} placeholders are returned untouched.
func TestGetLeavesPlaceholdersUntouched(t *testing.T) {
	pasteFixture(t)
	out, rc := captureStdout(t, func() int { return runGet([]string{"deploy"}) })
	if rc != 0 {
		t.Fatalf("exit code = %d, want 0", rc)
	}
	if !strings.Contains(out, "{{service}}") || !strings.Contains(out, "{{env}}") {
		t.Fatalf("stdout = %q, want the raw body with both placeholders untouched", out)
	}
}
