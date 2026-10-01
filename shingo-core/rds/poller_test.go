package rds

import (
	"errors"
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
	t.Parallel()
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

func TestPollerStopIdempotent(t *testing.T) {
	t.Parallel()
	client := NewClient("http://localhost:1", time.Second)
	p := NewPoller(client, &mockPollerEmitter{}, &mockResolver{}, time.Minute)

	// Calling Stop() multiple times — including before Start — must not panic
	// (stopOnce guards the channel close).
	p.Stop()
	p.Stop()
}

func TestPollerServerErrorDoesNotPanic(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestPollerEmitsBlockCompletedOnTransition is the core regression test
// for the bin-transit-state Phase 2 wiring. The poll snapshot already
// included per-block state pre-Phase-2 (see rds.OrderDetail.Blocks), but
// the poller never diffed it. This test asserts: when a block transitions
// to FINISHED on a subsequent poll, EXACTLY one EmitBlockCompleted fires
// for that block, with the BlockID/Location/BinTask passed through.
//
// Why this is load-bearing: the entire Phase 2 design rests on this
// being the per-pickup signal. If the diff misses transitions, queued
// orders never unblock and source slots stay stuck.
func TestPollerEmitsBlockCompletedOnTransition(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		// First poll: block running. Second poll: block FINISHED, order
		// still RUNNING (delivery block not yet started). Third poll: order
		// terminal — but the test only inspects the second.
		var blockState string
		switch {
		case n == 1:
			blockState = "RUNNING"
		default:
			blockState = "FINISHED"
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-blk","state":"RUNNING","vehicle":"AMB-9","blocks":[
			{"blockId":"b-pickup","location":"AP-SOURCE","state":"` + blockState + `","binTask":"Load"},
			{"blockId":"b-deliver","location":"AP-DEST","state":"CREATED","binTask":"Unload"}
		]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, &mockResolver{}, time.Minute)
	p.Track("rds-blk")

	// Poll once: pickup is RUNNING, no transition yet.
	p.poll()
	emitter.mu.Lock()
	if got := len(emitter.blockEvents); got != 0 {
		t.Errorf("after first poll: %d block events, want 0 (block was already RUNNING; first sample is a baseline)", got)
	}
	emitter.mu.Unlock()

	// Poll again: pickup transitioned to FINISHED. EXACTLY one event.
	p.poll()
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if got := len(emitter.blockEvents); got != 1 {
		t.Fatalf("after second poll: %d block events, want 1", got)
	}
	got := emitter.blockEvents[0]
	if got.blockID != "b-pickup" {
		t.Errorf("blockID = %q, want b-pickup", got.blockID)
	}
	if got.location != "AP-SOURCE" {
		t.Errorf("location = %q, want AP-SOURCE", got.location)
	}
	if got.binTask != "Load" {
		t.Errorf("binTask = %q, want Load (the kind classifier upstream depends on this)", got.binTask)
	}
	if got.orderID != 100 {
		t.Errorf("orderID = %d, want 100 (mockResolver)", got.orderID)
	}
}

// TestPollerBlockCompletedFiresOnce locks down idempotence: even if we
// poll the same FINISHED state repeatedly, we don't re-emit. The
// vendor's poll cadence is short (~500ms typical), so a sticky FINISHED
// state would otherwise flood the engine handler with duplicates and
// log lines on every cycle.
func TestPollerBlockCompletedFiresOnce(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always-FINISHED block, order still RUNNING.
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-once","state":"RUNNING","vehicle":"AMB-9","blocks":[
			{"blockId":"b-once","location":"AP-X","state":"FINISHED","binTask":"Load"}
		]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, &mockResolver{}, time.Minute)
	p.Track("rds-once")

	// First poll observes the transition (CREATED→FINISHED in our
	// internal model — `prev` starts empty). Subsequent polls must not
	// re-emit because `prev[b-once]==FINISHED` short-circuits the diff.
	p.poll()
	p.poll()
	p.poll()

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if got := len(emitter.blockEvents); got != 1 {
		t.Errorf("len(blockEvents) = %d after 3 polls, want 1 (sticky FINISHED must not re-emit)", got)
	}
}

// TestPollerBlockStateClearedOnTerminal locks down memory hygiene: when
// the parent order reaches a terminal state, the poller should drop its
// per-block tracking too. Without this, blockStates would grow
// unboundedly across the lifetime of the process.
func TestPollerBlockStateClearedOnTerminal(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		state := "RUNNING"
		blockState := "RUNNING"
		if n >= 2 {
			state = "FINISHED" // terminal
			blockState = "FINISHED"
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-term","state":"` + state + `","vehicle":"AMB-9","blocks":[
			{"blockId":"b","location":"AP-Q","state":"` + blockState + `","binTask":"Load"}
		]}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, &mockResolver{}, time.Minute)
	p.Track("rds-term")

	p.poll() // baseline
	p.poll() // transitions to terminal — order untracked, blockStates dropped

	p.mu.Lock()
	_, blockStillTracked := p.blockStates["rds-term"]
	p.mu.Unlock()
	if blockStillTracked {
		t.Errorf("blockStates['rds-term'] still present after terminal transition; expected drop alongside p.active")
	}
}

