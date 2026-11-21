// Package values remembers input values the operator typed into the picker form,
// so next time the form pre-fills from remembered values rather than only from
// defaults. The file lives next to the recency file under XDG_STATE_HOME/snip/.
//
// 🔴 BEST-EFFORT IN BOTH DIRECTIONS. An unreadable file means "nothing
// remembered" (degrade gracefully); a failed write must never fail the paste.
// The file holds operator-typed text that can be sensitive: never log it, never
// put it in telemetry.
//
// 🔴 ONLY THE INTERACTIVE FORM WRITES HERE. CLI --set never remembers values,
// and --ephemeral rows are never recorded either.
package values

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// maxEntries bounds the file so a label/input churn cannot grow it without bound.
const maxEntries = 500

// Record is a single remembered value entry.
type Record struct {
	Key   string `json:"k"` // "<label>\000<name>"
	Value string `json:"v"`
	At    int64  `json:"at"` // unix millis
}

// File is the remembered-values store.
type File struct {
	Records []Record `json:"records"`
}

// Path returns the values file: $XDG_STATE_HOME/snip/values.json, else
// ~/.local/state/snip/values.json.
func Path() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "snip", "values.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("snip: resolve home for the values file: %w", err)
	}
	return filepath.Join(home, ".local", "state", "snip", "values.json"), nil
}

// Load reads the remembered values file. Every failure mode returns an empty
// store rather than an error: this is a convenience, and "I remember nothing"
// is a coherent answer.
func Load(path string) *File {
	raw, err := os.ReadFile(path)
	if err != nil {
		return &File{}
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return &File{}
	}
	return &f
}

// Get returns the remembered value for (label, name) and when it was stored.
func (f *File) Get(label, name string) (string, int64, bool) {
	key := label + "\000" + name
	for _, r := range f.Records {
		if r.Key == key {
			return r.Value, r.At, true
		}
	}
	return "", 0, false
}

// Set records a remembered value for (label, name) at the given time.
// Returns the store for chaining.
func (f *File) Set(label, name, value string, at int64) *File {
	key := label + "\000" + name
	for i, r := range f.Records {
		if r.Key == key {
			f.Records[i].Value = value
			f.Records[i].At = at
			return f
		}
	}
	f.Records = append(f.Records, Record{Key: key, Value: value, At: at})
	f.prune()
	return f
}

// Remove drops the remembered value for (label, name).
func (f *File) Remove(label, name string) {
	key := label + "\000" + name
	for i, r := range f.Records {
		if r.Key == key {
			f.Records[i] = f.Records[len(f.Records)-1]
			f.Records = f.Records[:len(f.Records)-1]
			return
		}
	}
}

func (f *File) prune() {
	if len(f.Records) <= maxEntries {
		return
	}
	// Newest first.
	sort.Slice(f.Records, func(i, j int) bool {
		if f.Records[i].At != f.Records[j].At {
			return f.Records[i].At > f.Records[j].At
		}
		return f.Records[i].Key < f.Records[j].Key
	})
	f.Records = f.Records[:maxEntries]
}

// Save writes the file atomically. Best-effort: a failed write must not fail
// the paste, and the caller already decided the paste outcome.
func Save(path string, f *File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	blob, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".values-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
