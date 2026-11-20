package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Limits. These are CONSTANTS, not flags, on purpose: a flag is a second place
// the number lives, and the README quotes these. Change them here and the
// documented value moves with them.
const (
	// DefaultLimit is the page size when ?limit= is absent.
	//
	// 🔴 `limit` IS THE ONLY GROWTH BOUND THIS SERVICE HAS. The no-expiry design decision
	// is "no expiry, manual delete only", so the table is unbounded forever and
	// an unbounded list endpoint would eventually serve the whole history on
	// every picker open. 100 is chosen to be larger than any plausible scroll
	// (the pickers that consume this show a filtered list, not a full one) and
	// small enough that the response stays a few tens of KiB.
	DefaultLimit = 100

	// MaxLimit is the hard ceiling. A request asking for more is REFUSED with
	// 400, not silently clamped: a client that asked for 5000 and received 500
	// with a 200 cannot tell it was truncated, and a truncated newest-first list
	// looks exactly like a store that has only 500 rows.
	MaxLimit = 500

	// MaxBodyBytes bounds one clip. 256 KiB is ~2.6x the 100,000-CHARACTER cap
	// Android's PROCESS_TEXT path silently truncates at
	// (TextUtils.trimToParcelableSize), so the platform's own limit is the
	// binding one for the phone and this is not what a real capture hits.
	MaxBodyBytes = 256 * 1024

	// MaxRequestBytes bounds the whole JSON envelope. The body is the only
	// unbounded field in it, so this is MaxBodyBytes plus room for JSON escaping
	// of worst-case input (every byte escaped as \uXXXX is 6x) and the device
	// field.
	MaxRequestBytes = 8 * MaxBodyBytes

	// MaxDeviceBytes bounds the attribution string. It is rendered in a picker
	// row next to a relative age ("3h · pixel-7"), so anything long is a layout
	// bug rather than information.
	MaxDeviceBytes = 64

	// MinTokenBytes is the floor on the bearer token at startup. Stricter than
	// a minimum length of 8 — a short credential is a
	// configuration mistake and must be a loud refusal, never a silent accept.
	// ./setup provisions 32 random bytes as hex (64 chars).
	//
	// 🔴 WHAT IT DOES AND DOES NOT BUY IS STATED ON tokenRefusal (main.go), and
	// the short version is that it is a DIAGNOSIS mechanism: an empty token
	// authenticates nothing either way, because bearer() rejects an empty
	// credential. Enforced at startup only — NewAPI deliberately does not
	// re-check, so this constant is not a request-path guard. Mutating it to 0
	// once survived the entire suite; TestTheTokenFloorIsAtLeastEightBytes
	// (main_test.go) is what now makes that mutant red.
	MinTokenBytes = 24
)

// clipStore is the store surface the handlers use.
//
// 🔴 IT IS AN INTERFACE SO THAT A FAILING STORE IS REACHABLE FROM A TEST. The
// "fails loud, never empty" posture is the single most important property of
// this service and it lives entirely in the error branches of the handlers —
// branches that a real *Store on a healthy temp file CANNOT enter. Without this
// seam those branches would be untested code asserting the feature's headline
// guarantee.
type clipStore interface {
	Put(ctx context.Context, body, device string, nowMillis int64) (Clip, bool, error)
	List(ctx context.Context, limit int, q string) ([]Clip, error)
	After(ctx context.Context, after int64, limit int) ([]Clip, error)
	Get(ctx context.Context, id int64) (Clip, error)
	Delete(ctx context.Context, id int64) error
	Ping(ctx context.Context) error
}

// API is the HTTP surface. Every route that reads or writes a clip is
// bearer-gated, and /healthz is not. See routeTable.
type API struct {
	store clipStore
	token string
	// now is injected so the dedup-bump test can pin two different timestamps
	// without sleeping.
	now func() time.Time
	// changes wakes long-poll waiters on every write; waiters bounds them.
	// See feed.go.
	changes *changes
	waiters chan struct{}
}

// NewAPI builds the API. token must already have been validated by the caller
// (main does it at startup, loudly) — NewAPI does not re-check it, because a
// handler is the wrong place to discover a configuration error.
func NewAPI(store clipStore, token string, now func() time.Time) *API {
	if now == nil {
		now = time.Now
	}
	return &API{store: store, token: token, now: now, changes: newChanges(),
		waiters: make(chan struct{}, MaxWaiters)}
}

// muxRoute is one registration: the http.ServeMux pattern and its handler.
type muxRoute struct {
	// Pattern is the ServeMux pattern, and it is also the LEDGER ENTRY a test
	// compares against a literal list.
	Pattern string
	Handler http.HandlerFunc
}

