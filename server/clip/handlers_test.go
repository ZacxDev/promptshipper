// Handler tests.
//
// 🔴 WHAT THESE TESTS ARE. handlers.go is a NEW FILE in a NEW SERVICE, so every
// test in this file is an INVARIANT GUARD / CHARACTERIZATION TEST and NOT
// regression coverage: there is no pre-change code for them to be red against,
// and none of them has ever caught a bug that shipped, because nothing has
// shipped. They pin behaviour so a later change has to argue with them.
//
// THE ONES THAT CAN BE WATCHED FAIL, with the mutation that does it — reported
// as observed (see the PR body for the run):
//
//   - TestTheRouteLedgerIsExactlyTheseRoutes — add a route to either half of
//     handlers.go's routeTable, or move one between the halves, and this goes
//     red naming the set that changed. It compares against LITERAL lists, so it
//     fails when a set GROWS and when it SHRINKS.
//   - TestOnlyHealthzIsReachableWithoutTheBearerToken — move a clip route from
//     `gated` into `public` (the one-line mistake the two-mux shape makes
//     possible) and this goes red with ITS OWN message, naming the route that
//     answered without a token.
//   - TestTheLoudFailureCheckSeesAnErrorBodyCarryingAResult — the ISOLATING
//     CONTROL for the raw-body half of the "never an empty list" check. A 503
//     with the WRONG error code can never reach that loop, so before this
//     control existed the loop was unreachable scaffolding that read as
//     coverage. Its own comment carries the measurement.
//   - TestALimitOverTheCapIsRefusedNotClamped — change the handler's
//     `n > MaxLimit` refusal to `n = MaxLimit` (the obvious "be nice to clients"
//     edit) and this goes red naming the 200 it should not have received. A test
//     that only asserted "at most MaxLimit rows came back" would stay GREEN for
//     that mutant, which is why this one asserts the STATUS CODE.
//   - TestAnUnreadableStoreIs5xxWithANamedReasonAndNeverAnEmptyList — make any
//     error branch in handlers.go `writeJSON(w, 200, listClipsResponse{})` and
//     this is the only test in the package that notices.
//
// 🔴 THE FAILING-STORE FAKE IS THE REACHABILITY MECHANISM. The "fails loud,
// never empty" posture lives entirely in error branches that a real *Store on a
// healthy temp file CANNOT enter, so without this fake those branches would be
// untested code asserting the feature's headline guarantee. That is what the
// `clipStore` interface in handlers.go is for.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-at-least-24-bytes-long"

// fakeStore is a store that always fails, with a distinguishable error.
//
// It is NOT a partial implementation that "mostly works": a fake that sometimes
// succeeds would let a handler test pass through the happy path and never reach
// the branch under test.
type failingStore struct{ err error }

func (f failingStore) Put(context.Context, string, string, int64) (Clip, bool, error) {
	return Clip{}, false, f.err
}
func (f failingStore) List(context.Context, int, string) ([]Clip, error) { return nil, f.err }
func (f failingStore) Delete(context.Context, int64) error               { return f.err }
func (f failingStore) Ping(context.Context) error                        { return f.err }

// recordingStore captures the arguments the handler passed down, so a test can
// assert the handler's own parsing (the limit default, say) rather than the
// store's behaviour.
type recordingStore struct {
	clips     []Clip
	lastLimit int
	lastQuery string
}

func (r *recordingStore) Put(_ context.Context, body, device string, at int64) (Clip, bool, error) {
	return Clip{ID: 1, Body: body, Device: device, CreatedAt: at}, false, nil
}
func (r *recordingStore) List(_ context.Context, limit int, q string) ([]Clip, error) {
	r.lastLimit, r.lastQuery = limit, q
	if r.clips == nil {
		return []Clip{}, nil
	}
	return r.clips, nil
}
func (r *recordingStore) Delete(context.Context, int64) error { return nil }
func (r *recordingStore) Ping(context.Context) error          { return nil }

func serve(t *testing.T, store clipStore, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	api := NewAPI(store, testToken, func() time.Time { return time.UnixMilli(1700000000000) })
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	return rec
}

func authed(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	return r
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var out errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response body %q is not an error object: %v", rec.Body.String(), err)
	}
	return out
}

// probeFor maps a routeTable PATTERN to a concrete request, because a pattern
// containing a {wildcard} is not a URL and cannot be requested.
//
// 🔴 IT IS DELIBERATELY NOT EXHAUSTIVE-BY-DEFAULT. A pattern with no entry here
// makes the tests below FAIL rather than skip, so a route added to routeTable
// without a probe case is a red test instead of a silent hole in the coverage
// that the route-set assertion appears to provide.
var probeFor = map[string]struct{ method, target, body string }{
	"POST /v1/clips":        {"POST", "/v1/clips", `{"body":"x","device":"d"}`},
	"GET /v1/clips":         {"GET", "/v1/clips", ""},
	"DELETE /v1/clips/{id}": {"DELETE", "/v1/clips/1", ""},
	"GET /healthz":          {"GET", "/healthz", ""},
}

