// Store tests.
//
// 🔴 WHAT THESE TESTS ARE, STATED FIRST, BECAUSE A READER WILL ASSUME OTHERWISE.
// store.go is a NEW FILE in a NEW SERVICE. There is no pre-change code for
// almost anything here to be red against, so with the exceptions named below
// every test in this file is an INVARIANT GUARD / CHARACTERIZATION TEST — it
// pins what the code does so a later change has to argue with it — and NOT
// regression coverage. Most of them have never caught a bug that shipped,
// because nothing has shipped. Counting them as regression coverage would be a
// claim stronger than the code, which is the one thing this repo's rules forbid
// absolutely.
//
// 🔴 ITEMS 3-10 ARE DIFFERENT, AND THEY ARE THE ONLY ONES THAT ARE. The trigram
// change HAS a pre-change state — the unicode61 index — so each was watched FAIL
// against base ref 2f5a7f89e and pass after. The matrix is recorded against each
// test, with the exact message it failed with. Item 2 of the original pair is
// unchanged. Items 3-10 are genuine regression coverage for the semantics this
// change moved; nothing else in this file is.
//
// THE ONES THAT CAN BE WATCHED FAIL, with the mutation or base ref that does it:
//
//  1. TestSearchFindsARowWrittenThroughTheTriggers — this one is NOT
//     hypothetical. It was RED against store.go as first written (2026-11-14,
//     before the fix in this same change), whose List used
//     `JOIN clips_fts f ON f.rowid = c.id WHERE f MATCH ?`. FTS5 requires the
//     table's own NAME on the left of MATCH; an alias parses as a column, so
//     every `?q=` request answered `SQL logic error: no such column: f` and the
//     handler turned it into 503 store_unreadable. Measured by hand with curl
//     before this test existed, and the test reproduces it: restore the JOIN
//     form and it fails with that exact SQLite message.
//
//  2. TestADeletedClipLeavesNothingInTheSearchIndex — delete the `clips_ad`
//     trigger from `schema` and watch it go red with its own message ("the FTS5
//     index still holds N entry/entries"). Observed, not assumed.
//
// THE TRIGRAM ITEMS, each RED at base ref 2f5a7f89e (the unicode61 index) and
// green at HEAD. The message is the one the test actually printed:
//
//  3. TestSearchFindsASubstringMidWord — "q=assw returned 0 rows, want 1".
//  4. TestATwoCharQueryDoesNotSilentlyReturnEmpty — "q=hi returned 0 rows,
//     want 1".
//  5. TestLikeWildcardsInAQueryAreLiteralsNotWildcards — "q=% returned 0 rows,
//     want exactly 1" and the same for `_` and `\`.
//  6. TestAPunctuationOnlyQueryIsARealSearch — "q=---- returned 0 rows, want 1".
//  7. TestSearchIsCaseInsensitiveForSubstrings — "q=ASSW returned 0 rows,
//     want 1".
//  8. TestEveryTokenMustMatchAsASubstringOnBothPaths — "q=lph br returned 0
//     rows, want 1".
//  9. TestTheUpdateTriggerKeepsTheTrigramIndexCorrect — "q=assw after a dedup
//     bump returned 0 rows, want exactly 1".
//  10. TestOpenStoreMigratesAUnicode61IndexToTrigram — the migration. It cannot
//     be red "at base" in the same sense (base has nothing to migrate TO), so it
//     was watched fail against HEAD with ensureTrigramIndex's call REMOVED from
//     OpenStore: "the clips_fts DDL is ... which does not declare trigram".
//
// ⚠ TWO TESTS HERE PASSED AT BASE AS WELL, COINCIDENTALLY, AND ARE LABELLED
// INVARIANT GUARDS RATHER THAN COUNTED ABOVE: TestLikeWildcardsAreAlsoLiterals-
// OnTheIndexPath (unicode61's tokenisation happened to give the same answer) and
// TestAQueryThatMatchesNothingIsAnEmptyResultNotAnError. An earlier draft of
// item 4 also passed at base, because its fixture used `q=is` against
// `this is a clip` where `is` is a whole WORD; the fixture was changed to a
// mid-word 2-char needle precisely because the first one reported the hazard as
// already handled. That near-miss is the reason this header insists on the
// matrix.
//
// 🔴 AND ONE CLAIM THIS HEADER USED TO MAKE AND WHICH WAS MEASURED FALSE —
// recorded because the correction is the useful part. It said
// TestSearchNoLongerFindsADeletedRow goes red when `clips_ad` is removed. It
// does NOT: that mutant SURVIVED a run of exactly that test. The reason is a
// property of List's own SQL — `WHERE c.id IN (SELECT rowid FROM clips_fts …)`
// filters a stale index entry out through the main table, because the clips row
// is gone — so a desynchronised index is invisible to every query that goes
// through List. The guard was reading as coverage of the trigger while providing
// none, which is worse than no guard because it stops anyone looking. Item 2
// above is the replacement: it interrogates `clips_fts` DIRECTLY, which is the
// only surface where the trigger's absence is observable.
//
// That also means the trigger is not load-bearing for search CORRECTNESS. What
// it is load-bearing for is that a deleted clip's text stops being in the
// database at all: design decision "no expiry" makes delete the only hygiene mechanism there is,
// and a clip deleted because it held a token must not survive as tokens in an
// index. Item 2 is the guard on THAT, which is the property worth having.
//
// Everything else: an invariant guard. Labelled, not inflated.
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// A real file, not :memory:. WAL mode, the UNIQUE index and the FTS5 shadow
	// tables are all file-backed behaviours, and ":memory:" would quietly test a
	// different engine configuration from the one that ships.
	path := filepath.Join(t.TempDir(), "clipstore.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// put is a terse helper. It takes an explicit timestamp because ordering and
// dedup-bump assertions depend on two DIFFERENT times, and time.Now() in a test
// can hand you the same millisecond twice.
func put(t *testing.T, s *Store, body, device string, at int64) (Clip, bool) {
	t.Helper()
	clip, deduped, err := s.Put(context.Background(), body, device, at)
	if err != nil {
		t.Fatalf("Put(%q, %q, %d): %v", body, device, at, err)
	}
	return clip, deduped
}

func bodies(clips []Clip) []string {
	out := make([]string, 0, len(clips))
	for _, c := range clips {
		out = append(out, c.Body)
	}
	return out
}

// Invariant guard. Re-capturing an identical body must BUMP the existing row,
// not add a second one — the property the design names as one of the
// four things an existing chat transport cannot do.
func TestPutDedupBumpsRatherThanDuplicating(t *testing.T) {
	s := newTestStore(t)

	first, deduped := put(t, s, "ssh prod", "pixel-7", 1000)
	if deduped {
		t.Fatal("the first POST of a body reported deduped=true; it inserted a new row")
	}
	if first.BumpCount != 0 {
		t.Fatalf("a freshly inserted clip has bump_count %d, want 0", first.BumpCount)
	}

	put(t, s, "something else", "laptop", 2000)

	again, deduped := put(t, s, "ssh prod", "laptop", 3000)
	if !deduped {
		t.Fatal("re-capturing an identical body reported deduped=false; it duplicated the row")
	}
	if again.ID != first.ID {
		t.Fatalf("the bump landed on id %d, want the original id %d — that is a second row, "+
			"not a bump", again.ID, first.ID)
	}
	if again.CreatedAt != 3000 {
		t.Fatalf("created_at is %d after the bump, want 3000 — the bump did not resurface it",
			again.CreatedAt)
	}
	if again.BumpCount != 1 {
		t.Fatalf("bump_count is %d after one bump, want 1", again.BumpCount)
	}
	if again.Device != "laptop" {
		t.Fatalf("device is %q after a bump from laptop, want %q — a bump re-attributes the "+
			"clip to where it most recently came from", again.Device, "laptop")
	}

	// The whole point: TWO rows, and the bumped one is newest.
	clips, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := bodies(clips), []string{"ssh prod", "something else"}; !equalStrings(got, want) {
		t.Fatalf("List returned %q, want %q — newest-first with ONE row per distinct body",
			got, want)
	}
}

// Invariant guard. A third capture must keep counting, so bump_count is a real
// counter rather than a boolean spelled as an integer.
func TestPutBumpCountKeepsCounting(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "x", "a", 1000)
	put(t, s, "x", "a", 2000)
	third, deduped := put(t, s, "x", "a", 3000)
	if !deduped || third.BumpCount != 2 {
		t.Fatalf("after three captures of one body: deduped=%v bump_count=%d, want true and 2",
			deduped, third.BumpCount)
	}
}

