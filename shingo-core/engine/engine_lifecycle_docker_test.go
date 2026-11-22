//go:build docker

package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"shingocore/fleet/simulator"
)

// TestEngineStop_WaitsForItsGoroutines: Engine.Stop returns only once the
// goroutines Start launched have finished, here a piece of after-queue work
// already running when Stop is called. Stop used to return at once and leave
// the work running against a database the test's cleanup then dropped, logging
// through the finished test's t.Logf: a data race under -race that failed every
// engine test running beside it (CI run 43042514760, 2026-11-11: the threshold
// monitor's handleBinUpdated, handed to the after-queue, logging after
// TestHandleStoreBlockCompleted_IntermediateDropoffCarriesRealFromNode ended).
func TestEngineStop_WaitsForItsGoroutines(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	started := make(chan struct{})
	var finished atomic.Bool
	eng.after.later(func() {
		close(started)
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the after-queue did not start the work in 10 s")
	}

	eng.Stop() // t.Cleanup stops it again: a second Stop is a no-op
	if !finished.Load() {
		t.Fatal("Engine.Stop returned while after-queue work it had started was still running")
	}
}
