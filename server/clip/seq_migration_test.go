package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// preSeqSchema is the schema as shipped before `seq` existed (verbatim
// columns and triggers), used to build a database the migration must upgrade.
const preSeqSchema = `
CREATE TABLE clips (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  body        TEXT    NOT NULL,
  body_sha256 TEXT    NOT NULL UNIQUE,
  device      TEXT    NOT NULL,
  created_at  INTEGER NOT NULL,
  bump_count  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX clips_by_recency ON clips (created_at DESC, id DESC);
CREATE VIRTUAL TABLE clips_fts USING fts5 (body, content='clips', content_rowid='id', tokenize='trigram');
CREATE TRIGGER clips_ai AFTER INSERT ON clips BEGIN
  INSERT INTO clips_fts (rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER clips_ad AFTER DELETE ON clips BEGIN
  INSERT INTO clips_fts (clips_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER clips_au AFTER UPDATE ON clips BEGIN
  INSERT INTO clips_fts (clips_fts, rowid, body) VALUES ('delete', old.id, old.body);
  INSERT INTO clips_fts (rowid, body) VALUES (new.id, new.body);
END;
`

func TestAPreSeqDatabaseIsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(preSeqSchema); err != nil {
		t.Fatal(err)
	}
	// ids 1..5 issued, 2 and 5 deleted: AUTOINCREMENT has handed out 5.
	for i, b := range []string{"one", "two", "three", "four", "five"} {
		if _, err := old.Exec(`INSERT INTO clips (body, body_sha256, device, created_at) VALUES (?, ?, 'p', ?)`,
			b, BodyHash(b), 100+i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`DELETE FROM clips WHERE id IN (2, 5)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	for round := 0; round < 2; round++ { // the migration must also be idempotent
		s, err := OpenStore(path)
		if err != nil {
			t.Fatalf("open (round %d): %v", round, err)
		}
		rows, err := s.After(context.Background(), 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			// Existing rows keep their old feed position: seq = id, so a client
			// whose cursor was an id from the previous server stays correct.
			for _, c := range rows {
				if c.Seq != c.ID {
					t.Fatalf("migrated row %+v: seq != id", c)
				}
			}
			if len(rows) != 3 {
				t.Fatalf("migrated %d rows, want 3", len(rows))
			}
			// A new clip lands past every id ever issued (5), not just past
			// the surviving maximum (4).
			c, _, err := s.Put(context.Background(), "six", "p", 200)
			if err != nil || c.Seq <= 5 {
				t.Fatalf("first post-migration seq %d (%v), want > 5", c.Seq, err)
			}
			// And a bump of a migrated row is delivered after it.
			b, _, err := s.Put(context.Background(), "one", "mcp", 201)
			if err != nil || b.Seq <= c.Seq || b.ID != 1 {
				t.Fatalf("bump of a migrated row: %+v (%v), want id 1 and seq > %d", b, err, c.Seq)
			}
		}
		s.Close()
	}
}
