package main

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// waitersInFlight polls until n long-polls hold a slot.
func waitersInFlight(t *testing.T, api *API, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(api.waiters) < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d waiters arrived", len(api.waiters), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWaitersAreBoundedAndTheNextOneIsRefusedAtOnce(t *testing.T) {
	srv, api := feedServer(t)
	api.waiters = make(chan struct{}, 3) // a small bound for the test
	c := postClip(t, srv, "seed", "phone")
	q := "after=" + strconv.FormatInt(c.ID, 10) + "&wait=30"

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); feed(t, srv, q) }()
	}
	waitersInFlight(t, api, 3)

	code, _, took := feed(t, srv, q)
	if code != http.StatusTooManyRequests || took > 2*time.Second {
		t.Fatalf("the waiter over the bound got %d after %v, want 429 at once", code, took)
	}
	// A non-waiting read is not a waiter and is never refused.
	if code, _, _ := feed(t, srv, "after=0"); code != http.StatusOK {
		t.Fatalf("an immediate feed read under a full waiter pool got %d, want 200", code)
	}
	postClip(t, srv, "wakes all three", "mcp") // releases the slots
	wg.Wait()
	if len(api.waiters) != 0 {
		t.Fatalf("%d slots leaked after the waiters returned", len(api.waiters))
	}
}

func TestShutdownReleasesWaitersWithAnEmptyAnswer(t *testing.T) {
	srv, api := feedServer(t)
	c := postClip(t, srv, "seed", "phone")
	done := make(chan feedBody, 1)
	go func() {
		_, fb, _ := feed(t, srv, "after="+strconv.FormatInt(c.ID, 10)+"&wait=30")
		done <- fb
	}()
	waitersInFlight(t, api, 1)
	api.Shutdown()
	select {
	case fb := <-done:
		if fb.Clips == nil || len(*fb.Clips) != 0 {
			t.Fatalf("released waiter got %+v, want clips: []", fb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not release the waiter")
	}
}

func TestTheWriteTimeoutOutlastsTheLongestWait(t *testing.T) {
	if writeTimeout < (MaxWaitSeconds+10)*time.Second {
		t.Fatalf("writeTimeout %v does not outlast a %ds long-poll plus its write; the server "+
			"would cut every full-length wait", writeTimeout, MaxWaitSeconds)
	}
}
