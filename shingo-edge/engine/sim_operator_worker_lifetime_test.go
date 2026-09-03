//go:build sim

package engine

import (
	"context"
	"testing"
	"time"

	"shingo/protocol/clock"
)

// The lifetime of a scheduled operator worker, which is not the same thing as
// the lifetime of the Engine it holds a pointer to.
//
// Every schedule* helper spawns a goroutine that dwells on the sim clock and
// then reaches into op.e. Both ends of that dwell were unguarded: the select
// could pick its timer over a cancellation that had already happened, and
// nothing re-established that there was still a store on the far side.

// TestSimOperator_AWokenReleaseWorkerSurvivesAnEngineWithNoStore is the panic
// itself, made deterministic.
//
// scheduleRelease spawns runRelease; the manual clock's advance fires it; the
// fixture's Engine has no db. On main that is a nil dereference inside
// GetOrder, which takes the whole test binary with it — so the assertion here
// is that the test finishes at all. The releasing map is the observable: the
// worker's own defer clears its entry, so an entry that goes away is a worker
// that returned rather than one that died.
//
// MUTATION: drop the hasStore check from runRelease and this panics, exactly as
// `go test -count=20 -tags sim ./engine/` does on main.
func TestSimOperator_AWokenReleaseWorkerSurvivesAnEngineWithNoStore(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	op.releaseTries = make(map[int64]int)
	op.releasing = make(map[int64]bool)

	op.scheduleRelease(99)

	// Advancing before the worker has registered its waiter is harmless — the
	// clock only fires waiters it can see — so keep advancing until the worker
	// has woken, run, and cleared itself.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.Advance(defaultSwapReleaseDelay)
		op.mu.Lock()
		done := !op.releasing[99]
		op.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the release worker never returned. It is waiting on a clock that has been advanced " +
		"well past its delay, which means it is stuck somewhere it should not be.")
}

// TestSimOperator_DwellRefusesToActOnceCancelled pins the other end.
//
// A zero delay makes the manual clock's channel ready before dwell is even
// called (Manual.After sends immediately for d <= 0), so BOTH select cases are
// ready and the runtime picks between them uniformly at random. The answer must
// not depend on which one it took: a cancelled operator does not act.
//
// MUTATION: drop the trailing `op.ctx.Err() == nil` and this fails about half
// the time — which is the shape it had in production, where "about half the
// time" is a worker touching an engine that is going away.
func TestSimOperator_DwellRefusesToActOnceCancelled(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	ctx, cancel := context.WithCancel(context.Background())
	op.ctx = ctx
	cancel()

	for i := 0; i < 200; i++ {
		if op.dwell(0) {
			t.Fatalf("dwell said act on attempt %d, with the context already cancelled. "+
				"select picks a ready case at random; the decision has to be re-made after "+
				"the wake, not delegated to that pick.", i)
		}
	}
}

// TestSimOperator_DwellActsWhenTheTimerIsTheOnlyThingReady is the half that
// keeps the guard from becoming a stop-everything: a live operator whose delay
// has elapsed must still do its job.
func TestSimOperator_DwellActsWhenTheTimerIsTheOnlyThingReady(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	if !op.dwell(0) {
		t.Error("dwell refused to act on a live operator whose delay had elapsed")
	}
}

// TestSimOperator_HasStoreIsWhatTheDiagnosticAlreadyAsked keeps the two sites
// on one predicate. releaseCapDiagnosis carried this check inline and
// runRelease, one frame up and holding the same pointer across a longer wait,
// had none.
func TestSimOperator_HasStoreIsWhatTheDiagnosticAlreadyAsked(t *testing.T) {
	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	if op.hasStore() {
		t.Fatal("fixture: newTestSimOperator builds an Engine with no db, deliberately")
	}
	if got := op.releaseCapDiagnosis(1); got == "" {
		t.Error("the diagnostic must still answer without a store — it is the thing explaining " +
			"a refusal, not a thing entitled to end the run")
	}
	op.e = nil
	if op.hasStore() {
		t.Error("a nil Engine has no store either")
	}
}
