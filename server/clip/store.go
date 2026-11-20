package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Clip is one row of the shared clipboard.
//
// CreatedAt is unix MILLIseconds, not seconds: two captures inside one second
// are ordinary (a copy, a correction, a re-copy), and a seconds resolution would
// make `ORDER BY created_at DESC` a coin flip between them. `id DESC` is the
// documented tiebreak anyway, but it is a tiebreak on INSERT order, which after
// a dedup bump is the OPPOSITE of recency — the bumped row keeps its old, low
// id. Millisecond resolution is what keeps the tiebreak from mattering in
// practice.
type Clip struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Device    string `json:"device"`
	CreatedAt int64  `json:"created_at"`
	// BumpCount is how many times this exact body has been re-captured. 0 means
	// "posted once". It is the column that makes the dedup path OBSERVABLE: see
	// Put's comment for why a one-statement upsert cannot otherwise report
	// whether it inserted or bumped.
	BumpCount int64 `json:"bump_count"`
}

// Store is the SQLite-backed clip store.
//
// 🔴 ONE WRITER, ONE CONNECTION. The Deployment is replicas:1 + Recreate on a
// ReadWriteOnce volume precisely so that exactly one process ever opens this
// file, and SetMaxOpenConns(1) below carries the same property INSIDE the
// process: every statement is serialised by database/sql's pool rather than by
// SQLite's busy handler. For a store that holds low hundreds of rows and serves
// a handful of requests a day that costs nothing measurable, and it removes
// SQLITE_BUSY from the failure set entirely.
//
// ⚠ THE COST, STATED: a slow query blocks every other request, /healthz
// included. With one client per device and a 500-row cap on the largest read,
// the slowest statement here is an FTS5 scan over a few hundred short rows. If
// this store ever grows to where that is not true, raise the pool and add a
// busy_timeout-aware retry — do not quietly raise the pool alone.
type Store struct {
	db *sql.DB
}

// schema is applied on every start. Every statement is IF NOT EXISTS, so a
// restart against an existing file is a no-op.
//
// 🔴 THE FTS5 TABLE IS EXTERNAL-CONTENT (`content='clips'`), WHICH MEANS IT
// STORES NO COPY OF THE BODY — only the index. That is what keeps a clip's text
// in exactly one place, so a delete cannot leave the body recoverable from the
// index. The cost is that the index does NOT maintain itself: the three triggers
// below are the entire synchronisation mechanism, and dropping any one of them
// silently desynchronises search from the table while every write still
// succeeds. TestSearchFindsARowWrittenThroughTheTriggers is the guard.
//
// The `clips_au` trigger is not decoration either: POST's upsert takes the
// `DO UPDATE` path on every re-capture, which fires AFTER UPDATE. Without it an
// FTS5 external-content index accumulates a stale entry per bump and starts
// returning rows whose content no longer matches — "an fts5 table is corrupt"
// errors in the worst case.
// ftsTableDef is the clips_fts definition from the table NAME onwards, with no
// leading `CREATE VIRTUAL TABLE`.
//
// 🔴 IT IS SPLIT OUT SO THE TOKENIZER IS SPELLED EXACTLY ONCE. `schema` creates
// it with IF NOT EXISTS and ensureTrigramIndex re-creates it without; if each
// carried its own copy of the column list, a later change to one would leave a
// migrated database with a DIFFERENT index from a fresh one, and every test on a
// fresh file would still pass.
//
// 🔴 tokenize='trigram', NOT THE DEFAULT unicode61 — AND THAT CHANGES THE SEARCH
// CONTRACT, not just its performance. unicode61 is an inverted index keyed on
// token starts, so it can only match a PREFIX of a word; trigram indexes every
// 3-character window of the raw text, so a MATCH is a genuine case-insensitive
// SUBSTRING test. Measured through this driver (modernc.org/sqlite v1.51.0,
// sqlite_version() 3.53.1), body `my github.com password is hunter2`:
//
//	MATCH '"assw"'      -> 1 row   (unicode61: 0 — the whole point)
//	MATCH '"com pass"'  -> 1 row   (a phrase substring spans whitespace)
//	MATCH '"GITHUB"'    -> 1 row   (ASCII case-folded)
//	MATCH '"ab"'        -> 0 rows, NO ERROR  (below trigram's 3-character floor)
//
// 🔴 THAT LAST ROW IS WHY PlanSearch EXISTS. A 1- or 2-character query is not
// rejected by the index, it is answered EMPTY — indistinguishable from "nothing
// matched", which is exactly the confusion this service's whole posture is
// written against (see List, ErrNoSuchClip, store_unreadable). Such a query is
// therefore served by a LIKE scan instead; PlanSearch decides, and it is the
// only place that decides.
//
// Punctuation is INDEXED under trigram (body `----`, MATCH '"----"' -> 1 row),
// which unicode61 discarded entirely. FTSMatchExpr's old token filter existed
// because of that discarding and is gone; PlanSearch's comment records why.
const ftsTableDef = ` clips_fts USING fts5 (
  body,
  content='clips',
  content_rowid='id',
  tokenize='trigram'
)`