// Invariant guard. Delete-by-id from any device is a day-one requirement, and
// the no-expiry decision makes it the ONLY hygiene mechanism there is.
func TestDeleteByIDRemovesExactlyThatRow(t *testing.T) {
	s := newTestStore(t)
	keep, _ := put(t, s, "keep me", "a", 1000)
	drop, _ := put(t, s, "drop me", "a", 2000)

	if err := s.Delete(context.Background(), drop.ID); err != nil {
		t.Fatalf("Delete(%d): %v", drop.ID, err)
	}
	clips, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := bodies(clips), []string{"keep me"}; !equalStrings(got, want) {
		t.Fatalf("after deleting %d the store holds %q, want %q", drop.ID, got, want)
	}
	if clips[0].ID != keep.ID {
		t.Fatalf("the surviving row has id %d, want %d", clips[0].ID, keep.ID)
	}
}

// Invariant guard. "It is gone" and "it was never here" must be different
// answers — a client retrying a delete it believes failed needs to know whether
// the first attempt landed.
func TestDeleteAnAbsentIDReportsNoSuchClip(t *testing.T) {
	s := newTestStore(t)
	only, _ := put(t, s, "only", "a", 1000)
	if err := s.Delete(context.Background(), only.ID+9999); !errors.Is(err, ErrNoSuchClip) {
		t.Fatalf("Delete of an absent id returned %v, want ErrNoSuchClip", err)
	}
	// And a second delete of a row that HAS been deleted is the same answer.
	if err := s.Delete(context.Background(), only.ID); err != nil {
		t.Fatalf("Delete(%d): %v", only.ID, err)
	}
	if err := s.Delete(context.Background(), only.ID); !errors.Is(err, ErrNoSuchClip) {
		t.Fatalf("a repeated Delete returned %v, want ErrNoSuchClip", err)
	}
}

// 🔴 CAN BE WATCHED FAIL — see this file's header, item 1. Red against store.go
// as first written (`WHERE f MATCH ?` against an alias), with SQLite's own
// "no such column: f".
func TestSearchFindsARowWrittenThroughTheTriggers(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "ssh prod password hunter2", "pixel-7", 1000)
	put(t, s, "a totally different clip", "laptop", 2000)

	clips, err := s.List(context.Background(), 10, "hunter2")
	if err != nil {
		t.Fatalf("List with q=hunter2: %v — the FTS5 index is not queryable", err)
	}
	if got, want := bodies(clips), []string{"ssh prod password hunter2"}; !equalStrings(got, want) {
		t.Fatalf("q=hunter2 returned %q, want %q", got, want)
	}
}

// INVARIANT GUARD, and explicitly NOT a guard on the `clips_ad` trigger — see
// this file's header for the measurement that refuted that claim. List's
// subquery against the main table filters a stale index entry out on its own, so
// this test passes with the trigger deleted. It pins the user-visible contract
// ("a deleted clip stops being findable"), which is worth pinning on its own;
// the trigger's own guard is TestADeletedClipLeavesNothingInTheSearchIndex.
func TestSearchNoLongerFindsADeletedRow(t *testing.T) {
	s := newTestStore(t)
	clip, _ := put(t, s, "zzyzx singular token", "a", 1000)
	if err := s.Delete(context.Background(), clip.ID); err != nil {
		t.Fatalf("Delete(%d): %v", clip.ID, err)
	}
	clips, err := s.List(context.Background(), 10, "zzyzx")
	if err != nil {
		t.Fatalf("List with q=zzyzx after the delete: %v", err)
	}
	if len(clips) != 0 {
		t.Fatalf("q=zzyzx still returns %d row(s) after the row was deleted: %q — the FTS5 "+
			"index was not told about the delete (the clips_ad trigger)", len(clips), bodies(clips))
	}
}

// 🔴 CAN BE WATCHED FAIL — see this file's header, item 2. Delete the `clips_ad`
// trigger from `schema` and this goes red with its own message; it is the ONLY
// test in the package that does.
//
// 🔴 IT QUERIES `clips_fts` DIRECTLY, WHICH IS THE WHOLE POINT. Going through
// List cannot see this: List's `WHERE c.id IN (SELECT rowid FROM clips_fts …)`
// discards an index entry whose clips row is gone, so a completely
// desynchronised index still produces correct search results. Only the index
// itself can be asked whether it still holds the body.
//
// WHY IT MATTERS, beyond tidiness. The FTS5 table is external-content, so it
// stores the TOKENS of a body rather than the body — but a token is the body for
// anything short, and `MATCH` over the index is a working oracle for "was this
// string ever here". There is no expiry, so delete is the only hygiene mechanism
// there is, and a deleted clip already survives in any backup taken before the
// delete. Surviving in the LIVE database as well, in a
// table nothing cleans, is a second copy nobody signed off on.
func TestADeletedClipLeavesNothingInTheSearchIndex(t *testing.T) {
	s := newTestStore(t)
	// Two rows, so the assertion cannot pass merely because the index is empty.
	keep, _ := put(t, s, "quetzal retained token", "a", 1000)
	drop, _ := put(t, s, "zzyzx deleted token", "a", 2000)

	// POSITIVE CONTROL first: the index CAN see the body before the delete. A
	// zero from a probe that never worked is indistinguishable from a real zero.
	if n := indexHits(t, s, "zzyzx"); n != 1 {
		t.Fatalf("before the delete the index holds %d entry/entries for zzyzx, want 1 — this "+
			"probe is wired to nothing, so the assertion below would be vacuous", n)
	}

	if err := s.Delete(context.Background(), drop.ID); err != nil {
		t.Fatalf("Delete(%d): %v", drop.ID, err)
	}

	if n := indexHits(t, s, "zzyzx"); n != 0 {
		t.Fatalf("after deleting clip %d the FTS5 index still holds %d entry/entries for its "+
			"body. The `clips_ad` AFTER DELETE trigger in `schema` is the only thing that "+
			"removes them, and List cannot see this because its subquery filters stale rowids "+
			"out through the clips table", drop.ID, n)
	}
	// And the row that was NOT deleted is still indexed, so the mutant "drop
	// everything from the index on any delete" does not pass either.
	if n := indexHits(t, s, "quetzal"); n != 1 {
		t.Fatalf("the surviving clip %d is no longer in the index (%d hits) — the delete took "+
			"more than its own row with it", keep.ID, n)
	}
}