// routeTable is this service's COMPLETE routing ledger, split into the routes
// that require the bearer token and the ones that do not. It is DATA rather
// than a sequence of mux.HandleFunc calls for exactly one reason: an
// http.ServeMux does not expose what was registered on it, so a test cannot
// read a mux — but it can read this.
//
// 🔴 THE HAZARD THIS CLOSES, which is NOT the one the old single-mux shape
// closed. /healthz is unauthenticated, so there are two route sets instead of
// one, and the dangerous mistake is no longer "the author forgot the
// middleware" — it is a clip route landing in `public` by habit or by copy-paste
// from the line above it. Nothing about that mistake fails to compile and
// nothing about it looks wrong in a diff.
//
// TestTheRouteLedgerIsExactlyTheseRoutes pins BOTH sets against literal lists,
// so it goes red when either one GROWS or SHRINKS, and
// TestOnlyHealthzIsReachableWithoutTheBearerToken then drives every entry in
// this table over HTTP — so a route added here without a probe case is a test
// failure rather than silent coverage loss.
func (a *API) routeTable() (gated, public []muxRoute) {
	gated = []muxRoute{
		{"POST /v1/clips", a.postClip},
		{"GET /v1/clips", a.listClips},
		{"GET /v1/clips/{id}", a.getClip},
		{"DELETE /v1/clips/{id}", a.deleteClip},
	}
	public = []muxRoute{
		// THE ONLY UNAUTHENTICATED ROUTE. See healthz's own comment for why it
		// is one, and note it still READS A ROW.
		{"GET /healthz", a.healthz},
	}
	return gated, public
}

// Handler returns the routed handler.
//
// 🔴 AUTH WRAPS A MUX, NOT EACH ROUTE, AND THE CATCH-ALL IS WHAT MAKES THAT
// TRUE. Per-route middleware is how an endpoint added later ends up
// unauthenticated: the author adds a route and forgets the wrapper, and nothing
// fails. Here the gated mux is wrapped once and mounted at "/", so:
//
//   - every clip route is authenticated by construction, and so is any route
//     added to `gated` later;
//   - an UNKNOWN path matches "/" too, so it answers 401 rather than 404 and an
//     unauthenticated prober cannot map the API;
//   - becoming unauthenticated requires adding a pattern to `public`, which is
//     a one-line, visible, test-pinned act rather than an omission.
func (a *API) Handler() http.Handler {
	gated, public := a.routeTable()

	clips := http.NewServeMux()
	for _, r := range gated {
		clips.HandleFunc(r.Pattern, r.Handler)
	}

	root := http.NewServeMux()
	// "/" is the catch-all and it is registered FIRST so the reading order
	// matches the precedence: the explicit public patterns below are more
	// specific, so Go's ServeMux prefers them, and everything else lands on the
	// gated mux.
	root.Handle("/", a.requireToken(clips))
	for _, r := range public {
		root.HandleFunc(r.Pattern, r.Handler)
	}
	return root
}