const schema = `
CREATE TABLE IF NOT EXISTS clips (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  body        TEXT    NOT NULL,
  body_sha256 TEXT    NOT NULL UNIQUE,
  device      TEXT    NOT NULL,
  created_at  INTEGER NOT NULL,
  bump_count  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS clips_by_recency ON clips (created_at DESC, id DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS` + ftsTableDef + `;

CREATE TRIGGER IF NOT EXISTS clips_ai AFTER INSERT ON clips BEGIN
  INSERT INTO clips_fts (rowid, body) VALUES (new.id, new.body);
END;

CREATE TRIGGER IF NOT EXISTS clips_ad AFTER DELETE ON clips BEGIN
  INSERT INTO clips_fts (clips_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;

CREATE TRIGGER IF NOT EXISTS clips_au AFTER UPDATE ON clips BEGIN
  INSERT INTO clips_fts (clips_fts, rowid, body) VALUES ('delete', old.id, old.body);
  INSERT INTO clips_fts (rowid, body) VALUES (new.id, new.body);
END;
`

// OpenStore opens (creating if absent) the SQLite store at path.
//
// 🔴 journal_mode=WAL IS A DURABILITY REQUIREMENT, NOT A PERFORMANCE ONE. The
// Deployment is `strategy: Recreate` with no SIGTERM grace worth speaking of, so
// a rollout stops this process abruptly; WAL is what makes an abrupt stop
// crash-safe (committed transactions survive, the rest is rolled back on next
// open). It is also why the backup must go through SQLite's online backup API
// rather than `cp` (e.g. `sqlite3 clipstore.db ".backup out.db"`).
//
// busy_timeout is set even though SetMaxOpenConns(1) should make it unreachable
// from this process: an online backup opens the same file mode=rw to update
// the -shm index, so a second OS process does briefly touch it.
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Prove the file is actually usable NOW rather than on the first request:
	// a wrong mountPath or a read-only volume must be a startup failure, which
	// CrashLoopBackOff makes visible, not a 503 per request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema to %s: %w", path, err)
	}
	rebuilt, err := ensureTrigramIndex(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure trigram index on %s: %w", path, err)
	}
	if rebuilt {
		log.Printf("clipstore: clips_fts was not a trigram index; dropped and rebuilt it "+
			"from the clips table (%s)", path)
	}
	// FTS5 is a compile-time option of the driver, not a runtime one. If it were
	// absent the CREATE VIRTUAL TABLE above would already have failed — this
	// read is the positive control that it is present AND queryable, so a
	// missing-FTS5 build cannot reach the first search request.
	//
	// ⚠ WHAT IT DOES *NOT* PROVE, STATED SO IT IS NOT OVER-READ: it returns 0
	// and that is expected, so it cannot distinguish a working index from one
	// that matches nothing. It is a control on FTS5 being PRESENT AND QUERYABLE,
	// not on the tokenizer — ensureTrigramIndex above is what pins the
	// tokenizer, by reading the stored DDL and rebuilding when it disagrees.
	//
	// 🔴 THE NEEDLE MUST STAY >= MinTrigramRunes CHARACTERS. Under trigram a
	// shorter string returns 0 rows with NO ERROR whatever the index's state, so
	// a short needle would turn this from a weak control into a vacuous one that
	// cannot fail. TestTheStartupProbeNeedleIsLongEnoughForTrigram pins it.
	var probe int
	if err := db.QueryRow(`SELECT count(*) FROM clips_fts WHERE clips_fts MATCH ?`,
		`"`+startupProbeNeedle+`"`).Scan(&probe); err != nil {
		db.Close()
		return nil, fmt.Errorf("fts5 probe on %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// startupProbeNeedle is the string OpenStore's FTS5 control searches for. It is
// deliberately something no real clip contains.
const startupProbeNeedle = "clipstore-startup-probe"

// ensureTrigramIndex migrates a clips_fts that was created before the trigram
// switch. It reports whether it rebuilt the index.
//
// 🔴 THIS EXISTS BECAUSE `CREATE VIRTUAL TABLE IF NOT EXISTS` DOES NOT ALTER AN
// EXISTING TABLE — IT IS A NO-OP THAT RETURNS NO ERROR. Measured through this
// driver against a database holding a unicode61 clips_fts: applying the new
// `schema` returned err=nil, left the stored DDL completely unchanged, and
// `MATCH '"assw"'` still returned 0 rows. Without this function the service
// would keep prefix semantics forever on any pre-existing file while every test
// on a fresh temp file passed — the exact shape of silent failure this repo's
// rules single out.
//
// 🔴 A DROP AND REBUILD LOSES NOTHING, WHICH IS WHY IT IS SAFE TO DO ON EVERY
// START. clips_fts is external-content (`content='clips'`): it stores only the
// index, and `INSERT INTO clips_fts(clips_fts) VALUES('rebuild')` regenerates it
// from the clips table, which is untouched. Measured: after drop + re-create +
// rebuild, `MATCH '"assw"'` returned 1 row, `count(*) FROM clips` was unchanged,
// and the backup's `('integrity-check', 1)` still ran clean.
//
// 🔴 THAT CLAIM IS ONLY TRUE BECAUSE THE THREE STATEMENTS RUN IN ONE
// TRANSACTION — it was NOT true when they ran as three unprotected statements,
// because the half-way state is silent and permanent. See the transaction's own
// comment in the body below; do not unwind it.
//
// It is kept because any clipstore.db created by the first, prefix-only cut of
// this service hits it on the next start. Cheap, lossless, and the failure it
// prevents is silent.
func ensureTrigramIndex(db *sql.DB) (bool, error) {
	var ddl string
	switch err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='clips_fts'`,
	).Scan(&ddl); {
	case errors.Is(err, sql.ErrNoRows):
		// `schema` ran without error immediately above, so the table must be
		// there. If it is not, something is wrong that a rebuild would hide.
		return false, errors.New("clips_fts is absent even though the schema applied cleanly")
	case err != nil:
		return false, fmt.Errorf("read the stored clips_fts DDL: %w", err)
	}
	if ddlDeclaresTrigram(ddl) {
		return false, nil
	}
	// 🔴 ONE TRANSACTION, AND IT IS WHAT MAKES THE "LOSES NOTHING" CLAIM ABOVE
	// TRUE. The three statements used to run as one unprepared db.Exec with no
	// transaction, and the half-way state is NOT recoverable by anything else
	// here: once the DROP and the CREATE have landed and the `rebuild` has not,
	// the stored DDL says trigram, so ddlDeclaresTrigram's fail-safe direction
	// cannot help — the next start reads "already trigram", returns (false, nil),
	// and the service serves an EMPTY index forever with no error anywhere.
	// Measured: in that state `q=assw` returned 0 rows and no error while the
	// unfiltered list still returned its row. A once-loud failure became a
	// permanent silent one, which is the exact failure class this file is
	// written against.
	//
	// MEASURED, NOT ASSUMED, because both halves were open questions:
	//
	//   - FTS5's `('rebuild')` command DOES work inside an explicit transaction
	//     through this driver (modernc.org/sqlite v1.51.0): drop + create +
	//     rebuild in one tx committed clean and `MATCH '"assw"'` then returned 1.
	//   - A ROLLBACK genuinely restores the PREVIOUS virtual table. Against a
	//     database with no `clips` table — so `rebuild` fails with
	//     `no such table: main.clips` — the rollback put the original unicode61
	//     DDL back, and ddlDeclaresTrigram read it as not-trigram again. Without
	//     the transaction the identical failure left a trigram DDL behind.
	//
	// So a crash or a transient error between statements now leaves the OLD index
	// in place and the migration is RETRIED, loudly, on every subsequent start.
	// TestAFailedTrigramMigrationLeavesNoHalfMigratedIndex is the guard.
	//
	// ⚠ WHY NOT THE ROW-COUNT ASSERTION INSTEAD: comparing
	// `count(*) FROM clips_fts` against `count(*) FROM clips` after the rebuild
	// CANNOT FAIL. On an external-content FTS5 table an unqualified count of
	// clips_fts is served from the CONTENT table, so the two are equal by
	// construction — measured 1 and 1 in exactly the half-migrated state above,
	// where the index was empty. backup-scripts.yaml's COUNTED_TABLES comment
	// records the same property and is why it excludes clips_fts.
	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin the clips_fts trigram migration: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE clips_fts;
CREATE VIRTUAL TABLE` + ftsTableDef + `;
INSERT INTO clips_fts(clips_fts) VALUES('rebuild');`); err != nil {
		// The rollback error is reported alongside rather than instead: if the
		// rollback ALSO failed, the half-migrated state may have survived and
		// that is the more serious fact of the two.
		if rbErr := tx.Rollback(); rbErr != nil {
			return false, fmt.Errorf(
				"drop and rebuild clips_fts as trigram: %w (AND THE ROLLBACK FAILED: %v — "+
					"the index may be half-migrated on disk)", err, rbErr)
		}
		return false, fmt.Errorf("drop and rebuild clips_fts as trigram: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit the clips_fts trigram migration: %w", err)
	}
	return true, nil
}

// ddlDeclaresTrigram reports whether a stored clips_fts DDL asks for trigram.
//
// 🔴 IT FAILS SAFE TOWARDS REBUILDING, WHICH IS THE WHOLE DESIGN. It normalises
// away whitespace and every quoting style SQLite accepts, so `tokenize='trigram'`,
// `tokenize = "trigram"` and `tokenize=trigram` all read as trigram — but
// anything it does NOT recognise is reported as not-trigram, which costs one
// lossless rebuild. The opposite failure (reading an unfamiliar spelling as
// "already trigram" and skipping the migration) would leave a unicode61 index in
// place silently, so the asymmetry is deliberate: the cheap error is the one
// this function is allowed to make.
func ddlDeclaresTrigram(ddl string) bool {
	var norm strings.Builder
	for _, r := range strings.ToLower(ddl) {
		switch {
		case unicode.IsSpace(r), r == '\'', r == '"', r == '`', r == '[', r == ']':
			continue
		}
		norm.WriteRune(r)
	}
	return strings.Contains(norm.String(), "tokenize=trigram")
}

func (s *Store) Close() error { return s.db.Close() }

// Ping is the store half of /healthz. It runs `SELECT count(*) FROM clips`
// rather than db.Ping().
//
// 🔴 WHAT THAT BUYS, STATED NARROWLY, BECAUSE THIS COMMENT USED TO CLAIM MORE
// AND THE MORE WAS MEASURED FALSE. It buys exactly one property: the statement
// is RESOLVED AGAINST THE SCHEMA, so the `clips` table must be present and
// queryable for /healthz to answer 200. db.Ping() asks the driver whether the
// connection is alive and never names a table, so it answers nil on a database
// whose `clips` table is gone. Measured against an open store: after
// `DROP TABLE clips`, Ping returns `no such table: clips` while
// db.PingContext returns nil. TestStorePingReadsTheClipsTableNotTheConnection
// is the guard, and it fails if this body is "simplified" to db.PingContext.
//
// 🔴 WHAT IT DOES *NOT* BUY — THE RETRACTED CLAIM. This comment used to say
// that because it touches a page, "a pod that is listening with its volume
// missing fails these probes instead of being reported healthy forever". THAT
// IS FALSE. SQLite serves `count(*)` out of its page cache, and the pod never
// reopens the file, so an already-open store answers 200 in every file-level
// loss state. Measured 2026-11-15 through this driver (modernc.org/sqlite
// v1.51.0) against an open store, Ping returning nil in all four:
//
//	db file unlinked                      -> nil
//	db file truncated to 0 bytes          -> nil
//	db overwritten in place with 40 KiB
//	  of junk (in-place corruption)       -> nil
//	parent directory removed              -> nil
//
// So /healthz is a SCHEMA probe, not a storage probe. It does not detect a
// remounted volume, a deleted PV, a truncation or in-place corruption, and no
// comment or manifest may imply that it does. Detecting those needs a probe
// that re-opens the file or stats it, which this service does not have.
func (s *Store) Ping(ctx context.Context) error {
	var n int64
	return s.db.QueryRowContext(ctx, `SELECT count(*) FROM clips`).Scan(&n)
}

// BodyHash is the dedup key: hex sha256 over the body bytes.
//
// Hex, not raw bytes: it is a TEXT column so `sqlite3` and the backup script can
// print it, and a UNIQUE index over 64 ASCII bytes costs nothing at this scale.
func BodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// Put appends a clip, or bumps an existing identical one to the front.
//
// 🔴 ONE ATOMIC STATEMENT, which is the whole point. A read-then-write
// ("SELECT by hash; if found UPDATE else INSERT") has a window in which two
// concurrent captures of the same body both miss and one of them fails on the
// UNIQUE constraint — and the obvious repair for THAT is a retry loop that can
// still lose. `INSERT … ON CONFLICT(body_sha256) DO UPDATE` has no window: the
// conflict is resolved inside the statement.
//
// 🔴 HOW `deduped` IS DERIVED, because this is the subtle part. An upsert reports
// the same `changes()` (1) whether it inserted or updated, so the usual ways of
// asking "was this new?" all answer wrongly. The `bump_count` column makes it
// observable instead: a fresh INSERT leaves it at its DEFAULT 0, and the only
// path that can raise it is `DO UPDATE`. So `bump_count > 0` in the RETURNING
// row means THIS statement took the conflict path — a row that was bumped in an
// earlier request cannot come back from an INSERT, because it would not have
// been an INSERT.
//
// `device` is overwritten on a bump: the clip's attribution is "where it most
// recently came from", which is what a chronological newest-first list means by
// it. The previous device is not kept — nothing reads it and keeping it would be
// a second, unasked-for history.
func (s *Store) Put(ctx context.Context, body, device string, nowMillis int64) (Clip, bool, error) {
	c := Clip{Body: body, Device: device}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO clips (body, body_sha256, device, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (body_sha256) DO UPDATE SET
		  created_at = excluded.created_at,
		  device     = excluded.device,
		  bump_count = clips.bump_count + 1
		RETURNING id, created_at, bump_count`,
		body, BodyHash(body), device, nowMillis,
	).Scan(&c.ID, &c.CreatedAt, &c.BumpCount)
	if err != nil {
		return Clip{}, false, fmt.Errorf("upsert clip: %w", err)
	}
	return c, c.BumpCount > 0, nil
}

// List returns at most limit clips, newest first, optionally restricted to those
// matching q.
//
// 🔴 IT RETURNS A NON-NIL EMPTY SLICE FOR "NOTHING MATCHED" AND AN ERROR FOR
// "COULD NOT TELL". Those are different answers and the caller must be able to
// tell them apart — an unreadable store that returned `[]` would be
// indistinguishable from a working feature with nothing in it, which is the
// failure this service's whole posture is written against.
//
// 🔴 AND THAT IS WHY THERE IS NO LONGER A SHORT-CIRCUIT RETURN HERE. This
// function used to answer `[]Clip{}, nil` without running any query for a
// `q` whose every token the unicode61 tokenizer would discard. Under trigram no
// query is unanswerable: punctuation is indexed, and a token too short for the
// index is served by the scan instead (PlanSearch decides which). Every non-empty
// `q` therefore reaches a real query, and an empty result is now something the
// database actually said rather than something this function assumed.
//
// It renders the plan and makes no decisions of its own — PlanSearch owns the
// semantics, and its comment states them.
func (s *Store) List(ctx context.Context, limit int, q string) ([]Clip, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch plan := PlanSearch(q); plan.Kind {
	case SearchIndex:
		// 🔴 A SUBQUERY, AND `clips_fts` SPELLED IN FULL ON BOTH SIDES OF THE
		// MATCH. The obvious shape — `JOIN clips_fts f ON f.rowid = c.id WHERE f
		// MATCH ?` — does not work: FTS5's MATCH operator requires the table's
		// own NAME on its left, and an ALIAS is parsed as a column reference, so
		// SQLite answers `no such column: f`. Measured on this code before the
		// fix: every `?q=` request returned 503 store_unreadable while the
		// unfiltered list worked, which is exactly the shape of bug a handler
		// that answered `[]` on error would have hidden as "nothing matched".
		rows, err = s.db.QueryContext(ctx, `
			SELECT c.id, c.body, c.device, c.created_at, c.bump_count
			FROM clips c
			WHERE c.id IN (SELECT rowid FROM clips_fts WHERE clips_fts MATCH ?)
			ORDER BY c.created_at DESC, c.id DESC
			LIMIT ?`, plan.Match, limit)

	case SearchScan:
		// 🔴 `ESCAPE '\'` IS NOT OPTIONAL — WITHOUT IT THIS IS A CORRECTNESS BUG,
		// NOT A ROBUSTNESS NICETY. `%` and `_` are LIKE's wildcards, so a search
		// for `%` would match every row in the store and a search for `_` would
		// match any single character. PlanSearch has already escaped them into
		// the patterns; this clause is what makes the escaping mean anything, and
		// the two halves only work together. Measured: with the fixtures
		// `100% certain` + `1000 miles away`, the unescaped pattern matched 2 rows
		// and the escaped one matched 1.
		conds := make([]string, 0, len(plan.Like))
		args := make([]any, 0, len(plan.Like)+1)
		for _, pattern := range plan.Like {
			conds = append(conds, `c.body LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		args = append(args, limit)
		rows, err = s.db.QueryContext(ctx, `
			SELECT c.id, c.body, c.device, c.created_at, c.bump_count
			FROM clips c
			WHERE `+strings.Join(conds, " AND ")+`
			ORDER BY c.created_at DESC, c.id DESC
			LIMIT ?`, args...)

	default: // SearchAll — no query, the plain newest-first list.
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, body, device, created_at, bump_count
			FROM clips
			ORDER BY created_at DESC, id DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query clips: %w", err)
	}
	defer rows.Close()

	out := []Clip{}
	for rows.Next() {
		var c Clip
		if err := rows.Scan(&c.ID, &c.Body, &c.Device, &c.CreatedAt, &c.BumpCount); err != nil {
			return nil, fmt.Errorf("scan clip: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clips: %w", err)
	}
	return out, nil
}

// ErrNoSuchClip is returned by Delete for an id that is not in the store.
var ErrNoSuchClip = errors.New("no such clip")

// Delete removes one clip by id. It returns ErrNoSuchClip when the id was not
// there, so the handler can answer 404 rather than a cheerful 204 for a row that
// never existed.
func (s *Store) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clips WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete clip %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete clip %d: rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrNoSuchClip
	}
	return nil
}

// MinTrigramRunes is the shortest query token the trigram index can answer.
//
// 🔴 IT IS COUNTED IN RUNES, NOT BYTES. Trigram windows are three CHARACTERS, so
// the floor is a character count: a 2-character CJK query is 6 bytes and would
// pass a byte-length test while still being unanswerable by the index. Measured
// through this driver against body `東京都の天気`: MATCH '"東京都"' (3 runes,
// 9 bytes) -> 1 row, MATCH '"東京"' (2 runes, 6 bytes) -> 0 rows.
const MinTrigramRunes = 3

// SearchKind is how one query will be served.
type SearchKind int

const (
	// SearchAll is "no query at all" — the plain newest-first list.
	SearchAll SearchKind = iota
	// SearchIndex serves the query from the trigram FTS5 index.
	SearchIndex
	// SearchScan serves it from a LIKE '%…%' table scan.
	SearchScan
)

func (k SearchKind) String() string {
	switch k {
	case SearchAll:
		return "SearchAll"
	case SearchIndex:
		return "SearchIndex"
	case SearchScan:
		return "SearchScan"
	}
	return "SearchKind(?)"
}

// SearchPlan is a decided query: exactly one of Match or Like is populated.
type SearchPlan struct {
	Kind SearchKind
	// Match is the FTS5 MATCH expression. SearchIndex only.
	Match string
	// Like holds one already-escaped `%token%` pattern per query token, to be
	// AND-ed. SearchScan only.
	Like []string
}

// PlanSearch turns free text into a decided query plan.
//
// 🔴 ONE RULE, ONE PLACE: THIS IS THE ONLY FUNCTION THAT DECIDES SEARCH
// SEMANTICS. List renders the plan and nothing else, and the handler passes `q`
// through verbatim, so the semantics below are stated once and cannot drift
// between a caller and a test.
//
// THE CONTRACT, STATED EXACTLY: a clip matches when EVERY whitespace-separated
// query token appears somewhere in its body as a case-insensitive SUBSTRING.
// That is uniform across both execution paths, and it is what the design doc's
// the design ("every query token appears in some word of the body") asked for and the
// old unicode61 index could not provide — it could only match the PREFIX of a
// word, so `assw` did not find `password`.
//
// 🔴 WHY TWO PATHS AT ALL, AND WHY THE SHORT-TOKEN ONE IS A CORRECTNESS FIX. The
// trigram index cannot answer a token shorter than MinTrigramRunes: it returns
// ZERO ROWS AND NO ERROR, which is indistinguishable from "nothing matched".
// This service's entire posture is that an empty result and an "I could not
// tell" must be different answers (see List, ErrNoSuchClip, store_unreadable),
// so shipping that as a documented hole was not an option. When ANY token is
// short the WHOLE query goes to the scan instead — not just the short token —
// because mixing a MATCH and a LIKE for one query would AND together two
// different notions of matching, and the uniform contract above is worth more
// than the index lookup. At this scale (a personal clipboard, low hundreds of
// rows, a handful of requests a day) the scan is cheap.
//
// 🔴 NEVER INTERPOLATE RAW USER TEXT INTO A MATCH EXPRESSION. FTS5 MATCH has its
// own grammar (NEAR, OR, AND, column filters, parentheses, quotes); a body
// containing `"` or `(` passed through verbatim is at best a syntax error the
// client sees as a 5xx, and at worst a query against a column the caller should
// not be able to name. Every token is therefore re-emitted as a QUOTED STRING
// with `"` doubled, which FTS5 reads as a literal phrase, so no input can reach
// the grammar. The scan path has the mirror-image hazard and likeEscape handles
// it: `%` and `_` are LIKE wildcards, not literals.
//
// ⚠ NO TOKEN IS DROPPED ANY MORE, AND THE OLD REASON FOR DROPPING IS GONE. This
// function's predecessor discarded any token containing no letter or digit, on
// the stated grounds that "the FTS5 unicode61 tokenizer discards them, so `""*`
// would be a syntax error". Under trigram the premise is simply false:
// punctuation is INDEXED, and a search for `----` is a legitimate search that
// finds a clip whose body is `----` (measured: 1 row). Dropping it would discard
// a real constraint and silently widen the result set. The old claim was also
// measurably wrong on its own terms — against unicode61 this driver answers
// MATCH '"----"*' with 0 rows and NO error, not a syntax error. And nothing can
// reach the empty-phrase case regardless, because strings.Fields never yields an
// empty field.
//
// ⚠ THE TRAILING `*` IS GONE TOO. A prefix star still PARSES under trigram but
// is meaningless, because a trigram match is already positionally unanchored.
// Measured over six distinct needles: `"x"*` and `"x"` returned identical counts
// in every case, so removing it changes no result.
//
// ⚠ ONE MEASURED DIVERGENCE BETWEEN THE TWO PATHS, RECORDED BECAUSE IT IS REAL
// AND BECAUSE THE OBVIOUS ASSUMPTION IS WRONG. Trigram and LIKE do NOT fold case
// identically. Trigram case-folds BEYOND ASCII; SQLite's LIKE folds ASCII only.
// Measured against body `Ärger über Äpfel`:
//
//	query     index path (MATCH)   scan path (LIKE)
//	"ärger"   1 row                0 rows   <- DIVERGES
//	"Ärger"   1 row                1 row
//	"ÄRGER"   1 row                1 row
//
// So `ärger` finds that clip, but `ärger ab` — which routes to the scan because
// `ab` is short — does not. CJK needs no folding and agrees on both paths
// (`東京都`: 1 row and 1 row). The ASCII cases agree throughout, which is every
// query this service realistically sees; the divergence is confined to
// non-ASCII letters whose case differs from the body's. It is documented rather
// than fixed: fixing it would mean either a custom collation or lower-casing
// bodies into a shadow column, both of which cost more than the defect.
func PlanSearch(q string) SearchPlan {
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return SearchPlan{Kind: SearchAll}
	}
	for _, tok := range tokens {
		if utf8.RuneCountInString(tok) < MinTrigramRunes {
			patterns := make([]string, 0, len(tokens))
			for _, t := range tokens {
				patterns = append(patterns, "%"+likeEscape(t)+"%")
			}
			return SearchPlan{Kind: SearchScan, Like: patterns}
		}
	}
	quoted := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		quoted = append(quoted, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
	}
	return SearchPlan{Kind: SearchIndex, Match: strings.Join(quoted, " AND ")}
}

// likeEscape makes a token a LITERAL inside a LIKE pattern, for use with
// `ESCAPE '\'`.
//
// 🔴 THE BACKSLASH CASE MUST COME FIRST, and it is the one a reader skips. `\`
// is the escape character itself, so a token containing one (`C:\Users\zach`)
// would otherwise emit a dangling escape that captures the following character —
// corrupting the pattern rather than matching the text. Escaping `\` as part of
// the same single pass is what prevents it.
func likeEscape(token string) string {
	var out strings.Builder
	out.Grow(len(token))
	for _, r := range token {
		if r == '\\' || r == '%' || r == '_' {
			out.WriteRune('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}