// request builds a request from a probe spec, optionally with an Authorization
// header value.
func request(p struct{ method, target, body string }, authorization string) *http.Request {
	var r *http.Request
	if p.body == "" {
		r = httptest.NewRequest(p.method, p.target, nil)
	} else {
		r = httptest.NewRequest(p.method, p.target, strings.NewReader(p.body))
	}
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

// patterns pulls the Pattern field out of a routeTable half.
func patterns(rs []muxRoute) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Pattern)
	}
	return out
}

// 🔴 CAN BE WATCHED FAIL — see the header. THE STRUCTURAL GUARD, and the reason
// it is structural rather than spelled: it asserts the ENUMERATED SET of routes
// on each side of the auth boundary against a LITERAL list, so it goes red when
// either set GROWS or SHRINKS.
//
// A guard phrased as "no route named /healthz requires a token" would be
// walkable — a future route can spell anything, and `/healthz2` or
// `/v1/clips/export` would pass it while being unauthenticated. The set is what
// matters, so the set is what is pinned. The expected lists below are literals
// written by hand; the lists under test come from handlers.go's routeTable,
// which is the same data the server actually routes from.
func TestTheRouteLedgerIsExactlyTheseRoutes(t *testing.T) {
	api := NewAPI(&recordingStore{}, testToken, time.Now)
	gated, public := api.routeTable()

	// 🔴 LITERALS, not anything derived from the implementation. If a route is
	// added, moved or removed, one of these two assertions fails and whoever did
	// it has to state here which side of the auth boundary it belongs on.
	wantGated := []string{
		"POST /v1/clips",
		"GET /v1/clips",
		"DELETE /v1/clips/{id}",
	}
	wantPublic := []string{
		"GET /healthz",
	}

	if got := patterns(gated); !slices.Equal(got, wantGated) {
		t.Fatalf("the BEARER-GATED route set is %v, want %v — a route appearing here or "+
			"disappearing from here changes what the token protects, and this test is the "+
			"place to say so deliberately", got, wantGated)
	}
	if got := patterns(public); !slices.Equal(got, wantPublic) {
		t.Fatalf("the UNAUTHENTICATED route set is %v, want %v — every entry in this set is "+
			"reachable by any host on the network with no credential at all, so it may "+
			"only ever hold the kubelet's probe endpoint", got, wantPublic)
	}

	// Every pattern in the table must have a probe case, or the behavioural
	// tests below would silently stop covering it.
	for _, p := range append(patterns(gated), patterns(public)...) {
		if _, ok := probeFor[p]; !ok {
			t.Fatalf("route %q has no entry in probeFor, so no test drives it over HTTP — add "+
				"one rather than leaving the route-set assertion to look like coverage", p)
		}
	}
}