func TestPollerConcurrentTrackDuringPoll(t *testing.T) {
	t.Parallel()
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

// TestPollerGraceRecovery verifies RUNNING -> FAILED -> RUNNING within grace
// results in a recovery transition and the order stays tracked.
func TestPollerGraceRecovery(t *testing.T) {
	t.Parallel()
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		state := "RUNNING"
		if callCount == 2 {
			state = "FAILED"
		}
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"code":0,"msg":"ok","id":"rds-gr","state":"%s","vehicle":"AMB-01"}`, state)))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	p := NewPoller(client, emitter, &mockResolver{}, time.Minute)
	p.Track("rds-gr")

	// First poll: RUNNING baseline
	p.poll()

	// Second poll: RUNNING -> FAILED (enters grace period, stays tracked)
	p.poll()
	if p.ActiveCount() != 1 {
		t.Errorf("ActiveCount after FAILED = %d, want 1 (grace period)", p.ActiveCount())
	}
	if p.FaultedCount() != 1 {
		t.Errorf("FaultedCount after FAILED = %d, want 1", p.FaultedCount())
	}

	// Third poll: FAILED -> RUNNING (recovery within grace)
	p.poll()
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	recovered := false
	for _, ev := range emitter.events {
		if ev.newStatus == "RUNNING" && ev.oldStatus == "FAILED" {
			recovered = true
		}
	}
	if !recovered {
		t.Errorf("expected FAILED->RUNNING recovery transition, got events: %+v", emitter.events)
	}
	if p.FaultedCount() != 0 {
		t.Errorf("FaultedCount after recovery = %d, want 0", p.FaultedCount())
	}
	if len(emitter.graceExpired) != 0 {
		t.Errorf("grace expired events = %d, want 0", len(emitter.graceExpired))
	}
}

// TestPollerGraceExpiry verifies RUNNING -> FAILED + grace expiry emits GraceExpired
// and untracks the order.
func TestPollerGraceExpiry(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-exp","state":"FAILED","vehicle":"AMB-01"}`))
	}))
	defer srv.Close()
	client := NewClient(srv.URL, 2*time.Second)

	emitter := &mockPollerEmitter{}
	// Use a very short grace duration so it expires immediately
	p := NewPoller(client, emitter, &mockResolver{}, time.Minute, 1*time.Millisecond)
	p.Track("rds-exp")

	// First poll: baseline (CREATED -> FAILED transition)
	p.poll()
	if p.ActiveCount() != 1 {
		t.Errorf("ActiveCount after FAILED = %d, want 1 (grace period)", p.ActiveCount())
	}

	// KEEP: timing test — verifying grace-period expiry.
	time.Sleep(10 * time.Millisecond)

	// Second poll: still FAILED, grace should have expired
	p.poll()

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.graceExpired) != 1 {
		t.Fatalf("grace expired events = %d, want 1", len(emitter.graceExpired))
	}
	if emitter.graceExpired[0].rdsOrderID != "rds-exp" {
		t.Errorf("grace expired rdsOrderID = %q, want rds-exp", emitter.graceExpired[0].rdsOrderID)
	}
	if p.ActiveCount() != 0 {
		t.Errorf("ActiveCount after grace expiry = %d, want 0", p.ActiveCount())
	}
}

