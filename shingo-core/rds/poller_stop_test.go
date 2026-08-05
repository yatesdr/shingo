package rds

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPollerStop_IsSynchronous asserts the contract directly rather than by
// waiting and hoping.
//
// TestPollerStopHaltsPolling proves the same property THROUGH wall-clock
// sleeps: stop, wait, and check nothing arrived. That is a fair thing to
// assert, but on a loaded box the sleeps were the flakiest part of the suite
// — about one run in eight — because an in-flight poll could outlast the
// window it was given to drain. Stop now waits for the loop, so the property
// can be checked without any timing at all: count the requests the moment Stop
// returns, and no later request is possible.
func TestPollerStop_IsSynchronous(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	// A deliberately SLOW handler, so a poll is reliably in flight when Stop is
	// called. This is the exact case the old asynchronous Stop could not cover:
	// the poll carried on to completion after Stop returned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewPoller(NewClient(srv.URL, 5*time.Second), &mockPollerEmitter{}, &mockResolver{}, 5*time.Millisecond)
	p.Track("rds-1")
	p.Start()

	deadline := time.After(2 * time.Second)
	for requests.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no poll observed; the loop never ran")
		case <-time.After(2 * time.Millisecond):
		}
	}

	p.Stop()
	atStop := requests.Load()

	// No sleep, no tolerance: Stop returning IS the guarantee. Anything arriving
	// after this point means the loop outlived the call that stopped it.
	time.Sleep(150 * time.Millisecond)
	if after := requests.Load(); after != atStop {
		t.Errorf("polls continued after Stop returned: %d at Stop, %d after", atStop, after)
	}
}

// TestPollerStop_BeforeStartDoesNotBlock covers the case the `started` gate
// exists for. Stop is documented as safe before Start, and a naive wait on the
// done channel would hang forever on a loop that was never launched — turning
// a no-op into a deadlock.
func TestPollerStop_BeforeStartDoesNotBlock(t *testing.T) {
	t.Parallel()

	p := NewPoller(NewClient("http://localhost:1", time.Second), &mockPollerEmitter{}, &mockResolver{}, time.Minute)

	done := make(chan struct{})
	go func() {
		p.Stop()
		p.Stop() // and again: stopOnce guards the close, the wait is re-entrant
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked when the poller had never been started")
	}
}

// TestPollerStop_TwiceAfterStartDoesNotBlock is the same guard on the other
// path: once the loop has exited, doneChan stays closed, so a second Stop
// returns immediately rather than waiting out stopDrainLimit.
func TestPollerStop_TwiceAfterStartDoesNotBlock(t *testing.T) {
	t.Parallel()

	p := NewPoller(NewClient("http://localhost:1", time.Second), &mockPollerEmitter{}, &mockResolver{}, 5*time.Millisecond)
	p.Start()
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		p.Stop()
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second Stop blocked; it should return at once once the loop is done")
	}
}
