package values

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Save writes the file with mode 0600 — the store holds operator-typed text.
func TestSaveModeIs0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.json")
	f := &File{}
	f.Set("L", "n", "v", 1)
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o, want 0600", got)
	}
}

// The write is atomic: the target is produced by renaming a temp file, so a
// reader never sees a partial file, no temp file survives, and the result is
// valid JSON.
func TestSaveIsAtomicAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "values.json")
	f := &File{}
	f.Set("L", "n", "one", 1)
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	// Overwrite: the second write must fully replace the first.
	f.Set("L", "n", "two", 2)
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if v, _, ok := Load(path).Get("L", "n"); !ok || v != "two" {
		t.Fatalf("Get = %q, %v, want two, true", v, ok)
	}
	// No temp file left behind — the rename consumed it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".values-") {
			t.Fatalf("a temp file survived Save: %s", e.Name())
		}
	}
	// The file is whole: it parses.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed File
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("the written file is not valid JSON (a partial write?): %v (%q)", err, raw)
	}
}

// A failed write must leave the previous file intact — never a truncated or
// partially-written store.
func TestSaveFailureLeavesThePreviousFileIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory does not deny writes")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "values.json")
	old := &File{}
	old.Set("L", "n", "old", 1)
	if err := Save(path, old); err != nil {
		t.Fatal(err)
	}
	// Deny CreateTemp in the directory: Save can only fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	newf := &File{}
	newf.Set("L", "n", "new", 2)
	if err := Save(path, newf); err == nil {
		t.Fatal("Save into a read-only directory must fail")
	}
	if v, _, ok := Load(path).Get("L", "n"); !ok || v != "old" {
		t.Fatalf("the previous file was disturbed by a failed write: got %q, %v, want old, true", v, ok)
	}
}

// The store is bounded to maxEntries, dropping the OLDEST by At.
func TestStoreIsBoundedDroppingTheOldest(t *testing.T) {
	f := &File{}
	for i := 1; i <= maxEntries+1; i++ {
		f.Set("L", fmt.Sprintf("n%03d", i), "v", int64(i))
	}
	if len(f.Records) != maxEntries {
		t.Fatalf("records = %d, want the bound %d", len(f.Records), maxEntries)
	}
	if _, _, ok := f.Get("L", "n001"); ok {
		t.Fatal("the OLDEST entry survived the bound")
	}
	if v, at, ok := f.Get("L", fmt.Sprintf("n%03d", maxEntries+1)); !ok || v != "v" || at != int64(maxEntries+1) {
		t.Fatalf("the NEWEST entry is missing: got %q, %d, %v", v, at, ok)
	}
}

// Load on a missing file is an empty store, not an error (best-effort), and a
// valid file is loaded — the positive control that the reader works at all.
func TestLoadMissingFileIsEmptyAndLoadReadsAValidOne(t *testing.T) {
	if f := Load(filepath.Join(t.TempDir(), "absent.json")); f == nil || len(f.Records) != 0 {
		t.Fatalf("Load(missing) = %+v, want a non-nil empty store", f)
	}
	// POSITIVE CONTROL: a valid file IS read, so the empty above is a real
	// empty and not a reader wired to nothing.
	path := filepath.Join(t.TempDir(), "values.json")
	src := &File{}
	src.Set("L", "n", "kept", 5)
	if err := Save(path, src); err != nil {
		t.Fatal(err)
	}
	if v, _, ok := Load(path).Get("L", "n"); !ok || v != "kept" {
		t.Fatalf("the control did not load a valid file (%q, %v) — the empty case proves nothing", v, ok)
	}
}

// Load on a corrupt or truncated file is an empty store, not an error.
func TestLoadCorruptOrTruncatedIsEmpty(t *testing.T) {
	for _, content := range []string{"{ not json", `{"records":[{"k":"x"`, "", `[1,2,3]`} {
		path := filepath.Join(t.TempDir(), "values.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if f := Load(path); f == nil || len(f.Records) != 0 {
			t.Fatalf("Load(%q) = %+v, want a non-nil empty store", content, f)
		}
	}
}

// Set updates an existing (label, name) in place and bumps At.
func TestSetUpdatesInPlaceAndBumpsAt(t *testing.T) {
	f := &File{}
	f.Set("L", "n", "one", 1)
	f.Set("L", "n", "two", 2)
	if len(f.Records) != 1 {
		t.Fatalf("records = %d, want 1 (updated in place)", len(f.Records))
	}
	v, at, ok := f.Get("L", "n")
	if !ok || v != "two" || at != 2 {
		t.Fatalf("Get = %q, %d, %v, want two, 2, true", v, at, ok)
	}
}

// Remove drops exactly that (label, name) and nothing else.
func TestRemoveDropsExactlyThatTuple(t *testing.T) {
	f := &File{}
	f.Set("L", "a", "1", 1)
	f.Set("L", "b", "2", 2)
	f.Set("M", "a", "3", 3)
	f.Remove("L", "a")
	if _, _, ok := f.Get("L", "a"); ok {
		t.Fatal("Remove did not drop (L, a)")
	}
	if v, _, ok := f.Get("L", "b"); !ok || v != "2" {
		t.Fatalf("Remove dropped the wrong tuple: (L, b) = %q, %v", v, ok)
	}
	if v, _, ok := f.Get("M", "a"); !ok || v != "3" {
		t.Fatalf("Remove dropped the wrong tuple: (M, a) = %q, %v", v, ok)
	}
	if len(f.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(f.Records))
	}
}
