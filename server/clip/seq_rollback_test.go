package main

// Two ways a deployment can leave the feed's cursor and the store disagreeing:
//
//  1. ROLLBACK: the previous binary (no `seq`) runs against a migrated
//     database. Its INSERT leaves seq = 0. That must not break later POSTs.
//  2. A CURSOR AHEAD OF THE COUNTER: a client kept `after=<seq>` from a store
//     that was since restored from an older backup or replaced by a fresh
//     volume. Every feed answer carries `head`, and `after > head` answers at
//     once so the client can resync instead of waiting forever.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type headBody struct {
	Clips *[]seqClip `json:"clips"`
	Head  *int64     `json:"head"`
}

func headFeed(t *testing.T, srv *httptest.Server, query string) (int, headBody, time.Duration) {
	t.Helper()
	t0 := time.Now()
	code, raw := do(t, srv, "GET", "/v1/clips?"+query, "", true)
	var hb headBody
	if code == http.StatusOK {
		if err := json.Unmarshal(raw, &hb); err != nil {
			t.Fatalf("feed body: %v: %s", err, raw)
		}
	}
	return code, hb, time.Since(t0)
}

func TestPostKeepsWorkingAfterAnOldBinaryInsertedSeqZeroRows(t *testing.T) {
	s := newTestStore(t)
	// What the pre-seq binary's INSERT does against a migrated database: no
	// seq column in its statement, so every row it writes gets the DEFAULT 0.
	for _, b := range []string{"old binary 1", "old binary 2", "old binary 3"} {
		if _, err := s.db.Exec(`INSERT INTO clips (body, body_sha256, device, created_at) VALUES (?, ?, 'old', 1)`,
			b, BodyHash(b)); err != nil {
			t.Fatalf("an old-binary insert failed: %v — a UNIQUE seq index refuses the second seq=0 row", err)
		}
	}
	api := NewAPI(s, feedToken, time.Now)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	a := postClip(t, srv, "new binary 1", "mcp")
	b := postClip(t, srv, "new binary 2", "mcp")
	code, hb, _ := headFeed(t, srv, "after=0")
	if code != 200 || hb.Clips == nil {
		t.Fatalf("feed after an old-binary insert: %d", code)
	}
	got := bodiesOf(*hb.Clips)
	if !sameStrings(got, []string{"new binary 1", "new binary 2"}) {
		t.Fatalf("feed delivered %v, want the two new clips (seq=0 rows are not in the feed)", got)
	}
	if a.ID == b.ID {
		t.Fatal("two new clips shared an id")
	}
}

func TestAnExistingUniqueSeqIndexIsReplacedOnStart(t *testing.T) {
	path := t.TempDir() + "/c.db"
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// The index as the first cut of this branch created it.
	if _, err := s.db.Exec(`DROP INDEX clips_by_seq; CREATE UNIQUE INDEX clips_by_seq ON clips (seq)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var unique int
	if err := s.db.QueryRow(`SELECT "unique" FROM pragma_index_list('clips') WHERE name = 'clips_by_seq'`).
		Scan(&unique); err != nil {
		t.Fatalf("clips_by_seq missing after start: %v", err)
	}
	if unique != 0 {
		t.Fatal("clips_by_seq is still UNIQUE after start; an old-binary rollback would break POST")
	}
}

func TestEveryFeedAnswerCarriesHead(t *testing.T) {
	srv, _ := feedServer(t)
	postClip(t, srv, "A", "p")
	postClip(t, srv, "B", "p")
	postClip(t, srv, "A", "mcp") // a bump also advances the counter
	_, hb, _ := headFeed(t, srv, "after=0")
	if hb.Head == nil {
		t.Fatal("the feed answer has no `head`")
	}
	clips := *hb.Clips
	if *hb.Head != clips[len(clips)-1].Seq {
		t.Fatalf("head = %d, want the counter's value %d", *hb.Head, clips[len(clips)-1].Seq)
	}
	_, hb2, _ := headFeed(t, srv, "after="+strconv.FormatInt(*hb.Head, 10)+"&wait=1")
	if hb2.Head == nil || *hb2.Head != *hb.Head || len(*hb2.Clips) != 0 {
		t.Fatalf("an expired wait must still carry head: %+v", hb2)
	}
	// head is never in the plain newest-first list, whose shape is unchanged.
	_, raw := do(t, srv, "GET", "/v1/clips", "", true)
	var plain map[string]json.RawMessage
	_ = json.Unmarshal(raw, &plain)
	if _, has := plain["head"]; has {
		t.Fatalf("the plain list grew a head field: %s", raw)
	}
}

func TestACursorAheadOfHeadAnswersAtOnceWithHead(t *testing.T) {
	srv, _ := feedServer(t)
	postClip(t, srv, "only", "p")
	_, hb, _ := headFeed(t, srv, "after=0")
	if hb.Head == nil {
		t.Fatal("the feed answer has no `head`")
	}
	head := *hb.Head
	code, ahead, took := headFeed(t, srv, "after="+strconv.FormatInt(head+50, 10)+"&wait=20")
	if code != 200 || took > 3*time.Second {
		t.Fatalf("after > head with wait=20: %d after %v, want an immediate 200", code, took)
	}
	if ahead.Head == nil || *ahead.Head != head || ahead.Clips == nil || len(*ahead.Clips) != 0 {
		t.Fatalf("after > head answered %+v, want clips: [] and head %d", ahead, head)
	}
}