// requireToken enforces `Authorization: Bearer <token>` on everything it wraps.
//
// 🔴 IT WRAPS THE CLIP ROUTES AND THE CATCH-ALL — NOT /healthz. That is a
// settled decision and the reasoning runs the other way from what this comment
// used to say. The old shape gated /healthz too, and the only way to probe a
// bearer-gated endpoint with an httpGet probe is `httpGet.httpHeaders`, whose
// value is a LITERAL in the deployment manifest — so the token would have been
// committed in plaintext. That cost the service a `-healthcheck` self-probe mode
// and three `exec` probes. An unauthenticated /healthz removes the whole
// mechanism: probes (Kubernetes, compose, ./setup) are plain HTTP GETs.
//
// What /healthz leaks to an unauthenticated network peer is one bit — "this pod can
// read its database" — and no clip content, no row count and no route names. The
// bearer requirement is UNCHANGED on every route that reads or writes a clip.
//
// subtle.ConstantTimeCompare, not `==`: a byte-at-a-time comparison leaks the
// token's prefix to anything that can time the response, and this listener is
// reachable from every host on the private network.
func (a *API) requireToken(next http.Handler) http.Handler {
	want := []byte(a.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearer(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			// 🔴 THE SAME ANSWER FOR "no header" AND "wrong token". Telling them
			// apart is free information for anyone probing the listener, and no
			// legitimate client needs it: all four callers either have the token
			// or are misconfigured.
			writeErr(w, http.StatusUnauthorized, "unauthorized",
				"a valid `Authorization: Bearer <token>` header is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the credential from an Authorization header value.
// The scheme is matched case-INSENSITIVELY (RFC 7235 says it is), which is not
// pedantry: Android's HttpURLConnection and Go's http.Client both pass the
// header through verbatim, so whatever a client writes is what arrives.
func bearer(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

type postClipRequest struct {
	Body   string `json:"body"`
	Device string `json:"device"`
}

type postClipResponse struct {
	Clip
	// Deduped reports that this POST hit an existing identical body and bumped
	// it to the front instead of appending a row. A client that shows "saved"
	// can show "resurfaced" instead; nothing is required to read it.
	Deduped bool `json:"deduped"`
}

func (a *API) postClip(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"the request body exceeds "+strconv.Itoa(MaxRequestBytes)+" bytes")
		return
	}
	var req postClipRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "the request body is not a JSON object")
		return
	}
	if req.Body == "" {
		writeErr(w, http.StatusBadRequest, "empty_body",
			"`body` is required and must not be empty")
		return
	}
	if len(req.Body) > MaxBodyBytes {
		writeErr(w, http.StatusBadRequest, "body_too_large",
			"`body` is "+strconv.Itoa(len(req.Body))+" bytes; the limit is "+
				strconv.Itoa(MaxBodyBytes))
		return
	}
	// 🔴 `device` IS REQUIRED, NOT DEFAULTED. The design makes device
	// attribution per row part of the requirement, and a default ("unknown")
	// would make a misconfigured client indistinguishable from a working one in
	// the picker row that is supposed to tell them apart.
	if strings.TrimSpace(req.Device) == "" {
		writeErr(w, http.StatusBadRequest, "missing_device",
			"`device` is required and must not be blank")
		return
	}
	if len(req.Device) > MaxDeviceBytes {
		writeErr(w, http.StatusBadRequest, "device_too_long",
			"`device` is "+strconv.Itoa(len(req.Device))+" bytes; the limit is "+
				strconv.Itoa(MaxDeviceBytes))
		return
	}
	// 🔴 NEVER STORE THIS SERVER'S OWN CREDENTIAL. A token copied on a phone and
	// captured as a clip would be readable back out by anything that can read
	// clips (an agent via the MCP server). Refused 422 — not stored, never
	// echoed, never logged. `a.token != ""` keeps an (unsupported) empty token
	// from refusing every clip, since every string contains "".
	if a.token != "" && (strings.Contains(req.Body, a.token) || strings.Contains(req.Device, a.token)) {
		log.Printf("clipstore: POST /v1/clips: refused a clip containing this server's token (not stored)")
		writeErr(w, http.StatusUnprocessableEntity, "contains_server_token",
			"the clip looked like this server's own access token and was not stored")
		return
	}

	clip, deduped, err := a.store.Put(r.Context(), req.Body, strings.TrimSpace(req.Device),
		a.now().UnixMilli())
	if err != nil {
		// 🔴 FAIL LOUD. A capture the client believes was stored and was not is
		// the one failure this service must never produce silently; the Android
		// outbox keeps the item on anything that is not a success.
		log.Printf("clipstore: POST /v1/clips: %v", err)
		writeErr(w, http.StatusServiceUnavailable, "store_unwritable",
			"the clip store could not be written")
		return
	}
	// Wake long-poll waiters. A dedup bump wakes them too; they re-query and
	// find nothing new (a bump keeps its id), which costs one cheap SELECT.
	a.changes.notify()
	status := http.StatusCreated
	if deduped {
		status = http.StatusOK
	}
	writeJSON(w, status, postClipResponse{Clip: clip, Deduped: deduped})
}

type listClipsResponse struct {
	// Clips is never null. `encoding/json` renders a nil slice as `null`, and a
	// client that does `for (clip of body.clips)` on null throws where it would
	// have rendered an empty list — so List returns `[]Clip{}` and this field is
	// documented as always an array.
	Clips []Clip `json:"clips"`
	Limit int    `json:"limit"`
	Query string `json:"query,omitempty"`
}

func (a *API) listClips(w http.ResponseWriter, r *http.Request) {
	// `after` switches this route to the change feed (oldest first, optional
	// long-poll). Without it the route is exactly the newest-first list it was.
	if r.URL.Query().Has("after") {
		a.feedClips(w, r)
		return
	}
	if r.URL.Query().Has("wait") {
		writeErr(w, http.StatusBadRequest, "invalid_wait", "`wait` is only valid with `after`")
		return
	}
	limit := DefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxLimit {
			writeErr(w, http.StatusBadRequest, "invalid_limit",
				"`limit` must be an integer in 1.."+strconv.Itoa(MaxLimit)+
					"; it is not clamped, because a silently truncated "+
					"newest-first list is indistinguishable from a short store")
			return
		}
		limit = n
	}
	q := r.URL.Query().Get("q")

	clips, err := a.store.List(r.Context(), limit, q)
	if err != nil {
		// 🔴 NEVER `[]` ON AN ERROR. An empty result is indistinguishable from a
		// working feature with nothing in it, so an unreadable store answers 503
		// with a named reason and the clients report the reason verbatim.
		log.Printf("clipstore: GET /v1/clips: %v", err)
		writeErr(w, http.StatusServiceUnavailable, "store_unreadable",
			"the clip store could not be read")
		return
	}
	writeJSON(w, http.StatusOK, listClipsResponse{Clips: clips, Limit: limit, Query: q})
}

