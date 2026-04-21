package rds

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Reuses mockPollerEmitter, pollerEvent, and mockResolver defined in
// client_test.go, plus the testServer helper. The tests here focus on the
// Start/Stop lifecycle and the poll loop running against a live server,
// which client_test.go covers only via direct poll() calls.

// errResolver always fails — used to verify the loop doesn't panic or
// advance state on resolver errors.
type errResolver struct{}

func (errResolver) ResolveRDSOrderID(string) (int64, error) {
	return 0, fmt.Errorf("resolve failed")
}

// countingHandler wraps an HTTP handler and exposes a safe request count.
type countingHandler struct {
	count atomic.Int64
	inner http.HandlerFunc
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.count.Add(1)
	h.inner(w, r)
}

func (h *countingHandler) Count() int64 { return h.count.Load() }

func TestPollerStartPolls(t *testing.T) {
	// A handler that always returns RUNNING so we don't trigger terminal
	// removal — that keeps the poller busy making requests.
	ch := &countingHandler{inner: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-run","state":"RUNNING","vehicle":"AMB-01"}`))
	}}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	p := NewPoller(client, &mockPollerEmitter{}, &mockResolver{}, 10*time.Millisecond)
	p.Track("rds-run")

	p.Start()
	defer p.Stop()

	// Give the ticker a few cycles to fire.
	deadline := time.After(500 * time.Millisecond)
	for {
		if ch.Count() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("poll count = %d after 500ms, want >= 2", ch.Count())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestPollerStopHaltsPolling(t *testing.T) {
	ch := &countingHandler{inner: func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-stop","state":"RUNNING","vehicle":"AMB-01"}`))
	}}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	p := NewPoller(client, &mockPollerEmitter{}, &mockResolver{}, 10*time.Millisecond)
	p.Track("rds-stop")
	p.Start()

	// Wait until at least one poll has happened.
	deadline := time.After(500 * time.Millisecond)
waitLoop:
	for {
		if ch.Count() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no polls observed in 500ms")
		case <-time.After(5 * time.Millisecond):
			continue waitLoop
		}
	}

	p.Stop()
	// Allow any in-flight poll to complete.
	time.Sleep(30 * time.Millisecond)
	beforeQuiet := ch.Count()

	// Post-stop window many times the interval — no new requests should arrive.
	time.Sleep(100 * time.Millisecond)
	afterQuiet := ch.Count()

	if afterQuiet != beforeQuiet {
		t.Errorf("polls continued after Stop: before=%d after=%d", beforeQuiet, afterQuiet)
	}
}

func TestPollerStopIdempotent(t *testing.T) {
	client := NewClient("http://localhost:1", time.Second)
	p := NewPoller(client, &mockPollerEmitter{}, &mockResolver{}, time.Minute)

	// Calling Stop() multiple times — including before Start — must not panic
	// (stopOnce guards the channel close).
	p.Stop()
	p.Stop()
}

func TestPollerServerErrorDoesNotPanic(t *testing.T) {
	ch := &countingHandler{inner: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("fleet down"))
	}}
	srv := httptest.NewServer(ch)
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, &mockResolver{}, 10*time.Millisecond)
	p.Track("rds-err")
	p.Start()
	defer p.Stop()

	// Let a few polls cycle through the error path.
	deadline := time.After(500 * time.Millisecond)
	for {
		if ch.Count() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("poll count = %d after 500ms, want >= 2", ch.Count())
		case <-time.After(5 * time.Millisecond):
		}
	}

	// No transitions should have been emitted — the poller should have
	// logged and continued.
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.events) != 0 {
		t.Errorf("emitter got %d events, want 0 (errors should not emit)", len(emitter.events))
	}
	// Order remains tracked so the next successful poll can catch it up.
	if p.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (error path must not untrack)", p.ActiveCount())
	}
}

func TestPollerResolveErrorKeepsOldState(t *testing.T) {
	// Server reports RUNNING; resolver always fails. The poller should log
	// but keep the tracked state at its initial value so the transition is
	// retried next cycle.
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-x","state":"RUNNING","vehicle":"AMB-01"}`))
	})
	defer srv.Close()

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, errResolver{}, time.Minute)
	p.Track("rds-x")

	p.poll()

	if p.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (resolve-error must not remove)", p.ActiveCount())
	}
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.events) != 0 {
		t.Errorf("len(events) = %d, want 0 (resolve-error must not emit)", len(emitter.events))
	}
}

func TestPollerEmitsTransitionEndToEnd(t *testing.T) {
	// Drives the poller through a real Start/Stop cycle with a server that
	// flips state on the second hit. Asserts the emitter sees exactly one
	// CREATED->RUNNING transition.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		state := "CREATED"
		if n >= 2 {
			state = "RUNNING"
		}
		fmt.Fprintf(w, `{"code":0,"msg":"ok","id":"rds-e2e","state":"%s","vehicle":"AMB-07"}`, state)
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, &mockResolver{}, 10*time.Millisecond)
	p.Track("rds-e2e")
	p.Start()
	defer p.Stop()

	deadline := time.After(1 * time.Second)
	for {
		emitter.mu.Lock()
		n := len(emitter.events)
		emitter.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no transition emitted in 1s (hits=%d)", hits.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	ev := emitter.events[0]
	if ev.oldStatus != "CREATED" {
		t.Errorf("oldStatus = %q, want CREATED", ev.oldStatus)
	}
	if ev.newStatus != "RUNNING" {
		t.Errorf("newStatus = %q, want RUNNING", ev.newStatus)
	}
	if ev.rdsOrderID != "rds-e2e" {
		t.Errorf("rdsOrderID = %q, want rds-e2e", ev.rdsOrderID)
	}
	if ev.robotID != "AMB-07" {
		t.Errorf("robotID = %q, want AMB-07", ev.robotID)
	}
	if ev.orderID != 100 {
		t.Errorf("orderID = %d, want 100 (mockResolver)", ev.orderID)
	}
}

func TestPollerEmptyActiveSetIsNoOp(t *testing.T) {
	// No tracked orders — poll() should still not touch the server.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"x","state":"RUNNING"}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	p := NewPoller(client, &mockPollerEmitter{}, &mockResolver{}, time.Minute)
	p.poll()

	if hits.Load() != 0 {
		t.Errorf("server hits = %d, want 0 when no orders tracked", hits.Load())
	}
}

func TestPollerConcurrentTrackDuringPoll(t *testing.T) {
	// Hammers Track/Untrack from a goroutine while poll() runs to ensure
	// the mutex discipline in poll() doesn't deadlock or race.
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-c","state":"RUNNING","vehicle":"AMB-01"}`))
	})
	defer srv.Close()

	p := NewPoller(client, &mockPollerEmitter{}, &mockResolver{}, time.Minute)
	for i := 0; i < 5; i++ {
		p.Track(fmt.Sprintf("rds-%d", i))
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			p.Track(fmt.Sprintf("rds-new-%d", i))
			p.Untrack(fmt.Sprintf("rds-new-%d", i))
		}
	}()

	p.poll()
	wg.Wait()

	// No assertion on exact count (untracked in parallel) beyond sanity.
	if p.ActiveCount() < 0 {
		t.Errorf("ActiveCount went negative: %d", p.ActiveCount())
	}
}