// indexHits asks the FTS5 index itself, bypassing List entirely.
//
// The needle is a plain quoted phrase with NO trailing `*`: under trigram a
// prefix star still parses but means nothing, because a trigram match is already
// positionally unanchored. Measured over six needles — `"x"*` and `"x"` returned
// identical counts every time.
func indexHits(t *testing.T, s *Store, token string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM clips_fts WHERE clips_fts MATCH ?`, `"`+token+`"`,
	).Scan(&n); err != nil {
		t.Fatalf("probing clips_fts for %q: %v", token, err)
	}
	return n
}

// Invariant guard on the `clips_au` trigger. A dedup bump takes the DO UPDATE
// path, which fires AFTER UPDATE; without that trigger an external-content index
// accumulates a stale entry per bump.
func TestSearchStillFindsARowAfterADedupBump(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "zzyzx singular token", "a", 1000)
	put(t, s, "zzyzx singular token", "b", 2000)

	clips, err := s.List(context.Background(), 10, "zzyzx")
	if err != nil {
		t.Fatalf("List with q=zzyzx after a bump: %v", err)
	}
	if len(clips) != 1 {
		t.Fatalf("q=zzyzx returns %d rows after one bump, want exactly 1: %q — a bumped row is "+
			"indexed twice or not at all", len(clips), bodies(clips))
	}
}

// Invariant guard. Tokens are AND-ed, which is what "full-text search over
// bodies" means for a query of more than one word.
func TestSearchRequiresEveryTokenToMatch(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "alpha bravo", "a", 1000)
	put(t, s, "alpha charlie", "a", 2000)

	both, err := s.List(context.Background(), 10, "alpha bravo")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := bodies(both), []string{"alpha bravo"}; !equalStrings(got, want) {
		t.Fatalf("q=%q returned %q, want %q — tokens must be AND-ed, not OR-ed",
			"alpha bravo", got, want)
	}
	neither, err := s.List(context.Background(), 10, "alpha delta")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(neither) != 0 {
		t.Fatalf("q=%q returned %q, want nothing", "alpha delta", bodies(neither))
	}
}

// 🔴 CAN BE WATCHED FAIL — item 3 in this file's header. RED at base ref
// 2f5a7f89e with `q=assw returned 0 rows, want 1`. This test REPLACES
// TestSearchIsPrefixMatchingAndNotSubstringMatching, which asserted the exact
// opposite (`q=assw` must return 0) as a record of a known limit. The limit is
// gone, so the assertion is inverted rather than deleted.
//
// Fixtures: TWO rows, so a mutant that returns the whole table cannot pass, and
// the non-matching row shares no substring with the needle.
func TestSearchFindsASubstringMidWord(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "my github.com password is hunter2", "a", 1000)
	put(t, s, "a totally different clip", "b", 2000)

	for _, q := range []string{"assw", "hub", "ithub.com", "unter"} {
		got, err := s.List(context.Background(), 10, q)
		if err != nil {
			t.Fatalf("List q=%q: %v", q, err)
		}
		if got, want := bodies(got), []string{"my github.com password is hunter2"}; !equalStrings(got, want) {
			t.Fatalf("q=%q returned %q, want %q — a trigram MATCH is a genuine substring test",
				q, got, want)
		}
	}
	// A prefix still works; substring is a WIDENING, not a replacement.
	got, err := s.List(context.Background(), 10, "github")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("q=github returned %d rows, want 1 — the prefix case must not regress", len(got))
	}
}

// 🔴 CAN BE WATCHED FAIL — item 4 in this file's header. RED at base ref
// 2f5a7f89e with `q=hi returned 0 rows, want 1`.
//
// 🔴 THIS IS THE TEST THE WHOLE SCAN PATH EXISTS FOR, AND ITS FIXTURE IS CHOSEN
// SO IT CANNOT PASS BY ACCIDENT. `hi` is 2 runes — below MinTrigramRunes — and
// it is a substring of `this` but the PREFIX OF NO WORD in it, so neither the
// old unicode61 prefix index nor the new trigram index can answer it. Only the
// LIKE scan can. An earlier draft of this test used `q=is` against
// `this is a clip`, which PASSED AT BASE because `is` is a whole word there; that
// fixture would have reported the hazard as already-handled.
func TestATwoCharQueryDoesNotSilentlyReturnEmpty(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "this clip", "a", 1000)
	// 🔴 THE SECOND BODY MUST NOT CONTAIN THE NEEDLE, AND THE OBVIOUS CHOICE DID.
	// A first draft used "nothing relevant", which contains `hi` inside
	// "not-hi-ng" — the test failed with BOTH rows and looked like a code defect
	// for a minute. Checked by eye against every needle this test uses.
	put(t, s, "zebra fodder", "b", 2000)

	got, err := s.List(context.Background(), 10, "hi")
	if err != nil {
		t.Fatalf("List q=hi: %v", err)
	}
	if got, want := bodies(got), []string{"this clip"}; !equalStrings(got, want) {
		t.Fatalf("q=hi (2 runes) returned %q, want %q. A trigram index answers a sub-3-character "+
			"query with ZERO ROWS AND NO ERROR, which is indistinguishable from `nothing "+
			"matched`; PlanSearch must route it to the LIKE scan instead", got, want)
	}

	// A ONE-character query too, and a 2-rune CJK one — 6 bytes, so a
	// byte-length floor would wrongly send it to the index.
	s2 := newTestStore(t)
	put(t, s2, "東京都の天気", "a", 1000)
	put(t, s2, "unrelated latin", "b", 2000)
	for _, q := range []string{"東京", "京"} {
		got, err := s2.List(context.Background(), 10, q)
		if err != nil {
			t.Fatalf("List q=%q: %v", q, err)
		}
		if len(got) != 1 {
			t.Fatalf("q=%q (%d runes) returned %d rows, want 1 — the floor is counted in RUNES",
				q, len([]rune(q)), len(got))
		}
	}
}