// 🔴 CAN BE WATCHED FAIL — see the header. The BEHAVIOURAL half of the guard
// above: a structural set assertion type-checks past a route that is wired to
// the wrong mux, so this one actually drives every route in the table over HTTP
// with no credential and with five kinds of bad one.
//
// It reads the route sets from routeTable rather than from its own list, so a
// route added to `public` is probed by this test automatically — it does not
// have to be remembered twice.
func TestOnlyHealthzIsReachableWithoutTheBearerToken(t *testing.T) {
	api := NewAPI(&recordingStore{}, testToken, time.Now)
	gated, public := api.routeTable()

	headers := []struct {
		name, value string
	}{
		{"no Authorization header at all", ""},
		{"a wrong token", "Bearer wrong-token-of-a-plausible-length"},
		{"the right token under the wrong scheme", "Basic " + testToken},
		{"an empty Bearer credential", "Bearer "},
		{"a token with the right prefix but extra bytes", "Bearer " + testToken + "x"},
		{"a token that is a prefix of the right one", "Bearer " + testToken[:10]},
	}

	// A store that would HAPPILY SERVE if the request got through: a failing
	// store would answer 503 and the test would pass whether auth ran or not.
	store := &recordingStore{}

	for _, route := range gated {
		p, ok := probeFor[route.Pattern]
		if !ok {
			// 🔴 FAIL, DO NOT PANIC. The zero value of the probe struct is an
			// empty method and an empty target, and httptest.NewRequest PANICS on
			// those — which aborts the whole test binary and scores as a crash
			// rather than as this test's own verdict. Measured while mutation-
			// testing a route added with no probe case.
			t.Fatalf("gated route %q has no probeFor entry, so nothing drives it over HTTP",
				route.Pattern)
		}
		for _, h := range headers {
			t.Run(fmt.Sprintf("gated %s with %s", route.Pattern, h.name), func(t *testing.T) {
				rec := serve(t, store, request(p, h.value))
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("gated route %s answered %d without a valid token, want 401 — this "+
						"endpoint is reachable from every host on the network",
						route.Pattern, rec.Code)
				}
				if got := decodeErr(t, rec).Error; got != "unauthorized" {
					t.Fatalf("error code is %q, want %q", got, "unauthorized")
				}
			})
		}
	}

	// The INVERSE, and the half that makes this a guard on the new property
	// rather than the old one: the public routes must serve with NO credential.
	// Without this the whole file would be satisfied by re-gating /healthz.
	for _, route := range public {
		p, ok := probeFor[route.Pattern]
		if !ok {
			// Same reason as above, and it matters MORE here: an unauthenticated
			// route added with no probe case is the one this file most needs to
			// see, so it must report itself rather than crash the binary.
			t.Fatalf("UNAUTHENTICATED route %q has no probeFor entry, so nothing drives it "+
				"over HTTP — every ungated route must be exercised here", route.Pattern)
		}
		t.Run("public "+route.Pattern+" with no credential at all", func(t *testing.T) {
			rec := serve(t, store, request(p, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("public route %s answered %d with NO Authorization header, want 200 — "+
					"the kubelet probes it with a plain httpGet and cannot send a secret "+
					"header, so re-gating it breaks every probe in the Deployment",
					route.Pattern, rec.Code)
			}
		})
		// And a GARBAGE credential must not break it either: a probe never
		// sends one, but a wrong-token 401 here would be the same outage.
		t.Run("public "+route.Pattern+" with a garbage credential", func(t *testing.T) {
			rec := serve(t, store, request(p, "Bearer not-the-token-at-all-but-long-enough"))
			if rec.Code != http.StatusOK {
				t.Fatalf("public route %s answered %d with a WRONG token, want 200 — it is "+
					"ungated, so a credential must be ignored rather than rejected",
					route.Pattern, rec.Code)
			}
		})
	}
}

// Invariant guard. A missing header and a wrong token must be the SAME answer:
// telling them apart is free information for anything probing the listener.
func TestMissingAndWrongTokensAreIndistinguishable(t *testing.T) {
	store := &recordingStore{}

	missing := serve(t, store, httptest.NewRequest("GET", "/v1/clips", nil))
	wrongReq := httptest.NewRequest("GET", "/v1/clips", nil)
	wrongReq.Header.Set("Authorization", "Bearer definitely-not-the-token")
	wrong := serve(t, store, wrongReq)

	if missing.Code != wrong.Code || missing.Body.String() != wrong.Body.String() {
		t.Fatalf("a missing token answers %d %q and a wrong one answers %d %q — they must be "+
			"byte-identical", missing.Code, missing.Body.String(), wrong.Code, wrong.Body.String())
	}
}

// Invariant guard. RFC 7235 makes the auth scheme case-insensitive and both
// clients pass the header through verbatim, so `bearer` must work.
//
// 🔴 THE TARGET IS A GATED ROUTE, AND IT HAS TO BE. This test used to probe
// /healthz, which is now ungated and answers 200 for EVERY scheme including
// `Basic` and nonsense — so against /healthz this test would pass without
// bearer() being called at all. That is the exact shape of a test that reads as
// coverage and provides none.
func TestTheBearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/clips", nil)
			req.Header.Set("Authorization", scheme+" "+testToken)
			if rec := serve(t, &recordingStore{}, req); rec.Code != http.StatusOK {
				t.Fatalf("scheme %q answered %d on a GATED route, want 200", scheme, rec.Code)
			}
		})
	}
	// The negative control for the loop above: a scheme that is NOT bearer must
	// be refused on the same route. Without it, a `bearer()` that returned
	// (token, true) for anything would keep this test green.
	req := httptest.NewRequest("GET", "/v1/clips", nil)
	req.Header.Set("Authorization", "Basic "+testToken)
	if rec := serve(t, &recordingStore{}, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the right token under the `Basic` scheme answered %d on a gated route, "+
			"want 401 — case-insensitive must not mean scheme-insensitive", rec.Code)
	}
}

