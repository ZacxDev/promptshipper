package main

// The feed cursor is `seq`: a counter that only ever grows, set on INSERT and
// on every dedup BUMP. So an agent re-pushing identical text reaches the phone
// again (the user may have copied something else in between). These tests read
// `seq` out of the JSON so they speak the wire contract the app polls with.

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

type seqClip struct {
	ID   int64  `json:"id"`
	Seq  int64  `json:"seq"`
	Body string `json:"body"`
}

func feedSeq(t *testing.T, raw []byte) []seqClip {
	t.Helper()
	var fb struct {
		Clips []seqClip `json:"clips"`
	}
	if err := json.Unmarshal(raw, &fb); err != nil {
		t.Fatalf("feed body: %v: %s", err, raw)
	}
	return fb.Clips
}

func bodiesOf(cs []seqClip) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestARePushOfIdenticalTextIsDeliveredToAWaiter(t *testing.T) {
	srv, _ := feedServer(t)
	postClip(t, srv, "copy this again", "mcp")
	postClip(t, srv, "something else the user copied", "phone")
	_, raw := do(t, srv, "GET", "/v1/clips?after=0", "", true)
	all := feedSeq(t, raw)
	cursor := all[len(all)-1].Seq

	done := make(chan []seqClip, 1)
	go func() {
		_, raw := do(t, srv, "GET", "/v1/clips?after="+strconv.FormatInt(cursor, 10)+"&wait=20", "", true)
		done <- feedSeq(t, raw)
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("the waiter returned before the re-push: %v (cursor %d)", bodiesOf(got), cursor)
	default:
	}
	postClip(t, srv, "copy this again", "mcp") // a dedup BUMP of an existing body
	select {
	case got := <-done:
		if !sameStrings(bodiesOf(got), []string{"copy this again"}) || got[0].Seq <= cursor {
			t.Fatalf("waiter got %+v, want exactly the re-pushed clip with seq > %d", got, cursor)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a re-push of identical text never reached the waiter")
	}
}

func TestTheFeedIsOrderedBySeqAcrossInsertsAndBumps(t *testing.T) {
	srv, _ := feedServer(t)
	postClip(t, srv, "A", "p")
	postClip(t, srv, "B", "p")
	postClip(t, srv, "C", "p")
	postClip(t, srv, "A", "mcp") // bump
	postClip(t, srv, "C", "mcp") // bump
	postClip(t, srv, "D", "p")

	_, raw := do(t, srv, "GET", "/v1/clips?after=0", "", true)
	got := feedSeq(t, raw)
	if !sameStrings(bodiesOf(got), []string{"B", "A", "C", "D"}) {
		t.Fatalf("after=0 order %v, want B A C D (seq order: bumps move to the end)", bodiesOf(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq not strictly increasing: %+v", got)
		}
	}
	// A cursor in the middle returns exactly what came after it.
	_, raw = do(t, srv, "GET", "/v1/clips?after="+strconv.FormatInt(got[1].Seq, 10), "", true)
	if rest := bodiesOf(feedSeq(t, raw)); !sameStrings(rest, []string{"C", "D"}) {
		t.Fatalf("after=<seq of A> returned %v, want C D", rest)
	}
	// The newest-first LIST is unchanged by any of this: recency order.
	_, raw = do(t, srv, "GET", "/v1/clips", "", true)
	if list := bodiesOf(feedSeq(t, raw)); !sameStrings(list, []string{"D", "C", "A", "B"}) {
		t.Fatalf("plain list order %v, want D C A B (newest first by created_at)", list)
	}
}

func TestASeqIsNeverReusedAfterADelete(t *testing.T) {
	srv, _ := feedServer(t)
	postClip(t, srv, "A", "p")
	b := postClip(t, srv, "B", "p")
	_, raw := do(t, srv, "GET", "/v1/clips?after=0", "", true)
	all := feedSeq(t, raw)
	top := all[len(all)-1].Seq
	if code, _ := do(t, srv, "DELETE", "/v1/clips/"+strconv.FormatInt(b.ID, 10), "", true); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	postClip(t, srv, "C", "p")
	_, raw = do(t, srv, "GET", "/v1/clips?after="+strconv.FormatInt(top, 10), "", true)
	if got := bodiesOf(feedSeq(t, raw)); !sameStrings(got, []string{"C"}) {
		t.Fatalf("after deleting the newest row, a new clip was %v past the old top seq, want [C]", got)
	}
}
