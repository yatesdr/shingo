//go:build docker

package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
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

// TestEngineStop_AnEventAfterStop pins what a stopped engine does with an event
// on its bus, in counted statements. Today it still answers: Stop leaves every
// subscription in place, so a bin event runs the synchronous subscribers (the
// audit row, at least) against a database the caller is about to close. Docket
// D1 turns this to 0, and changes this test in the same commit.
func TestEngineStop_AnEventAfterStop(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "STOPPED-BUS-1")
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open the counting store")
	t.Cleanup(func() { cdb.Close() })
	eng := newTestEngine(t, cdb, simulator.New())

	eng.Stop()
	counter.Reset()
	eng.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
		NodeID: sd.StorageNode.ID, Action: BinActionCounted, BinID: bin.ID, PayloadCode: sd.Payload.Code,
	}})
	n := counter.Count()
	t.Logf("a bin event after Stop: %d statements: %q", n, counter.Statements())
	if n == 0 {
		t.Fatal("a stopped engine answered a bin event with no statement: if D1 landed, this pin asserts 0")
	}
}