// Invariant guard on bearer() itself, with literal inputs.
func TestBearerParsing(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"Bearer   abc  ", "abc", true},
		{"Bearer ", "", false},
		{"Bearer", "", false},
		{"Basic abc", "", false},
		{"abc", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := bearer(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("bearer(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Invariant guard. The POST path, against the real store, so the response shape
// and the dedup status codes are pinned together.
func TestPostAndDedupStatusCodes(t *testing.T) {
	store := newTestStore(t)
	api := NewAPI(store, testToken, func() time.Time { return time.UnixMilli(1700000000000) })

	first := httptest.NewRecorder()
	api.Handler().ServeHTTP(first, authed("POST", "/v1/clips", `{"body":"x","device":"pixel-7"}`))
	if first.Code != http.StatusCreated {
		t.Fatalf("the first POST answered %d, want 201: %s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	api.Handler().ServeHTTP(second, authed("POST", "/v1/clips", `{"body":"x","device":"laptop"}`))
	if second.Code != http.StatusOK {
		t.Fatalf("a dedup POST answered %d, want 200: %s", second.Code, second.Body.String())
	}
	var out postClipResponse
	if err := json.Unmarshal(second.Body.Bytes(), &out); err != nil {
		t.Fatalf("dedup response %q: %v", second.Body.String(), err)
	}
	if !out.Deduped || out.BumpCount != 1 {
		t.Fatalf("dedup response is deduped=%v bump_count=%d, want true and 1",
			out.Deduped, out.BumpCount)
	}
}

// Invariant guard. Every refusal names a stable machine-readable reason; the
// clients surface it verbatim.
func TestPostRefusesMalformedRequestsWithNamedReasons(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		reason     string
	}{
		{"not JSON", `not json at all`, http.StatusBadRequest, "invalid_json"},
		{"a JSON array", `[]`, http.StatusBadRequest, "invalid_json"},
		{"an empty body field", `{"body":"","device":"d"}`, http.StatusBadRequest, "empty_body"},
		{"no body field", `{"device":"d"}`, http.StatusBadRequest, "empty_body"},
		{"no device field", `{"body":"x"}`, http.StatusBadRequest, "missing_device"},
		{"a blank device", `{"body":"x","device":"   "}`, http.StatusBadRequest, "missing_device"},
		{
			"an over-long device",
			`{"body":"x","device":"` + strings.Repeat("d", MaxDeviceBytes+1) + `"}`,
			http.StatusBadRequest, "device_too_long",
		},
		{
			"an over-size clip body",
			`{"body":"` + strings.Repeat("b", MaxBodyBytes+1) + `","device":"d"}`,
			http.StatusBadRequest, "body_too_large",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, &recordingStore{}, authed("POST", "/v1/clips", tc.body))
			if rec.Code != tc.status {
				t.Fatalf("answered %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if got := decodeErr(t, rec).Error; got != tc.reason {
				t.Fatalf("error code is %q, want %q", got, tc.reason)
			}
		})
	}
}

// Invariant guard. The whole envelope is bounded too, so an enormous request
// cannot be read into memory before the body-size check sees it.
func TestAnEnormousRequestIsRefusedBeforeItIsParsed(t *testing.T) {
	huge := `{"body":"` + strings.Repeat("b", MaxRequestBytes+1024) + `","device":"d"}`
	rec := serve(t, &recordingStore{}, authed("POST", "/v1/clips", huge))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("answered %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec).Error; got != "request_too_large" {
		t.Fatalf("error code is %q, want %q", got, "request_too_large")
	}
}

// Invariant guard. Absent `limit` means DefaultLimit reaching the store — a
// handler that passed 0 would return nothing and look like an empty store.
func TestTheDefaultLimitReachesTheStore(t *testing.T) {
	store := &recordingStore{}
	if rec := serve(t, store, authed("GET", "/v1/clips", "")); rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body.String())
	}
	if store.lastLimit != DefaultLimit {
		t.Fatalf("the store was asked for %d rows with no ?limit=, want DefaultLimit=%d",
			store.lastLimit, DefaultLimit)
	}
}

// 🔴 CAN BE WATCHED FAIL — see the header. It asserts the STATUS CODE, which is
// what kills the "clamp instead of refuse" mutant; a row-count assertion would
// survive it.
func TestALimitOverTheCapIsRefusedNotClamped(t *testing.T) {
	store := &recordingStore{}
	// MaxLimit+1 is the boundary, 5000 is well past it. Both, because a mutant
	// that used `>=` or `>` wrongly would only move one of them.
	for _, limit := range []string{fmt.Sprint(MaxLimit + 1), "5000"} {
		t.Run("limit="+limit, func(t *testing.T) {
			rec := serve(t, store, authed("GET", "/v1/clips?limit="+limit, ""))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("?limit=%s answered %d, want 400. A clamp-and-200 is NOT an acceptable "+
					"alternative: the client cannot tell a truncated newest-first list from a "+
					"store that holds only %d rows", limit, rec.Code, MaxLimit)
			}
			if got := decodeErr(t, rec).Error; got != "invalid_limit" {
				t.Fatalf("error code is %q, want %q", got, "invalid_limit")
			}
		})
	}
	// And the boundary from BELOW is accepted, so the test is not green merely
	// because every limit is refused.
	rec := serve(t, store, authed("GET", "/v1/clips?limit="+fmt.Sprint(MaxLimit), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("?limit=%d answered %d, want 200 — the cap itself must be accepted",
			MaxLimit, rec.Code)
	}
	if store.lastLimit != MaxLimit {
		t.Fatalf("the store was asked for %d rows at the cap, want %d", store.lastLimit, MaxLimit)
	}
}

// Invariant guard. A non-integer, zero or negative limit is a refusal too.
func TestAnUnusableLimitIsRefused(t *testing.T) {
	for _, limit := range []string{"0", "-1", "abc", "1.5", "", "+5x"} {
		t.Run("limit="+limit, func(t *testing.T) {
			rec := serve(t, &recordingStore{}, authed("GET", "/v1/clips?limit="+limit, ""))
			if limit == "" {
				// An EMPTY ?limit= is "absent", which is the default, not an error.
				if rec.Code != http.StatusOK {
					t.Fatalf("an empty ?limit= answered %d, want 200 (treated as absent)", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("?limit=%s answered %d, want 400", limit, rec.Code)
			}
		})
	}
}

// loudFailureViolation reports why a response is NOT a loud failure, or "" when
// it is one. It is the "fails loud, never empty" contract in one place:
//
//  1. the status is a 5xx;
//  2. the error code is the expected stable reason;
//  3. the RAW body carries no result field and no empty collection.
//
// 🔴 IT RETURNS A STRING INSTEAD OF CALLING t.Fatalf, AND THAT IS THE WHOLE
// POINT. Check 3 can only be reached by a response that PASSES checks 1 and 2,
// and no mutation of handlers.go produces one — every plausible mutant trips
// check 1 or check 2 first. So as an inline assertion check 3 was unreachable.
// As a function it can be fed a crafted response directly, which is what
// TestTheLoudFailureCheckSeesAnErrorBodyCarryingAResult does.
func loudFailureViolation(rec *httptest.ResponseRecorder, wantReason string) string {
	body := rec.Body.String()
	if rec.Code < 500 {
		return fmt.Sprintf("status is %d, want a 5xx. Body: %s", rec.Code, body)
	}
	var out errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return fmt.Sprintf("body %q is not an error object: %v", body, err)
	}
	if out.Error != wantReason {
		return fmt.Sprintf("error code is %q, want %q", out.Error, wantReason)
	}
	// 🔴 THE THIRD CHECK, AND THE ONE A STATUS-AND-CODE ASSERTION MISSES: the
	// body must not be, or contain, an empty result. A handler that wrote 503
	// with the CORRECT reason AND `{"clips":[]}` satisfies both checks above and
	// still hands a client something it can render as "no clips" — which is the
	// precise confusion this service exists not to cause. That mutant is a
	// natural edit, not a contrived one: "include an empty list so the client
	// does not crash" is a change somebody makes on purpose.
	for _, forbidden := range []string{`"clips"`, `[]`} {
		if strings.Contains(body, forbidden) {
			return fmt.Sprintf("the error body contains %s: %s — an error must not carry a "+
				"result field at all", forbidden, body)
		}
	}
	return ""
}

// 🔴 CAN BE WATCHED FAIL — see the header. THE HEADLINE GUARANTEE OF THIS
// SERVICE: an unreadable store answers 5xx with a named reason and NEVER an
// empty list, because an empty result is indistinguishable from a working
// feature with nothing in it.
//
// The healthz case sends NO Authorization header — it is ungated now — while the
// three clip cases send a valid one, so each route is driven the way a real
// caller drives it.
func TestAnUnreadableStoreIs5xxWithANamedReasonAndNeverAnEmptyList(t *testing.T) {
	broken := failingStore{err: errors.New("disk I/O error")}
	cases := []struct {
		name, method, target, body, reason string
		authenticate                       bool
	}{
		{"list", "GET", "/v1/clips", "", "store_unreadable", true},
		{"search", "GET", "/v1/clips?q=anything", "", "store_unreadable", true},
		{"post", "POST", "/v1/clips", `{"body":"x","device":"d"}`, "store_unwritable", true},
		{"delete", "DELETE", "/v1/clips/1", "", "store_unwritable", true},
		{"healthz without a token", "GET", "/healthz", "", "store_unreadable", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.authenticate {
				req = authed(tc.method, tc.target, tc.body)
			} else {
				req = request(struct{ method, target, body string }{tc.method, tc.target, tc.body}, "")
			}
			rec := serve(t, broken, req)
			if why := loudFailureViolation(rec, tc.reason); why != "" {
				t.Fatalf("%s %s against an unreadable store: %s", tc.method, tc.target, why)
			}
		})
	}
}

// 🔴 THE ISOLATING CONTROL for check 3 of loudFailureViolation, and the reason
// it had to be built: the test above CANNOT reach that check.
//
// MEASURED, on this branch. Every mutant of handlers.go that would produce an
// error body carrying a result trips an EARLIER check and dies there, so
// deleting check 3 entirely left the suite green:
//
//   - `writeJSON(w, 200, listClipsResponse{})` in an error branch → body is
//     `{"clips":[],"limit":0}` but the status is 200, so check 1 fires first.
//   - `writeJSON(w, 503, listClipsResponse{})` → the status passes, but the
//     error code decodes to "" and check 2 fires ("error code is \"\", want
//     \"store_unreadable\"") three lines above the raw-body loop.
//
// A response that reaches check 3 must carry the CORRECT reason AND a result,
// and nothing but a hand-crafted body does both. So the control crafts them.
//
// Each case names a DIFFERENT forbidden token, so the loop's list is mutated one
// element at a time: dropping `"clips"` leaves case 1 green under a `[]`-only
// check, and dropping `[]` leaves case 2 unseen entirely. Neither case's body
// can satisfy the other's token — `results` is not `clips`, and case 1's `[]`
// is what case 2 relies on alone.
func TestTheLoudFailureCheckSeesAnErrorBodyCarryingAResult(t *testing.T) {
	cases := []struct {
		name, body, wantMention string
	}{
		{
			// The natural mutant: the right 503, the right reason, and an empty
			// list bolted on "so the client does not crash".
			name:        "a correct 503 carrying clips:[]",
			body:        `{"error":"store_unreadable","detail":"the clip store could not be read","clips":[]}`,
			wantMention: `"clips"`,
		},
		{
			// The same mistake under a different field name, which a guard
			// spelling only `"clips"` would walk straight past.
			name:        "a correct 503 carrying a differently-named empty result",
			body:        `{"error":"store_unreadable","detail":"the clip store could not be read","results":[]}`,
			wantMention: `[]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rec.Code = http.StatusServiceUnavailable
			rec.Body.WriteString(tc.body)

			why := loudFailureViolation(rec, "store_unreadable")
			if why == "" {
				t.Fatalf("loudFailureViolation accepted a 503 whose body is %s — check 3 is "+
					"not firing, so the \"never an empty list\" half of this service's "+
					"headline guarantee has no coverage at all", tc.body)
			}
			// 🔴 THE REJECTION MUST NAME THE TOKEN THAT MATCHED — a PREFIX match,
			// not strings.Contains over the whole message.
			//
			// MEASURED: a Contains check here SURVIVES the mutant that drops
			// `"clips"` from the forbidden list. The message embeds the offending
			// BODY, and case 1's body contains the literal `"clips"`, so
			// Contains(why, `"clips"`) is satisfied by the echoed body even when
			// `[]` is the token that actually fired. That is the "fixture is not
			// distinct from a constant the assertion names" trap, in the
			// assertion rather than the fixture. The prefix pins which branch of
			// the loop ran.
			wantPrefix := "the error body contains " + tc.wantMention + ":"
			if !strings.HasPrefix(why, wantPrefix) {
				t.Fatalf("loudFailureViolation rejected the body with %q, but it must be rejected "+
					"with a message beginning %q — a rejection from check 1 or check 2, or on "+
					"the other forbidden token, would leave this one unproven",
					why, wantPrefix)
			}
		})
	}

	// THE POSITIVE/NEGATIVE PAIR. The real error body — the one the handlers
	// actually write — must be ACCEPTED, or the control above would pass against
	// a check that rejects everything and proves nothing.
	clean := httptest.NewRecorder()
	clean.Code = http.StatusServiceUnavailable
	clean.Body.WriteString(`{"error":"store_unreadable","detail":"the clip store could not be read"}`)
	if why := loudFailureViolation(clean, "store_unreadable"); why != "" {
		t.Fatalf("loudFailureViolation rejected the error body the handlers really write: %s — "+
			"a check that refuses everything cannot distinguish anything", why)
	}
}

// Invariant guard. The counterpart to the test above: an EMPTY store is a 200
// and an empty ARRAY (never `null`), because that is a legitimate state and the
// clients must be able to tell it from a failure.
func TestAnEmptyStoreIs200AndAnEmptyArrayNotNull(t *testing.T) {
	rec := serve(t, &recordingStore{}, authed("GET", "/v1/clips", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", rec.Code)
	}
	// The RAW BODY, not a decoded struct: `null` and `[]` both unmarshal into a
	// nil slice, so a decode-then-assert test cannot see the difference that
	// matters to a JavaScript or Kotlin client.
	if got, want := rec.Body.String(), `{"clips":[],"limit":100}`; got != want {
		t.Fatalf("body is %s, want %s — `null` would throw in a client that iterates it",
			got, want)
	}
}

// Invariant guard. `q` is passed to the store verbatim; the handler does no
// matching of its own (one rule, one place: PlanSearch owns it).
func TestTheQueryIsPassedThroughVerbatim(t *testing.T) {
	store := &recordingStore{}
	serve(t, store, authed("GET", "/v1/clips?q=ssh+prod&limit=7", ""))
	if store.lastQuery != "ssh prod" {
		t.Fatalf("the store received q=%q, want %q", store.lastQuery, "ssh prod")
	}
	if store.lastLimit != 7 {
		t.Fatalf("the store received limit=%d, want 7", store.lastLimit)
	}
}

// Invariant guard. Delete-by-id, end to end over HTTP, with 404 for an absent id.
func TestDeleteOverHTTP(t *testing.T) {
	store := newTestStore(t)
	api := NewAPI(store, testToken, time.Now)
	clip, _ := put(t, store, "x", "d", 1000)

	gone := httptest.NewRecorder()
	api.Handler().ServeHTTP(gone, authed("DELETE", fmt.Sprintf("/v1/clips/%d", clip.ID), ""))
	if gone.Code != http.StatusNoContent {
		t.Fatalf("DELETE of a present id answered %d, want 204: %s", gone.Code, gone.Body.String())
	}

	again := httptest.NewRecorder()
	api.Handler().ServeHTTP(again, authed("DELETE", fmt.Sprintf("/v1/clips/%d", clip.ID), ""))
	if again.Code != http.StatusNotFound {
		t.Fatalf("DELETE of an absent id answered %d, want 404", again.Code)
	}
	if got := decodeErr(t, again).Error; got != "no_such_clip" {
		t.Fatalf("error code is %q, want %q", got, "no_such_clip")
	}
}

// Invariant guard. A malformed id is a 400, not a 404 and not a panic.
func TestAMalformedDeleteIDIsRefused(t *testing.T) {
	for _, id := range []string{"abc", "0", "-1", "1.5", "99999999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			rec := serve(t, &recordingStore{}, authed("DELETE", "/v1/clips/"+id, ""))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("DELETE /v1/clips/%s answered %d, want 400", id, rec.Code)
			}
			if got := decodeErr(t, rec).Error; got != "invalid_id" {
				t.Fatalf("error code is %q, want %q", got, "invalid_id")
			}
		})
	}
}

// Invariant guard, and the one that pins BOTH halves of the settled decision
// together: /healthz needs no token AND still reads a row.
//
// 🔴 THE TWO HALVES MUST BE ONE TEST. Dropping the gate and turning /healthz
// into a static 200 would each satisfy a test that checked only the other half.
// Every request here is sent with NO Authorization header, so the 503 case also
// proves the store is reached on the unauthenticated path rather than only on
// the gated one.
//
// 🔴 WHAT THIS TEST DOES *NOT* PROVE, STATED BECAUSE IT USED TO BE CITED FOR IT.
// It uses the failingStore FAKE, so it can only show that the HANDLER turns a
// store error into a 503 with a named reason. It says nothing about whether the
// real Store.Ping ever errors — and it was measured that mutating Store.Ping's
// body to `return s.db.PingContext(ctx)` SURVIVED the whole suite while this
// test was the one cited against that simplification.
// TestStorePingReadsTheClipsTableNotTheConnection (store_test.go) is the guard
// on the real implementation, and the two are not interchangeable.
//
// ⚠ AND THE RETRACTED CLAIM: this comment used to say the pair prevents "a pod
// that is listening while its volume is gone, reported healthy forever". That
// was measured FALSE — an already-open SQLite store serves count(*) from its
// page cache, so /healthz answers 200 with the db file unlinked, truncated,
// overwritten in place or its directory removed. See Store.Ping's comment for
// the four measurements. /healthz is a SCHEMA probe.
func TestHealthzReadsARowWithoutATokenAndFailsLoud(t *testing.T) {
	ok := serve(t, &recordingStore{}, httptest.NewRequest("GET", "/healthz", nil))
	if ok.Code != http.StatusOK {
		t.Fatalf("/healthz on a healthy store with no token answered %d, want 200", ok.Code)
	}

	bad := serve(t, failingStore{err: errors.New("database disk image is malformed")},
		httptest.NewRequest("GET", "/healthz", nil))
	if bad.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz on a store whose Ping errors answered %d, want 503 — an "+
			"unconditional 200 would report a pod healthy while it cannot query its own "+
			"schema, and dropping the bearer gate must not become dropping the query",
			bad.Code)
	}
	// The NAMED reason, not just the code: the probe's failure is read out of a
	// kubelet event, and "503" alone does not say which of the two things broke.
	if got := decodeErr(t, bad).Error; got != "store_unreadable" {
		t.Fatalf("error code is %q, want %q", got, "store_unreadable")
	}
}

// 🔴 THE GUARD THAT MAKES THE CORRECTED MinTokenBytes CLAIM CHECKABLE. main.go's
// comment used to justify the token floor by saying an empty token "would accept
// `Authorization: Bearer ` from any mesh peer". Audit round 1 pointed out that
// two layers in THIS file already close that, so the floor is a diagnosis
// mechanism rather than an authorization fix — and a corrected comment is a claim
// like any other, so here it is pinned.
//
// 🔴 WHICH LAYER DOES IT MATTERS, because the obvious answer is wrong.
// subtle.ConstantTimeCompare of two EMPTY slices returns 1, so it is NOT the
// comparison that saves an empty configured token — it is bearer()'s `tok == ""`
// rejection. That is a single line with nothing behind it, which is why it is
// worth a test of its own rather than a sentence in a comment.
//
// An empty token is not a supported configuration (tokenRefusal refuses to
// start) — this pins what WOULD happen, so the comment explaining why the floor
// exists stays true.
func TestAnEmptyConfiguredTokenAuthenticatesNothing(t *testing.T) {
	store := &recordingStore{}
	api := NewAPI(store, "", time.Now)

	for _, tc := range []struct{ name, header string }{
		{"no Authorization header at all", ""},
		{"the scheme and a single space", "Bearer "},
		{"the bare scheme", "Bearer"},
		{"the scheme and only whitespace", "Bearer   "},
		{"lower-case scheme, empty credential", "bearer "},
		{"an empty credential the comparison WOULD match", "Bearer " + ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/clips", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			api.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("Authorization=%q against an EMPTY configured token answered %d, "+
					"want 401. An empty token must authenticate NOTHING: if this is a 200 the "+
					"listener is open to every host on the mesh, and MinTokenBytes becomes the "+
					"only thing standing there — which is a startup check, not a request "+
					"check.\nbody: %s", tc.header, rec.Code, rec.Body.String())
			}
			if got := decodeErr(t, rec).Error; got != "unauthorized" {
				t.Errorf("error code is %q, want %q", got, "unauthorized")
			}
		})
	}

	// The store must never have been reached. A 401 that still ran the query
	// would be a different and worse bug than a 401 that did not.
	if store.lastLimit != 0 || store.lastQuery != "" {
		t.Errorf("the store was called despite every request being refused "+
			"(lastLimit=%d lastQuery=%q)", store.lastLimit, store.lastQuery)
	}

	// 🔴 POSITIVE CONTROL: the same handler, the same path, a NON-empty token —
	// so the 401s above are the gate refusing, not a route that cannot answer at
	// all. Without this the test would pass identically for a mux that 401s
	// everything unconditionally.
	okAPI := NewAPI(&recordingStore{}, testToken, time.Now)
	okRec := httptest.NewRecorder()
	okAPI.Handler().ServeHTTP(okRec, authed("GET", "/v1/clips", ""))
	if okRec.Code != http.StatusOK {
		t.Fatalf("the positive control (a correct non-empty token on the same route) answered "+
			"%d, want 200 — the 401s above prove nothing about the EMPTY token specifically",
			okRec.Code)
	}
}

// Invariant guard. Clip bodies are a user's clipboard; nothing may cache them.
func TestResponsesAreNotCacheable(t *testing.T) {
	rec := serve(t, &recordingStore{}, authed("GET", "/v1/clips", ""))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control is %q, want %q", got, "no-store")
	}
}

// Invariant guard, and it guards the CATCH-ALL specifically. An unknown path
// and a wrong method on a known path must not become a success, and must not
// become a 404 either: the gated mux is mounted at "/", so anything the public
// patterns do not match lands behind the token and an unauthenticated prober
// cannot enumerate which routes exist.
//
// `POST /healthz` is in the list on purpose: the public pattern is METHOD-SCOPED
// ("GET /healthz"), so any other method on that path falls through to the
// catch-all and is gated. A pattern written as a bare "/healthz" would open
// every method on it, and that mistake fails here.
func TestUnknownRoutesAreStillGatedByTheToken(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{"GET", "/"},
		{"GET", "/v1/clips/1"},
		{"PUT", "/v1/clips"},
		{"POST", "/healthz"},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec := serve(t, &recordingStore{}, httptest.NewRequest(tc.method, tc.target, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s answered %d without a token, want 401 — the auth wrapper must "+
					"run before routing, or an unauthenticated prober can map the API",
					tc.method, tc.target, rec.Code)
			}
		})
	}
}
