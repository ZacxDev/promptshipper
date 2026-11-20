package main

// The newest-first list breaks created_at ties by RECENCY (seq), not by insert
// order (id). A dedup bump keeps its old, low id, so with `id DESC` a row
// bumped in the same millisecond as a newer insert sorted BELOW it — the
// opposite of recency. These tests put every write in ONE millisecond.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestTiesInTheListAreBrokenByRecencyNotInsertOrder(t *testing.T) {
	s := newTestStore(t)
	const sameMs = 5000
	put(t, s, "A", "p", sameMs)
	put(t, s, "B", "p", sameMs)
	put(t, s, "A", "mcp", sameMs) // bump: A is now more recent than B
	got, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if b := bodies(got); strings.Join(b, ",") != "A,B" {
		t.Fatalf("plain list %v, want A,B (A was bumped last)", b)
	}
}

func TestTiesAreBrokenByRecencyOnBothSearchPaths(t *testing.T) {
	s := newTestStore(t)
	const sameMs = 5000
	put(t, s, "alpha shared", "p", sameMs)
	put(t, s, "beta shared", "p", sameMs)
	put(t, s, "alpha shared", "mcp", sameMs)     // bump
	for _, q := range []string{"shared", "sh"} { // trigram index path, LIKE scan path
		got, err := s.List(context.Background(), 10, q)
		if err != nil {
			t.Fatal(err)
		}
		if b := bodies(got); strings.Join(b, ",") != "alpha shared,beta shared" {
			t.Fatalf("q=%q order %v, want the bumped row first", q, b)
		}
	}
}

func TestTheRecencyIndexIsMigratedToSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// The index as every earlier release created it.
	if _, err := s.db.Exec(`DROP INDEX clips_by_recency;
		CREATE INDEX clips_by_recency ON clips (created_at DESC, id DESC)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'clips_by_recency'`).
		Scan(&ddl); err != nil {
		t.Fatalf("clips_by_recency missing after start: %v", err)
	}
	if !strings.Contains(ddl, "seq DESC") || strings.Contains(ddl, "id DESC") {
		t.Fatalf("clips_by_recency after start is %q, want (created_at DESC, seq DESC)", ddl)
	}
}