// 🔴 CAN BE WATCHED FAIL — item 5 in this file's header. RED at base ref
// 2f5a7f89e with `q=%% returned 0 rows, want exactly 1` and `q=_ returned 0
// rows, want exactly 1`.
//
// 🔴 THE QUERIES HERE ARE ONE CHARACTER LONG *ON PURPOSE*, AND THAT IS THE WHOLE
// VALUE OF THIS TEST. `100%` is 4 runes, so it goes to the INDEX, where `%` is
// already a literal — a test using it cannot see a missing ESCAPE clause at all
// and would certify the escaping while exercising none of it. A bare `%` is 1
// rune, which forces the scan, which is the only path where `%` is a wildcard.
//
// Fixtures are chosen so the escaped and unescaped forms MUST produce different
// counts: `1000 miles away` contains `100` but not `100%`, and `axb not under`
// matches `%a_b%` only if `_` is a wildcard. Measured: unescaped 2 rows, escaped
// 1 row, for both characters.
func TestLikeWildcardsInAQueryAreLiteralsNotWildcards(t *testing.T) {
	cases := []struct {
		name    string
		bodies  [2]string
		q       string
		wantHit string
	}{
		{"a bare percent", [2]string{"100% certain", "1000 miles away"}, "%", "100% certain"},
		{"a bare underscore", [2]string{"a_b underscore", "axb not under"}, "_", "a_b underscore"},
		// `\` is the ESCAPE character itself: a dangling escape would capture the
		// following `%` and corrupt the pattern.
		{"a bare backslash", [2]string{`C:\Users\zach`, "no backslash here"}, `\`, `C:\Users\zach`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			put(t, s, tc.bodies[0], "a", 1000)
			put(t, s, tc.bodies[1], "b", 2000)

			// POSITIVE CONTROL: both rows are present and findable, so a 1-row
			// answer below is a filter working rather than a store with one row.
			all, err := s.List(context.Background(), 10, "")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(all) != 2 {
				t.Fatalf("the fixture holds %d rows, want 2 — the assertion below would be "+
					"vacuous", len(all))
			}

			got, err := s.List(context.Background(), 10, tc.q)
			if err != nil {
				t.Fatalf("List q=%q: %v", tc.q, err)
			}
			if got, want := bodies(got), []string{tc.wantHit}; !equalStrings(got, want) {
				t.Fatalf("q=%q returned %q, want exactly %q. BOTH failure directions mean the "+
					"same defect: 2 rows = the character acted as a LIKE wildcard (likeEscape "+
					"did not escape it), 0 rows = the pattern is corrupt (the `ESCAPE '\\'` "+
					"clause is missing, so the escaping backslash became literal text, or a "+
					"dangling escape swallowed the trailing `%%`)", tc.q, got, want)
			}
		})
	}
}

// Invariant guard on the INDEX path's own immunity to the same characters. It
// passed at base too (coincidentally, via unicode61's tokenisation), so it is
// labelled a guard and NOT counted as regression coverage.
func TestLikeWildcardsAreAlsoLiteralsOnTheIndexPath(t *testing.T) {
	// 🔴 THE SUBTEST NAME IS NOT THE QUERY, AND THAT IS DELIBERATE. `t.TempDir()`
	// derives its path from the subtest name, and OpenStore builds a `file:` URI
	// from that path — so a subtest literally named `100%` produced a path whose
	// `%` was percent-DECODED by the driver and SQLite answered "unable to open
	// database file (14)". Harmless here (the real mountPath is a fixed
	// `/data/clipstore.db`), but it is why these names are words.
	for _, tc := range []struct {
		name    string
		q       string
		bodies  [2]string
		wantHit string
	}{
		{"a percent", "100%", [2]string{"100% certain", "1000 miles away"}, "100% certain"},
		{"an underscore", "a_b", [2]string{"a_b underscore", "axb not under"}, "a_b underscore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if k := PlanSearch(tc.q).Kind; k != SearchIndex {
				t.Fatalf("PlanSearch(%q).Kind = %v, want SearchIndex — this test is supposed to "+
					"exercise the INDEX path; if it now plans a scan it is a duplicate of "+
					"TestLikeWildcardsInAQueryAreLiteralsNotWildcards and proves nothing new", tc.q, k)
			}
			s := newTestStore(t)
			put(t, s, tc.bodies[0], "a", 1000)
			put(t, s, tc.bodies[1], "b", 2000)
			got, err := s.List(context.Background(), 10, tc.q)
			if err != nil {
				t.Fatalf("List q=%q: %v", tc.q, err)
			}
			if got, want := bodies(got), []string{tc.wantHit}; !equalStrings(got, want) {
				t.Fatalf("q=%q returned %q, want %q", tc.q, got, want)
			}
		})
	}
}

// 🔴 CAN BE WATCHED FAIL — item 6 in this file's header. RED at base ref
// 2f5a7f89e with `q=---- returned 0 rows, want 1`.
//
// Under unicode61 a punctuation-only token was DISCARDED by the tokenizer, so
// FTSMatchExpr dropped it and List short-circuited to an empty result. Under
// trigram punctuation is INDEXED and such a query is a legitimate search.
func TestAPunctuationOnlyQueryIsARealSearch(t *testing.T) {
	cases := []struct {
		q      string
		bodies [2]string
		want   string
	}{
		// 4 runes -> the index path
		{"----", [2]string{"----", "no dashes here"}, "----"},
		// 2 runes -> the scan path, and mid-string so a prefix match could not find it
		{"--", [2]string{"a--b joined", "no dashes here"}, "a--b joined"},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			s := newTestStore(t)
			put(t, s, tc.bodies[0], "a", 1000)
			put(t, s, tc.bodies[1], "b", 2000)
			got, err := s.List(context.Background(), 10, tc.q)
			if err != nil {
				t.Fatalf("List q=%q errored: %v — it must be a SEARCH, not a failure", tc.q, err)
			}
			if got, want := bodies(got), []string{tc.want}; !equalStrings(got, want) {
				t.Fatalf("q=%q returned %q, want %q — trigram indexes punctuation, so dropping "+
					"such a token would silently discard a real constraint", tc.q, got, want)
			}
		})
	}
}

// 🔴 CAN BE WATCHED FAIL — item 7 in this file's header. RED at base ref
// 2f5a7f89e with `q="ASSW" returned 0 rows, want 1` (base could case-fold a
// prefix but had no substring to fold).
func TestSearchIsCaseInsensitiveForSubstrings(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "my GitHub.com Password", "a", 1000)
	put(t, s, "unrelated", "b", 2000)

	for _, q := range []string{"GITHUB", "github", "GiThUb", "ASSW", "assw", "aSSw"} {
		got, err := s.List(context.Background(), 10, q)
		if err != nil {
			t.Fatalf("List q=%q: %v", q, err)
		}
		if len(got) != 1 {
			t.Fatalf("q=%q returned %d rows, want 1 — matching folds ASCII case on both paths",
				q, len(got))
		}
	}
	// And on the SCAN path (2 runes), which folds case independently of the index.
	for _, q := range []string{"GI", "gi", "Hu", "hU"} {
		got, err := s.List(context.Background(), 10, q)
		if err != nil {
			t.Fatalf("List q=%q: %v", q, err)
		}
		if len(got) != 1 {
			t.Fatalf("q=%q (scan path) returned %d rows, want 1", q, len(got))
		}
	}
}

// CHARACTERIZATION TEST of the one MEASURED DIVERGENCE between the two paths,
// recorded as behaviour rather than left to be discovered.
//
// Trigram case-folds BEYOND ASCII; SQLite's LIKE folds ASCII only. So the same
// needle can be found via the index and missed via the scan. This is not a
// preference — it is what the engine does, and PlanSearch's comment carries the
// measurement. If it is ever fixed, this test must be UPDATED, not deleted.
func TestNonASCIICaseFoldingDivergesBetweenTheTwoPaths(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "Ärger über Äpfel", "a", 1000)
	put(t, s, "unrelated latin text", "b", 2000)

	// The index path folds Ä -> ä.
	if k := PlanSearch("ärger").Kind; k != SearchIndex {
		t.Fatalf("PlanSearch(%q).Kind = %v, want SearchIndex", "ärger", k)
	}
	viaIndex, err := s.List(context.Background(), 10, "ärger")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(viaIndex) != 1 {
		t.Fatalf("q=ärger via the index returned %d rows, want 1 — trigram folds non-ASCII case",
			len(viaIndex))
	}

	// The scan path does NOT. `ärger ab` routes to the scan because `ab` is
	// short; `ab` IS present in `Äpfel`? No — so use a needle that is.
	if k := PlanSearch("ärger üb").Kind; k != SearchScan {
		t.Fatalf("PlanSearch(%q).Kind = %v, want SearchScan", "ärger üb", k)
	}
	viaScan, err := s.List(context.Background(), 10, "ärger üb")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(viaScan) != 0 {
		t.Fatalf("q=%q via the scan returned %d rows, want 0. SQLite's LIKE folds ASCII ONLY, "+
			"so a lower-case ä does not match the body's Ä. If this now returns 1 the engine or "+
			"the implementation changed and PlanSearch's documented divergence table is stale",
			"ärger üb", len(viaScan))
	}
	// CONTROL: with the body's own capitalisation the scan path DOES find it, so
	// the zero above is a case-folding fact and not a broken query.
	control, err := s.List(context.Background(), 10, "Ärger üb")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(control) != 1 {
		t.Fatalf("q=%q via the scan returned %d rows, want 1 — this is the control that proves "+
			"the scan path works at all, so the 0 above means case folding and nothing else",
			"Ärger üb", len(control))
	}
}

// Invariant guard, and the one that matters most for safety: no input may reach
// the FTS5 grammar, and none may reach LIKE's wildcards either.
//
// 🔴 EXPECTED STRINGS ARE LITERAL, NEVER DERIVED FROM THE IMPLEMENTATION. The
// point of this test is the exact wire form of the expression and the patterns;
// computing either from PlanSearch's own logic would assert that the function
// equals itself.
//
// 🔴 IT ASSERTS THE PLAN'S *KIND* AS WELL AS ITS TEXT, AND THAT IS NOT
// DECORATION. The two execution paths return the same ROWS for every query whose
// tokens are all long enough, so a mutant that sent a 3-rune token down the scan
// instead of the index is behaviourally INVISIBLE — same results, index never
// used. Only an assertion on Kind can see it. (Mutation-proved: `<
// MinTrigramRunes` -> `< 4` survives every row-level test in this file and dies
// here.)
func TestPlanSearchQuotesEveryTokenSoNoInputReachesEitherGrammar(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantKind SearchKind
		wantMch  string
		wantLike []string
	}{
		// ---- the index path: every token >= MinTrigramRunes runes ----
		{"one token", "hunter2", SearchIndex, `"hunter2"`, nil},
		{"two tokens are AND-ed", "ssh prod", SearchIndex, `"ssh" AND "prod"`, nil},
		{"an embedded quote is doubled", `say "hid"`, SearchIndex, `"say" AND """hid"""`, nil},
		// 🔴 `AND`, NOT `OR`, AND THE REASON IS WORTH KEEPING: `AND` is the
		// operator this function itself emits between tokens, so a token spelled
		// `AND` is the one that could actually escape the quoting and change the
		// expression's meaning. It is also 3 runes, so it stays on the index
		// path — `OR` is 2 runes and routes to the scan (asserted separately
		// below), which would have tested nothing about the FTS5 grammar.
		{"the AND operator is literalised", "aaa AND bbb", SearchIndex,
			`"aaa" AND "AND" AND "bbb"`, nil},
		{"NOT is literalised", "aaa NOT bbb", SearchIndex, `"aaa" AND "NOT" AND "bbb"`, nil},
		{"a column filter is literalised", "body:x", SearchIndex, `"body:x"`, nil},
		{"a NEAR call is literalised", "NEAR(aa bb)", SearchIndex,
			`"NEAR(aa" AND "bb)"`, nil},
		{"a mixed token survives", "foo-bar", SearchIndex, `"foo-bar"`, nil},
		{"digits count as content", "2026", SearchIndex, `"2026"`, nil},
		// 🔴 PUNCTUATION IS NO LONGER DROPPED — trigram indexes it. The old
		// FTSMatchExpr returned ok=false here.
		{"a punctuation-only token is a real search", "---", SearchIndex, `"---"`, nil},
		{"no trailing star", "password", SearchIndex, `"password"`, nil},
		// exactly at the floor: 3 runes goes to the INDEX
		{"three runes is the index", "abc", SearchIndex, `"abc"`, nil},
		{"three CJK runes is the index", "東京都", SearchIndex, `"東京都"`, nil},

		// ---- the scan path: ANY token shorter than MinTrigramRunes ----
		{"two runes is a scan", "ab", SearchScan, "", []string{`%ab%`}},
		{"one rune is a scan", "a", SearchScan, "", []string{`%a%`}},
		// 🔴 RUNES, NOT BYTES: 2 CJK characters are 6 bytes.
		{"two CJK runes is a scan", "東京", SearchScan, "", []string{`%東京%`}},
		{"one short token drags the WHOLE query to the scan", "password ab", SearchScan, "",
			[]string{`%password%`, `%ab%`}},
		{"a percent is escaped", "%", SearchScan, "", []string{`%\%%`}},
		{"an underscore is escaped", "_", SearchScan, "", []string{`%\_%`}},
		{"a backslash is escaped", `\`, SearchScan, "", []string{`%\\%`}},
		{"a backslash before a percent does not run away", `\%`, SearchScan, "",
			[]string{`%\\\%%`}},
		{"a short punctuation token is a real search", "--", SearchScan, "", []string{`%--%`}},
		// `OR` is 2 runes, so an FTS5 operator can also arrive on the scan path.
		{"a 2-rune FTS5 operator routes to the scan", "aaa OR bbb", SearchScan, "",
			[]string{`%aaa%`, `%OR%`, `%bbb%`}},
		{"quotes need no doubling on the scan path", `"x`, SearchScan, "", []string{`%"x%`}},

		// ---- no query at all ----
		{"blank is the whole list", "   ", SearchAll, "", nil},
		{"empty is the whole list", "", SearchAll, "", nil},
		{"a tab and a newline are still blank", "\t\n", SearchAll, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanSearch(tc.in)
			if got.Kind != tc.wantKind {
				t.Fatalf("PlanSearch(%q).Kind = %v, want %v", tc.in, got.Kind, tc.wantKind)
			}
			if got.Match != tc.wantMch {
				t.Fatalf("PlanSearch(%q).Match = %q, want %q", tc.in, got.Match, tc.wantMch)
			}
			if !equalStrings(got.Like, tc.wantLike) {
				t.Fatalf("PlanSearch(%q).Like = %q, want %q", tc.in, got.Like, tc.wantLike)
			}
		})
	}
}

// Invariant guard, WITH ITS REASON REWRITTEN because the old one is no longer
// true. This test used to be justified by "a query made only of characters the
// tokenizer discards can match nothing, ever", and List short-circuited such a
// query to `[]Clip{}` without running anything. Under trigram punctuation is
// indexed, there is no such query, and the short-circuit is gone — so `--- !!`
// now runs a REAL scan (`!!` is 2 runes) that genuinely finds nothing in this
// fixture. The assertion is unchanged; what it proves is different, and saying
// so is the point.
func TestAQueryThatMatchesNothingIsAnEmptyResultNotAnError(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "something", "a", 1000)

	for _, q := range []string{"--- !!", "zzzznotthere", "zz", "nope nope"} {
		clips, err := s.List(context.Background(), 10, q)
		if err != nil {
			t.Fatalf("List q=%q errored: %v — it must be an empty RESULT, not a failure", q, err)
		}
		if len(clips) != 0 {
			t.Fatalf("q=%q matched %q, want nothing", q, bodies(clips))
		}
		if clips == nil {
			t.Fatalf("List q=%q returned a nil slice; it must return an empty one so JSON "+
				"renders [] not null", q)
		}
	}
	// POSITIVE CONTROL. Every assertion above is a ZERO, which is
	// indistinguishable from a query wired to nothing. This is the pair: a needle
	// that MUST be found, through the same List call on the same store.
	hit, err := s.List(context.Background(), 10, "ethi")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hit) != 1 {
		t.Fatalf("the positive control q=ethi returned %d rows, want 1 — without it the zeros "+
			"above would prove only that this store answers nothing to anything", len(hit))
	}
}

// 🔴 CAN BE WATCHED FAIL — item 8 in this file's header. RED at base ref
// 2f5a7f89e with `q="lph br" returned 0 rows, want 1`.
//
// Tokens are AND-ed on BOTH paths, and each must match as a SUBSTRING. Routing
// is asserted explicitly so this test cannot silently stop covering the path it
// names.
func TestEveryTokenMustMatchAsASubstringOnBothPaths(t *testing.T) {
	cases := []struct {
		q        string
		wantKind SearchKind
		wantHits []string
	}{
		// all tokens >= 3 runes -> index
		{"lpha ravo", SearchIndex, []string{"alpha bravo"}},
		{"lpha harlie", SearchIndex, []string{"alpha charlie"}},
		{"lpha elta", SearchIndex, nil},
		// a short token drags the whole query to the scan
		{"lph br", SearchScan, []string{"alpha bravo"}},
		{"lph ch", SearchScan, []string{"alpha charlie"}},
		{"lph de", SearchScan, nil},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			s := newTestStore(t)
			put(t, s, "alpha bravo", "a", 1000)
			put(t, s, "alpha charlie", "a", 2000)

			if k := PlanSearch(tc.q).Kind; k != tc.wantKind {
				t.Fatalf("PlanSearch(%q).Kind = %v, want %v — this case is meant to exercise "+
					"that path", tc.q, k, tc.wantKind)
			}
			got, err := s.List(context.Background(), 10, tc.q)
			if err != nil {
				t.Fatalf("List q=%q: %v", tc.q, err)
			}
			if want := tc.wantHits; !equalStrings(bodies(got), want) {
				t.Fatalf("q=%q returned %q, want %q — tokens must be AND-ed, not OR-ed",
					tc.q, bodies(got), want)
			}
		})
	}
}

// 🔴 CAN BE WATCHED FAIL — item 9 in this file's header. RED at base ref
// 2f5a7f89e with `q=assw after a dedup bump returned 0 rows, want exactly 1`.
//
// POST's upsert takes the `DO UPDATE` path on every re-capture, which fires the
// `clips_au` AFTER UPDATE trigger. This is the substring-era version of
// TestSearchStillFindsARowAfterADedupBump: it pins that the trigger keeps the
// TRIGRAM index correct across a bump, and that the bump does not leave a second
// index entry (which would return the row twice).
func TestTheUpdateTriggerKeepsTheTrigramIndexCorrect(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "my github.com password", "a", 1000)
	put(t, s, "an unrelated clip", "b", 1500)
	// Re-capture: AFTER UPDATE.
	put(t, s, "my github.com password", "c", 2000)

	got, err := s.List(context.Background(), 10, "assw")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := bodies(got), []string{"my github.com password"}; !equalStrings(got, want) {
		t.Fatalf("q=assw after a dedup bump returned %q, want exactly %q — more than one row "+
			"means the bump left a stale trigram entry behind (the clips_au trigger)", got, want)
	}
	// Interrogate the index DIRECTLY, which is the only surface where a stale
	// entry is visible: List's subquery filters duplicates out through clips.
	if n := indexHits(t, s, "assw"); n != 1 {
		t.Fatalf("the trigram index holds %d entries for `assw` after one bump, want 1", n)
	}
	// Every trigger, through the substring path: INSERT (above), UPDATE (above),
	// DELETE (here).
	all, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := s.Delete(context.Background(), all[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n := indexHits(t, s, "assw"); n != 0 {
		t.Fatalf("the trigram index still holds %d entries for a deleted clip's body", n)
	}
}

// 🔴 CAN BE WATCHED FAIL — item 10 in this file's header, and it is the ONLY
// test that covers the migration.
//
// 🔴 WHAT IT PROVES AND WHY NOTHING ELSE CAN. `CREATE VIRTUAL TABLE IF NOT
// EXISTS` DOES NOT ALTER AN EXISTING TABLE — it is a no-op that returns NO
// ERROR. So against a database that already holds a unicode61 `clips_fts`, the
// whole trigram change is silently inert while every other test in this file,
// all of which run on a FRESH temp file, passes. This test is the only one that
// builds the pre-change state, and it builds it BY HAND rather than from
// `schema`, because `schema` no longer can.
func TestOpenStoreMigratesAUnicode61IndexToTrigram(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clipstore.db")

	// Build the PRE-CHANGE database: the old unicode61 clips_fts, with rows.
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	legacy.SetMaxOpenConns(1)
	if _, err := legacy.Exec(`
		CREATE TABLE clips (
		  id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL,
		  body_sha256 TEXT NOT NULL UNIQUE, device TEXT NOT NULL,
		  created_at INTEGER NOT NULL, bump_count INTEGER NOT NULL DEFAULT 0);
		CREATE VIRTUAL TABLE clips_fts USING fts5 (
		  body, content='clips', content_rowid='id');
		CREATE TRIGGER clips_ai AFTER INSERT ON clips BEGIN
		  INSERT INTO clips_fts (rowid, body) VALUES (new.id, new.body);
		END;`); err != nil {
		t.Fatalf("build the legacy schema: %v", err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO clips (body, body_sha256, device, created_at) VALUES (?,?,?,?)`,
		"my github.com password", BodyHash("my github.com password"), "old-device", 1000); err != nil {
		t.Fatalf("seed a legacy row: %v", err)
	}
	// NEGATIVE CONTROL: the legacy index genuinely CANNOT do substrings. If this
	// ever returns 1, the fixture is not the pre-change state and everything
	// below is vacuous.
	var before int
	if err := legacy.QueryRow(
		`SELECT count(*) FROM clips_fts WHERE clips_fts MATCH ?`, `"assw"`).Scan(&before); err != nil {
		t.Fatalf("probe the legacy index: %v", err)
	}
	if before != 0 {
		t.Fatalf("the legacy unicode61 index answered %d rows for a substring, want 0 — this "+
			"fixture is not the pre-change state, so this test proves nothing", before)
	}
	// POSITIVE CONTROL on the same probe: it CAN see a prefix, so the 0 above is
	// a property of substrings and not of a probe wired to nothing.
	var beforePrefix int
	if err := legacy.QueryRow(
		`SELECT count(*) FROM clips_fts WHERE clips_fts MATCH ?`, `"github"`).Scan(&beforePrefix); err != nil {
		t.Fatalf("probe the legacy index: %v", err)
	}
	if beforePrefix != 1 {
		t.Fatalf("the legacy probe answered %d for a prefix it must find, want 1 — the probe is "+
			"wired to nothing", beforePrefix)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Now open it the way the service does.
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore over a legacy database: %v", err)
	}
	defer s.Close()

	// 1. The stored DDL now asks for trigram.
	var ddl string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='clips_fts'`).Scan(&ddl); err != nil {
		t.Fatalf("read the DDL: %v", err)
	}
	if !ddlDeclaresTrigram(ddl) {
		t.Fatalf("after OpenStore the clips_fts DDL is %q, which does not declare trigram. "+
			"`CREATE VIRTUAL TABLE IF NOT EXISTS` is a silent no-op over an existing table, so "+
			"without ensureTrigramIndex the service keeps prefix semantics forever", ddl)
	}

	// 2. Substring search WORKS on the pre-existing row — the behaviour, not the DDL.
	got, err := s.List(context.Background(), 10, "assw")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := bodies(got), []string{"my github.com password"}; !equalStrings(got, want) {
		t.Fatalf("after the migration q=assw returned %q, want %q", got, want)
	}

	// 3. The REBUILD LOST NOTHING: the row and its metadata are intact.
	all, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("the clips table holds %d rows after the migration, want 1 — a rebuild must "+
			"not touch the content table", len(all))
	}
	if all[0].Device != "old-device" || all[0].CreatedAt != 1000 {
		t.Fatalf("the migrated row is %+v, want device=old-device created_at=1000", all[0])
	}

	// 4. The backup's FTS5 integrity check still passes on the rebuilt index.
	if _, err := s.db.Exec(
		`INSERT INTO clips_fts(clips_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		t.Fatalf("the backup's FTS5 integrity-check (rank=1) failed after the migration: %v", err)
	}
}

// Invariant guard. The migration must be IDEMPOTENT — it must not drop and
// rebuild the index on every start, which would be invisible behaviourally
// (results identical) while making every restart O(rows).
//
// 🔴 IT OBSERVES THE REBUILD DIRECTLY via ensureTrigramIndex's bool, because the
// ROWS cannot see the difference: a needlessly rebuilt index returns exactly the
// same answers as one left alone.
func TestEnsureTrigramIndexIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "my github.com password", "a", 1000)

	// A fresh store is already trigram, so the FIRST call must not rebuild.
	rebuilt, err := ensureTrigramIndex(s.db)
	if err != nil {
		t.Fatalf("ensureTrigramIndex: %v", err)
	}
	if rebuilt {
		t.Fatal("ensureTrigramIndex rebuilt the index of a FRESH store, which `schema` already " +
			"created as trigram — every restart would pay a full reindex")
	}
	// POSITIVE CONTROL: it DOES rebuild when the index is not trigram, so the
	// false above is a decision and not a function that never rebuilds.
	if _, err := s.db.Exec(`DROP TABLE clips_fts;
		CREATE VIRTUAL TABLE clips_fts USING fts5 (body, content='clips', content_rowid='id');
		INSERT INTO clips_fts(clips_fts) VALUES('rebuild');`); err != nil {
		t.Fatalf("install a unicode61 index: %v", err)
	}
	rebuilt, err = ensureTrigramIndex(s.db)
	if err != nil {
		t.Fatalf("ensureTrigramIndex: %v", err)
	}
	if !rebuilt {
		t.Fatal("ensureTrigramIndex did NOT rebuild a unicode61 index — the control proving it " +
			"can rebuild at all, so the idempotence assertion above is vacuous without it")
	}
	// And it is settled afterwards.
	rebuilt, err = ensureTrigramIndex(s.db)
	if err != nil {
		t.Fatalf("ensureTrigramIndex: %v", err)
	}
	if rebuilt {
		t.Fatal("ensureTrigramIndex rebuilt twice in a row")
	}
}

// Invariant guard on ddlDeclaresTrigram's FAIL-SAFE DIRECTION, which is the
// property that matters: an unrecognised spelling must read as NOT trigram
// (costing one lossless rebuild) rather than as trigram (silently keeping a
// unicode61 index).
func TestDDLDeclaresTrigramFailsSafeTowardsRebuilding(t *testing.T) {
	trigram := []string{
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, content='clips', tokenize='trigram')`,
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, tokenize = "trigram")`,
		`create virtual table clips_fts using fts5 (body, TOKENIZE=TRIGRAM)`,
		"CREATE VIRTUAL TABLE clips_fts USING fts5 (\n  body,\n  tokenize='trigram'\n)",
	}
	for _, ddl := range trigram {
		if !ddlDeclaresTrigram(ddl) {
			t.Errorf("ddlDeclaresTrigram(%q) = false, want true", ddl)
		}
	}
	notTrigram := []string{
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, content='clips', content_rowid='id')`,
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, tokenize='unicode61')`,
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, tokenize='porter')`,
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, tokenize='ascii')`,
		// a column literally NAMED trigram is not a tokenizer declaration
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (trigram, content='clips')`,
		``,
	}
	for _, ddl := range notTrigram {
		if ddlDeclaresTrigram(ddl) {
			t.Errorf("ddlDeclaresTrigram(%q) = true, want false — reading an unfamiliar "+
				"spelling as `already trigram` SKIPS the migration, which is the expensive "+
				"direction to be wrong in", ddl)
		}
	}
}

// 🔴 SCHEMA LEDGER. Nothing in this repository reads clipstore.db's schema
// from outside today. This pins it for the phase-6 backup cross-check, when the
// maintainer's deployment moves onto this repo (docs/PLAN.md phase 6) and its
// backup verifier checks against it: a verifier has to know which tables and
// triggers a healthy database carries, and the triggers are the ENTIRE synchronisation
// mechanism between `clips` and its FTS5 index, so a restore that loses one
// leaves search silently stale. The exact object set is pinned as LITERALS, in
// BOTH directions: a set that GROWS (a new shadow table, a new sync trigger) is
// as much a contract change as one that shrinks, and it must be made here on
// purpose — and mirrored in that verifier once it exists.
//
// The expected sets are written out rather than read back from sqlite_master,
// because a ledger derived from the thing it checks asserts that SQLite equals
// itself. Measured when the tokenizer moved to trigram: trigram and unicode61
// produce an IDENTICAL shadow-table set, and `('integrity-check', 1)` runs
// clean under both.
func TestTheSchemaLedgerIsExactlyTheseObjects(t *testing.T) {
	wantTables := map[string]bool{
		"clips": true, "clips_fts": true, "clips_fts_config": true,
		"clips_fts_data": true, "clips_fts_docsize": true, "clips_fts_idx": true,
		// The change-feed counter (one row; see ensureSeq).
		"clip_seq": true,
	}
	wantTriggers := map[string]bool{"clips_ai": true, "clips_ad": true, "clips_au": true}

	s := newTestStore(t)
	put(t, s, "a clip so the index is not empty", "a", 1000)

	gotTables := schemaNames(t, s, "table")
	gotTriggers := schemaNames(t, s, "trigger")
	// sqlite_sequence is SQLite's own AUTOINCREMENT bookkeeping, not part of the
	// schema this service declares.
	delete(gotTables, "sqlite_sequence")

	assertExactSet(t, "table", wantTables, gotTables)
	assertExactSet(t, "trigger", wantTriggers, gotTriggers)

	// FTS5's own integrity instrument, on the real trigram index. It raises
	// rather than returning a verdict, so reaching the next line IS the pass.
	if _, err := s.db.Exec(
		`INSERT INTO clips_fts(clips_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		t.Fatalf("FTS5 integrity-check (rank=1) failed on a healthy trigram index: %v", err)
	}
}

// schemaNames reads every sqlite_master object of one type.
func schemaNames(t *testing.T, s *Store, objType string) map[string]bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = ?`, objType)
	if err != nil {
		t.Fatalf("read sqlite_master for type=%q: %v", objType, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("the live schema has NO objects of type %q. This query is the instrument the "+
			"set comparison reads; an empty result would make every assertion below fire "+
			"for the wrong reason", objType)
	}
	return out
}

// assertExactSet fails in BOTH directions.
func assertExactSet(t *testing.T, objType string, want, got map[string]bool) {
	t.Helper()
	var missing, extra []string
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	for name := range got {
		if !want[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("the schema ledger requires %s(s) %v, which the live schema does NOT create",
			objType, missing)
	}
	if len(extra) > 0 {
		t.Errorf("the live schema creates %s(s) %v that the ledger does not list — a schema "+
			"change is a contract change for anything that verifies a backup; add it to the "+
			"ledger in the same change", objType, extra)
	}
}

// 🔴 THE GUARD THAT MAKES Store.Ping's DOCUMENTED PROPERTY TRUE, and the one
// that was missing: mutating Ping's body to `return s.db.PingContext(ctx)` —
// the exact simplification handlers.go forbids — SURVIVED the whole suite at
// pass=149 fail=0. The test that was cited against it uses the failingStore
// fake, so it could only ever prove the HANDLER turns an error into a 503;
// nothing proved the real Store.Ping ever errors for anything.
//
// The property asserted here is the narrow true one, not the retracted wide one
// (see Store.Ping's comment for the four file-level states it does NOT detect):
// the statement is resolved against the SCHEMA, so the `clips` table must be
// present and queryable.
//
// Invariant guard, not regression coverage: store.go is new and nothing has
// shipped. What it pins is the difference between the shipped implementation and
// the one-line alternative a future reader will reach for.
func TestStorePingReadsTheClipsTableNotTheConnection(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "a clip so the table is not empty", "d", 1000)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a healthy store: %v — the fixture is broken before the "+
			"assertion is reached", err)
	}

	// Remove the object the statement NAMES, leaving the connection and the file
	// perfectly intact. Nothing earlier in Ping can reject this: Ping is one
	// statement with no guards in front of it.
	if _, err := s.db.Exec(`DROP TABLE clips`); err != nil {
		t.Fatalf("DROP TABLE clips: %v", err)
	}

	// 🔴 THE DISCRIMINATOR, AND IT COMES FIRST BECAUSE IT IS WHAT MAKES THE NEXT
	// ASSERTION MEAN ANYTHING. db.PingContext must still answer nil in exactly
	// this state. If it ever errors here, this test would pass for a
	// db.PingContext implementation too and would be asserting nothing — so that
	// is a failure of the TEST, reported as one.
	if err := s.db.PingContext(context.Background()); err != nil {
		t.Fatalf("db.PingContext errored (%v) in the state this test uses to tell a "+
			"schema-touching Ping apart from a connection-liveness Ping. The two "+
			"implementations are no longer distinguishable here, so this guard no longer "+
			"discriminates — find a state where db.PingContext still answers nil", err)
	}

	err := s.Ping(context.Background())
	if err == nil {
		t.Fatalf("Store.Ping returned nil with the `clips` table DROPPED, while " +
			"db.PingContext also returns nil here. Ping is not reading the schema at all, so " +
			"/healthz would answer 200 for a store that cannot serve a single request — and " +
			"store.go's and handlers.go's comments both claim the opposite. Ping must run a " +
			"statement that NAMES a table")
	}
	if !strings.Contains(err.Error(), "clips") {
		t.Errorf("Ping failed with %q, which does not name the missing table. The probe's "+
			"failure is read out of a kubelet event, so the message has to say what was "+
			"unreadable", err)
	}
}

// 🔴 THE GUARD ON THE MIGRATION'S ATOMICITY. ensureTrigramIndex used to run
// DROP + CREATE + rebuild as three statements in ONE unprotected db.Exec, and
// the half-way state is the worst kind: the stored DDL already says trigram, so
// ddlDeclaresTrigram's fail-safe direction cannot help, the next start returns
// (false, nil), and the service serves an EMPTY search index forever with no
// error anywhere. Measured in that state: `q=assw` returned 0 rows and no error
// while the unfiltered list still returned its row.
//
// 🔴 HOW THE FAILURE IS REACHED WITHOUT MOCKING ANYTHING. The fixture is a
// database with a unicode61 clips_fts — so the migration is genuinely triggered,
// not short-circuited by ddlDeclaresTrigram — and NO `clips` table, so FTS5's
// `('rebuild')` must fail: an external-content rebuild reads its content table.
// Measured: `SQL logic error: no such table: main.clips`. That is the LAST of
// the three statements, which is precisely the ordering that leaves the DROP and
// the CREATE already landed.
//
// Invariant guard. store.go is new, so this is not regression coverage for a
// shipped bug — it is the guard that keeps the "a drop and rebuild loses
// nothing" claim in ensureTrigramIndex's own comment true.
func TestAFailedTrigramMigrationLeavesNoHalfMigratedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clipstore.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(
		`CREATE VIRTUAL TABLE clips_fts USING fts5 (body, content='clips', content_rowid='id')`,
	); err != nil {
		t.Fatalf("build the legacy unicode61 index: %v", err)
	}

	// Premise control: the migration must actually be reachable, or every
	// assertion below is vacuous.
	before := storedFTSDDL(t, db)
	if ddlDeclaresTrigram(before) {
		t.Fatalf("the fixture's clips_fts already declares trigram (%q), so "+
			"ensureTrigramIndex would short-circuit and this test would assert nothing",
			before)
	}

	rebuilt, err := ensureTrigramIndex(db)
	if err == nil {
		t.Fatalf("ensureTrigramIndex reported success on a database whose rebuild step CANNOT " +
			"succeed (no `clips` content table). The fixture no longer reaches the failure " +
			"path, so the atomicity assertion below is unreachable")
	}
	if rebuilt {
		t.Errorf("ensureTrigramIndex returned rebuilt=true alongside an error (%v); a failed "+
			"migration must not be logged as a completed one", err)
	}

	// THE ASSERTION. A failed migration must leave the OLD index in place.
	after := storedFTSDDL(t, db)
	if ddlDeclaresTrigram(after) {
		t.Fatalf("a FAILED migration left a TRIGRAM clips_fts DDL on disk (%q). The DROP and "+
			"the CREATE landed and the rebuild did not, so the index is empty while the DDL "+
			"claims trigram — the next start reads `already trigram`, returns (false, nil), and "+
			"the service serves an empty search index forever with no error anywhere. Wrap the "+
			"DROP/CREATE/rebuild in ONE transaction so this rolls back", after)
	}

	// And it must stay LOUD: the next start has to retry and fail again, not
	// quietly decide there is nothing to do.
	if _, err := ensureTrigramIndex(db); err == nil {
		t.Fatalf("the SECOND ensureTrigramIndex on the same unchanged database returned nil. " +
			"A once-loud failure has become a permanent silent one, which is the exact failure " +
			"class this file is written against")
	}
}

