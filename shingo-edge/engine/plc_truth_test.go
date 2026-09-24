package engine

import (
	"testing"
	"time"

	"shingoedge/store"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// The engine half of the PLC-truth tests (close-out 2b). The poll half is
// plc/plc_truth_test.go.

// hourlyTotal sums the hourly_counts rows for (process, style) around the
// current UTC hour, the bucket HourlyTracker writes.
func hourlyTotal(t *testing.T, db *store.DB, processID, styleID int64) int64 {
	t.Helper()
	bucket := counters.HourBucket(time.Now())
	rows, err := db.ListHourlyCounts(processID, styleID, bucket-3600, bucket+2*3600)
	if err != nil {
		t.Fatalf("list hourly counts: %v", err)
	}
	var n int64
	for _, r := range rows {
		n += r.Delta
	}
	return n
}

// TestPLCTruth_ResetCountsNewCount is the inverted
// TestPin_ResetDeltaMovesNoCount: a reset's delta (newCount, as
// plc.CalculateDelta returns it) moves the bound carrier's count and the
// hourly record like any tick.
func TestPLCTruth_ResetCountsNewCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RESET-TRUTH", PayloadCode: "PART-R", UOPCapacity: 200, InitialUOP: 100,
	})
	eng := testEngine(t, db)
	eng.hourlyTracker = NewHourlyTracker(db)
	eng.wireEventHandlers()

	(&plcEmitter{bus: eng.Events}).EmitCounterDelta(0, processID, styleID, 3, 3, "reset")

	rt := runtimeRow(t, db, nodeID)
	if rt.RemainingUOPCached != 97 {
		t.Errorf("RemainingUOP = %d, want 97 (the reset's 3 are parts)", rt.RemainingUOPCached)
	}
	if n := hourlyTotal(t, db, processID, styleID); n != 3 {
		t.Errorf("hourly total = %d, want 3", n)
	}
}

// TestPLCTruth_JumpChargesTheCarrierBoundAtTheRead: the poll now emits a
// jump's delta in the pass that read it (plc TestPLCTruth_JumpEmitsItsDeltaInThePass),
// and the count path charges whichever carrier is bound when the event
// arrives — so the carrier bound at the read takes it, and one bound later
// does not. Replaces TestPin_JumpChargedAtConfirmTime; there is no Confirm.
func TestPLCTruth_JumpChargesTheCarrierBoundAtTheRead(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "JUMP-TRUTH", PayloadCode: "PART-J", UOPCapacity: 1000, InitialUOP: 300,
	})
	eng := testEngine(t, db)
	eng.hourlyTracker = NewHourlyTracker(db)
	eng.wireEventHandlers()

	(&plcEmitter{bus: eng.Events}).EmitCounterDelta(0, processID, styleID, 150, 600, "jump")
	if rt := runtimeRow(t, db, nodeID); rt.RemainingUOPCached != 150 {
		t.Fatalf("RemainingUOP = %d, want 150 (the jump's 150 off the carrier bound at the read)", rt.RemainingUOPCached)
	}

	second := int64(2)
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &second, 1000); err != nil {
		t.Fatalf("bind the second carrier: %v", err)
	}
	if rt := runtimeRow(t, db, nodeID); rt.RemainingUOPCached != 1000 {
		t.Errorf("second carrier RemainingUOP = %d, want 1000 (nothing charged later)", rt.RemainingUOPCached)
	}
	if n := hourlyTotal(t, db, processID, styleID); n != 150 {
		t.Errorf("hourly total = %d, want 150 (written at the read)", n)
	}
}

// runtimeRow reads the node's runtime row, failing the test on a read error.
func runtimeRow(t *testing.T, db *store.DB, nodeID int64) *processes.RuntimeState {
	t.Helper()
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	return rt
}