// TestPollerReportsNextWaitAfterRelease is order 112 at the poller: a release
// appends a segment and the robot drives it and parks again between two polls,
// so both reads say WAITING. The second WAITING is reported once the appended
// segment's last block is FINISHED, and only once. A WAITING read while that
// block is still pending is the old wait and is not reported.
func TestPollerReportsNextWaitAfterRelease(t *testing.T) {
	t.Parallel()
	var appendedState atomic.Value
	appendedState.Store("CREATED")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-112","state":"WAITING","vehicle":"AMB-1","blocks":[
			{"blockId":"b1","location":"AP-LINE","state":"FINISHED","binTask":"Unload"},
			{"blockId":"b2","location":"AP-STORE","state":"` + appendedState.Load().(string) + `","binTask":"Unload"}
		]}`)); err != nil {
			t.Errorf("write RDS reply: %v", err)
		}
	}))
	defer srv.Close()

	emitter := &mockPollerEmitter{}
	p := NewPoller(NewClient(srv.URL, 2*time.Second), emitter, &mockResolver{}, time.Minute)
	p.Track("rds-112")
	count := func() int {
		emitter.mu.Lock()
		defer emitter.mu.Unlock()
		return len(emitter.events)
	}

	p.poll() // CREATED -> WAITING: the first wait
	if got := count(); got != 1 {
		t.Fatalf("first wait: %d events, want 1", got)
	}

	p.ExpectNextWait("rds-112", "b2")
	p.poll() // still parked at the first wait: the appended block has not run
	if got := count(); got != 1 {
		t.Fatalf("before the segment finished: %d events, want 1", got)
	}

	appendedState.Store("FINISHED")
	p.poll() // parked at the next wait; RUNNING was never read
	p.poll() // and again: reported once
	if got := count(); got != 2 {
		t.Fatalf("after the segment finished: %d events, want 2", got)
	}
	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if ev := emitter.events[1]; ev.oldStatus != "WAITING" || ev.newStatus != "WAITING" {
		t.Errorf("second event = %s -> %s, want WAITING -> WAITING", ev.oldStatus, ev.newStatus)
	}
}

// TestPollerRestartAfterRelease is order 112 across a Core restart (S3).
// Core restarts in the seconds after a release, before the robot moves off:
// RDS still reads WAITING at the old wait, and the appended block has not run.
//
// A plain Track (a new order) seeds CREATED, so that first WAITING is a
// transition and Core would stage the order at its NEXT wait while the robot
// still stands at the old one. TrackReleased, which boot uses for an open
// order already released past a wait, seeds WAITING and arms the last block
// RDS holds: the read counts only once that block is FINISHED.
func TestPollerRestartAfterRelease(t *testing.T) {
	t.Parallel()
	var appendedState atomic.Value
	appendedState.Store("CREATED")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-112r","state":"WAITING","vehicle":"AMB-1","blocks":[
			{"blockId":"b1","location":"AP-LINE","state":"FINISHED","binTask":"Unload"},
			{"blockId":"b2","location":"AP-STORE","state":"` + appendedState.Load().(string) + `","binTask":"Unload"}
		]}`)); err != nil {
			t.Errorf("write RDS reply: %v", err)
		}
	}))
	defer srv.Close()

	emitter := &mockPollerEmitter{}
	p := NewPoller(NewClient(srv.URL, 2*time.Second), emitter, &mockResolver{}, time.Minute)
	count := func() int {
		emitter.mu.Lock()
		defer emitter.mu.Unlock()
		return len(emitter.events)
	}

	p.TrackReleased("rds-112r", "b2")
	p.poll() // the restart's first read: still the old wait
	if got := count(); got != 0 {
		t.Fatalf("restart, robot at the old wait: %d events, want 0 — Core would stage the next wait early", got)
	}
	appendedState.Store("FINISHED")
	p.poll() // the robot finished the segment and parked at the next wait
	if got := count(); got != 1 {
		t.Fatalf("restart, robot parked at the next wait: %d events, want 1", got)
	}
}

// flakyResolver fails its first n resolutions, then answers.
type flakyResolver struct {
	mu    sync.Mutex
	fails int
}

func (r *flakyResolver) ResolveRDSOrderID(string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails > 0 {
		r.fails--
		return 0, errors.New("resolver unavailable")
	}
	return 100, nil
}

// TestPollerBlockFinishedSurvivesAResolveFailure (R4-9): a block that reached
// FINISHED while the order could not be resolved is reported on the next
// cycle that resolves it, once. It was recorded as seen and then dropped, so
// the pickup it carries (the bin's move to _TRANSIT, BinPickedUp) never came.
func TestPollerBlockFinishedSurvivesAResolveFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-r49","state":"RUNNING","vehicle":"AMB-1","blocks":[
			{"blockId":"p1","location":"AP-LINE","state":"FINISHED","binTask":"JackLoad"}
		]}`)); err != nil {
			t.Errorf("write RDS reply: %v", err)
		}
	}))
	defer srv.Close()
	emitter := &mockPollerEmitter{}
	p := NewPoller(NewClient(srv.URL, 2*time.Second), emitter, &flakyResolver{fails: 1}, time.Minute)
	p.Track("rds-r49")
	blocks := func() int {
		emitter.mu.Lock()
		defer emitter.mu.Unlock()
		return len(emitter.blockEvents)
	}
	p.poll() // the resolver fails
	if got := blocks(); got != 0 {
		t.Fatalf("with the resolver failing: %d block events, want 0", got)
	}
	p.poll() // it answers: the FINISHED block is reported now
	p.poll() // and only once
	if got := blocks(); got != 1 {
		t.Fatalf("after the resolver recovered: %d block events, want 1", got)
	}
}
