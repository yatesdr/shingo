//go:build docker

package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
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

// TestEngineStop_WaitsForTheAdvanceAfterALastLegFails is docket B4: when the
// fleet fails a dig's last live leg, nothing is left to cancel, so the chapter's
// end is handed to another goroutine, and that one writes (the order's restart,
// the lane's release). Stop must not return before it has finished: the caller
// closes the database next.
//
// The test holds the order's row, so the hand-off stops at its first write on
// it, and lets the row go a moment after Stop is called. A Stop that does not
// wait for the hand-off returns with the order still reshuffling.
func TestEngineStop_WaitsForTheAdvanceAfterALastLegFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "STOPRACE"})
	sim := simulator.New()
	eng := newTestEngine(t, db, sim)

	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "stoprace-dig-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  sc.Payload.Code,
		SourceNode:   sc.Grp.Name,
		DeliveryNode: sc.LineNode.Name,
		Quantity:     1,
	})
	parent := testdb.RequireOrderStatus(t, db, "stoprace-dig-1", dispatch.StatusReshuffling)
	legs := legsByID(t, db, parent.ID)
	if len(legs) != 2 || legs[0].VendorOrderID == "" {
		t.Fatalf("fixture: want two legs with the first sent, got %d", len(legs))
	}
	driveLegRunToFinish(t, db, sim, legs[0].ID, legs[0].VendorOrderID)
	var retrieve *orders.Order
	testutil.AssertEventually(t, 5*time.Millisecond, 10*time.Second, func() bool {
		retrieve = legsByID(t, db, parent.ID)[1]
		return retrieve.VendorOrderID != ""
	})

	hold, err := db.Begin()
	testutil.MustNoErr(t, err, "open the transaction that holds the order's row")
	defer hold.Rollback() //nolint:errcheck // let go below; a second rollback is a no-op
	_, err = hold.Exec(`SELECT 1 FROM orders WHERE id = $1 FOR UPDATE`, parent.ID)
	testutil.MustNoErr(t, err, "hold the order's row")

	sim.DriveState(retrieve.VendorOrderID, "RUNNING")
	sim.DriveState(retrieve.VendorOrderID, "FAILED")
	eng.handleGraceExpired(GraceExpiredEvent{OrderID: retrieve.ID, VendorOrderID: retrieve.VendorOrderID})
	testutil.AssertEventually(t, 5*time.Millisecond, 10*time.Second, func() bool {
		var waiting int
		testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting),
			"count the backends waiting on a lock")
		return waiting > 0
	})

	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = hold.Rollback()
	}()
	eng.Stop() // t.Cleanup stops it again: a second Stop is a no-op

	if got := testdb.RequireOrder(t, db, "stoprace-dig-1"); got.Status == dispatch.StatusReshuffling {
		t.Fatalf("Engine.Stop returned while the hand-off after the last leg's failure was still running: "+
			"the order is %q", got.Status)
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