func (a *API) deleteClip(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeErr(w, http.StatusBadRequest, "invalid_id", "the path id must be a positive integer")
		return
	}
	switch err := a.store.Delete(r.Context(), id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrNoSuchClip):
		// 🔴 404, NOT 204. Design decision "no expiry" makes delete the ONLY hygiene mechanism
		// there is, so "it is gone" and "it was never here" must be
		// distinguishable: a client retrying a delete it believes failed needs to
		// know whether its first attempt landed.
		writeErr(w, http.StatusNotFound, "no_such_clip",
			"no clip with id "+strconv.FormatInt(id, 10))
	default:
		log.Printf("clipstore: DELETE /v1/clips/%d: %v", id, err)
		writeErr(w, http.StatusServiceUnavailable, "store_unwritable",
			"the clip store could not be written")
	}
}

// healthz is PROBE-ONLY. No client calls it: the Android outbox is barred from
// probing by two of its own ledgers, and snip just attempts the fetch. Do
// not read its presence as a client-side reachability mechanism.
//
// 🔴 IT IS THE ONE UNAUTHENTICATED ROUTE, SO THE KUBELET CAN PROBE IT WITH A
// PLAIN httpGet. The alternative was gating it and having the binary probe
// itself over loopback, which is what this service used to do; an unauthenticated
// /healthz is the settled decision. Nothing about the clip routes
// changes — requireToken still covers all three and the catch-all.
//
// 🔴 IT STILL QUERIES THE DATABASE, and dropping the gate must not become
// dropping that. `Store.Ping` runs `SELECT count(*) FROM clips` rather than
// db.Ping(), so the statement is resolved against the SCHEMA and /healthz cannot
// answer 200 on a database whose `clips` table is gone. Do not "simplify" this
// to a static 200, and do not simplify Store.Ping to db.PingContext — two guards
// cover the two halves: TestHealthzReadsARowWithoutATokenAndFailsLoud pins that
// the HANDLER turns a store error into a 503 on the unauthenticated path, and
// TestStorePingReadsTheClipsTableNotTheConnection (store_test.go) pins that the
// real Store.Ping ERRORS for a state db.PingContext reports healthy.
//
// ⚠ THE SCOPE, STATED EXACTLY, BECAUSE THIS COMMENT USED TO OVER-CLAIM IT. This
// is a SCHEMA probe, NOT a storage probe. It was measured false that it catches
// "a pod listening with its volume missing": an already-open SQLite store serves
// `count(*)` from its page cache, so /healthz returned 200 with the db file
// unlinked, truncated to zero, overwritten in place with junk, and with the
// parent directory removed. Store.Ping's own comment records the measurements.
// What the probe genuinely reports is "this process can still resolve and query
// its `clips` table" — which catches a lost or half-migrated schema, and nothing
// about the file underneath it.
func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		log.Printf("clipstore: GET /healthz: %v", err)
		writeErr(w, http.StatusServiceUnavailable, "store_unreadable",
			"the clip store could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type errorResponse struct {
	// Error is a STABLE MACHINE-READABLE REASON, and the clients surface it
	// verbatim. The set is closed: unauthorized, invalid_json, empty_body,
	// body_too_large, missing_device, device_too_long, request_too_large,
	// invalid_limit, invalid_id, no_such_clip, store_unreadable,
	// store_unwritable, and for the change feed invalid_after, invalid_wait,
	// invalid_feed, too_many_waiters, and contains_server_token.
	Error string `json:"error"`
	// Detail is prose for a human reading a log or a toast. Never parsed.
	Detail string `json:"detail,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Unreachable for the concrete types above, which is why it must not
		// pretend to recover: a half-written 200 is worse than a 500.
		log.Printf("clipstore: marshalling a %T failed: %v", payload, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// 🔴 The bodies here are a user's clipboard. Nothing may cache them.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