func storedFTSDDL(t *testing.T, db *sql.DB) string {
	t.Helper()
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='clips_fts'`,
	).Scan(&ddl); err != nil {
		t.Fatalf("read the stored clips_fts DDL: %v", err)
	}
	return ddl
}

// 🔴 SEAM GUARD, not a component test. OpenStore's FTS5 control and trigram's
// 3-character floor are owned by different lines, and nothing else pins the
// RELATIONSHIP between them: under trigram a needle shorter than
// MinTrigramRunes returns 0 rows with no error WHATEVER the index's state, so a
// short needle would turn that control from weak into vacuous — it could not
// fail even with a completely broken index.
func TestTheStartupProbeNeedleIsLongEnoughForTrigram(t *testing.T) {
	if n := len([]rune(startupProbeNeedle)); n < MinTrigramRunes {
		t.Fatalf("startupProbeNeedle is %d runes (%q), want at least MinTrigramRunes=%d — "+
			"OpenStore's FTS5 probe would return 0 unconditionally and could never fail",
			n, startupProbeNeedle, MinTrigramRunes)
	}
	// And it must still be a needle no real clip contains, or the probe would
	// start matching user data.
	if strings.TrimSpace(startupProbeNeedle) == "" {
		t.Fatal("startupProbeNeedle is blank")
	}
	// It must route to the INDEX, since that is the surface it is controlling.
	if k := PlanSearch(startupProbeNeedle).Kind; k != SearchIndex {
		t.Fatalf("PlanSearch(startupProbeNeedle).Kind = %v, want SearchIndex", k)
	}
}

// Invariant guard. Chronological newest-first is requirement 1.
func TestListIsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "oldest", "a", 1000)
	put(t, s, "newest", "a", 3000)
	put(t, s, "middle", "a", 2000)

	clips, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := bodies(clips), []string{"newest", "middle", "oldest"}; !equalStrings(got, want) {
		t.Fatalf("List returned %q, want %q", got, want)
	}
}

// Invariant guard. `limit` is the only growth bound this service has, so it has
// to be the newest N — not an arbitrary N.
func TestListHonoursTheLimitAndTakesTheNewest(t *testing.T) {
	s := newTestStore(t)
	for i := 1; i <= 7; i++ {
		put(t, s, strings.Repeat("x", i), "a", int64(i*1000))
	}
	clips, err := s.List(context.Background(), 3, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(clips) != 3 {
		t.Fatalf("List(limit=3) returned %d rows", len(clips))
	}
	// Bounds that OVERSHOOT the step: 7 rows and a limit of 3 means the expected
	// set is {7,6,5} — no multiple of the limit lands on the boundary, so a
	// mutant that reads the OLDEST three, or that uses a sort it did not mean to,
	// produces a different answer rather than the same one by coincidence.
	want := []string{strings.Repeat("x", 7), strings.Repeat("x", 6), strings.Repeat("x", 5)}
	if got := bodies(clips); !equalStrings(got, want) {
		t.Fatalf("List(limit=3) returned %q, want the NEWEST three %q", got, want)
	}
}

// Invariant guard on the "never empty on error" posture at the STORE layer: an
// empty store is an empty SLICE and a successful call, because a store with
// nothing in it is a legitimate state and must be distinguishable from one that
// could not be read.
func TestListOfAnEmptyStoreIsAnEmptySliceAndNoError(t *testing.T) {
	s := newTestStore(t)
	clips, err := s.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List on an empty store errored: %v", err)
	}
	if clips == nil {
		t.Fatal("List returned nil; it must return an empty slice so JSON renders [] not null")
	}
	if len(clips) != 0 {
		t.Fatalf("List on an empty store returned %d rows", len(clips))
	}
}

// Invariant guard. WAL is a durability requirement here (see OpenStore), so the
// pragma being applied is worth asserting rather than trusting the DSN spelling.
func TestOpenStoreSetsWALJournalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clipstore.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	var mode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode is %q, want wal — an abrupt stop (strategy: Recreate) is only "+
			"crash-safe under WAL", mode)
	}
}

// Invariant guard. A wrong mountPath or a read-only volume must be a STARTUP
// failure — CrashLoopBackOff is visible; a 503 per request is not.
func TestOpenStoreRefusesAnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file should be: SQLite cannot open it, and
	// no amount of retrying will change that.
	blocked := filepath.Join(dir, "clipstore.db")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := OpenStore(blocked); err == nil {
		t.Fatal("OpenStore accepted a path that is a directory; it must refuse at startup")
	}
}

// Invariant guard. The dedup key is a hash of the body and nothing else — not
// the device, not the timestamp — so the same text from two devices is one clip.
func TestBodyHashIsOverTheBodyAlone(t *testing.T) {
	// A literal, not a computed value: sha256("") is a published constant, so
	// this pins the algorithm and the encoding rather than restating the code.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := BodyHash(""); got != emptySHA256 {
		t.Fatalf("BodyHash(%q) = %q, want %q", "", got, emptySHA256)
	}
	if BodyHash("a") == BodyHash("b") {
		t.Fatal("BodyHash collides on two different bodies")
	}
	if BodyHash("a") != BodyHash("a") {
		t.Fatal("BodyHash is not deterministic")
	}
}

// Invariant guard. The UNIQUE constraint is what makes the one-statement upsert
// possible; if it were dropped the upsert would silently become an append-only
// INSERT and dedup would stop existing with every other test still green except
// the two that assert row counts.
func TestBodySHA256IsUnique(t *testing.T) {
	s := newTestStore(t)
	put(t, s, "x", "a", 1000)
	_, err := s.db.Exec(
		`INSERT INTO clips (body, body_sha256, device, created_at) VALUES (?, ?, ?, ?)`,
		"x", BodyHash("x"), "b", 2000)
	if err == nil {
		t.Fatal("a second row with the same body_sha256 was accepted; the UNIQUE index is gone")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("insert of a duplicate hash failed with %v, which does not name a UNIQUE "+
			"violation — the row may have been rejected for an unrelated reason, which would "+
			"make this test green for the wrong reason", err)
	}
}

// Invariant guard. Ping must touch a page, not just report a live pool handle.
func TestPingFailsOnAClosedStore(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a healthy store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against a closed store")
	} else if !errors.Is(err, sql.ErrConnDone) && !strings.Contains(err.Error(), "closed") {
		t.Logf("Ping error is %v (not sql.ErrConnDone); the assertion is only that it FAILS", err)
	}
}

func equalStrings(a, b []string) bool {
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
