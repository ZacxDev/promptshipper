package main

// The change feed: GET /v1/clips?after=<seq>&wait=<seconds> (long-poll). It
// returns full clips, so no separate fetch-by-id route exists.
//
// HOW A WAITER IS WOKEN, AND WHY IT CANNOT MISS A POST. `changes` hands out the
// CURRENT generation channel; every successful Put closes it and installs a
// new one. A waiter takes the channel BEFORE it queries the store, then waits
// on it only if the query was empty. A Put that commits after the query closes
// the channel the waiter already holds, so the wake-up cannot be lost; a Put
// that committed before the query is simply in the query's result.
//
// WHY THIS IS SAFE WITH ONE SQLITE WRITER. A waiter holds NO database
// connection while it waits: it runs one short SELECT, releases the
// connection, and blocks on a channel. The store's single connection is only
// ever held for the length of a query, so waiters cannot starve writers.
//
// THE BOUND. At most MaxWaiters long-polls wait at once (each is a goroutine
// and an open socket); one more is refused 429 `too_many_waiters` at once,
// without waiting, so a misbehaving client cannot pin the server.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// MaxWaitSeconds caps `wait`. 60 s keeps one poll inside the idle timeouts
	// of common mobile carriers and proxies; the client simply polls again.
	MaxWaitSeconds = 60
	// MaxWaiters bounds concurrent long-polls. A personal clipboard has a
	// handful of devices; 64 is generous and still cheap.
	MaxWaiters = 64
)

// changes is the wake-up broadcaster for long-poll waiters.
type changes struct {
	mu      sync.Mutex
	gen     chan struct{}
	stopped bool
}

func newChanges() *changes { return &changes{gen: make(chan struct{})} }

// current is the channel that will be closed by the NEXT change.
func (c *changes) current() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// notify wakes every waiter holding the current channel.
func (c *changes) notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	close(c.gen)
	c.gen = make(chan struct{})
}

// stop wakes every waiter for good (shutdown): later waiters return at once.
func (c *changes) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		c.stopped = true
		close(c.gen)
	}
}

// Shutdown releases every long-poll waiter so http.Server.Shutdown does not
// sit out a 60 s wait. Each released waiter answers its normal empty 200.
func (a *API) Shutdown() { a.changes.stop() }

// feedClips serves GET /v1/clips when `after` is present.
func (a *API) feedClips(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	after, err := strconv.ParseInt(qs.Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeErr(w, http.StatusBadRequest, "invalid_after", "`after` must be a non-negative integer seq (the largest `seq` seen)")
		return
	}
	if qs.Has("q") {
		writeErr(w, http.StatusBadRequest, "invalid_feed",
			"`after` (the change feed) cannot be combined with `q` (search)")
		return
	}
	wait := 0
	if raw := qs.Get("wait"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > MaxWaitSeconds {
			writeErr(w, http.StatusBadRequest, "invalid_wait",
				"`wait` must be an integer number of seconds in 0.."+strconv.Itoa(MaxWaitSeconds))
			return
		}
		wait = n
	}
	limit := DefaultLimit
	if raw := qs.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxLimit {
			writeErr(w, http.StatusBadRequest, "invalid_limit",
				"`limit` must be an integer in 1.."+strconv.Itoa(MaxLimit))
			return
		}
		limit = n
	}

	if wait > 0 {
		select {
		case a.waiters <- struct{}{}:
			defer func() { <-a.waiters }()
		default:
			w.Header().Set("Retry-After", "5")
			writeErr(w, http.StatusTooManyRequests, "too_many_waiters",
				"too many long-polls are already waiting; retry shortly")
			return
		}
	}

	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		woken := a.changes.current() // BEFORE the query: see the file comment
		clips, err := a.store.After(r.Context(), after, limit)
		if err != nil {
			if errors.Is(r.Context().Err(), context.Canceled) {
				return // the client went away
			}
			log.Printf("clipstore: GET /v1/clips?after=%d: %v", after, err)
			writeErr(w, http.StatusServiceUnavailable, "store_unreadable", "the clip store could not be read")
			return
		}
		if len(clips) > 0 || wait == 0 {
			writeJSON(w, http.StatusOK, listClipsResponse{Clips: clips, Limit: limit})
			return
		}
		select {
		case <-woken:
			if a.changes.isStopped() {
				writeJSON(w, http.StatusOK, listClipsResponse{Clips: []Clip{}, Limit: limit})
				return
			}
		case <-deadline.C:
			writeJSON(w, http.StatusOK, listClipsResponse{Clips: []Clip{}, Limit: limit})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (c *changes) isStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}
