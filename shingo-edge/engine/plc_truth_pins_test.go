package engine

import (
	"testing"
	"time"

	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// The engine half of the PLC-truth pins (close-out 2b): what the count path
// does with a reset the poll emits, and where a jump's units land once an
// operator confirms it. The poll half is plc/plc_truth_pins_test.go.

// hourlyTotal sums the hourly_counts rows for (process, style) in the current
// UTC hour, the bucket HourlyTracker writes.
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

// TestPin_ResetDeltaMovesNoCount: the poll emits a reset's newCount as its
// delta, and both consumers of EventCounterDelta drop it — the carrier's count
// does not move and no hourly row is written.
func TestPin_ResetDeltaMovesNoCount(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RESET-PIN", PayloadCode: "PART-R", UOPCapacity: 200, InitialUOP: 100,
	})
	eng := testEngine(t, db)
	eng.hourlyTracker = NewHourlyTracker(db)
	eng.wireEventHandlers()

	(&plcEmitter{bus: eng.Events}).EmitCounterDelta(0, processID, styleID, 3, 3, "reset")

	rt := runtimeRow(t, db, nodeID)
	if rt.RemainingUOPCached != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (the reset is dropped)", rt.RemainingUOPCached)
	}
	if n := hourlyTotal(t, db, processID, styleID); n != 0 {
		t.Errorf("hourly total = %d, want 0 (the reset is skipped)", n)
	}
}

// TestPin_JumpChargedAtConfirmTime: a jump's units reach the count path only
// when an operator confirms it, and they are charged to whichever carrier is
// bound THEN. Here the carrier the jump was read against (300 left) is swapped
// for another (1000) before the Confirm, and the 150 lands on the new one.
func TestPin_JumpChargedAtConfirmTime(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "JUMP-PIN", PayloadCode: "PART-J", UOPCapacity: 1000, InitialUOP: 300,
	})
	eng := testEngine(t, db)
	eng.hourlyTracker = NewHourlyTracker(db)
	eng.wireEventHandlers()

	rpID, err := db.CreateReportingPoint("PLC1", "JUMP_TAG", styleID)
	if err != nil {
		t.Fatalf("create reporting point: %v", err)
	}
	// The poll's write for a jump: the row, unconfirmed, and no delta.
	snapID, err := db.InsertCounterSnapshot(rpID, 600, 150, "jump", false, counters.TickStamp{})
	if err != nil {
		t.Fatalf("insert jump: %v", err)
	}
	if rt := runtimeRow(t, db, nodeID); rt.RemainingUOPCached != 300 {
		t.Fatalf("before confirm RemainingUOP = %d, want 300 (the jump is held)", rt.RemainingUOPCached)
	}

	// The carrier the jump was read against leaves; another is bound.
	second := int64(2)
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &second, 1000); err != nil {
		t.Fatalf("bind the second carrier: %v", err)
	}

	svc := service.NewCounterService(db, nil)
	svc.SetDeltaEmitter(&plcEmitter{bus: eng.Events})
	if err := svc.ConfirmAnomaly(snapID); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	rt := runtimeRow(t, db, nodeID)
	if rt.ActiveBinID == nil || *rt.ActiveBinID != second || rt.RemainingUOPCached != 850 {
		t.Errorf("after confirm bin/RemainingUOP = %v/%d, want 2/850 (charged to the carrier bound at confirm)",
			rt.ActiveBinID, rt.RemainingUOPCached)
	}
	if n := hourlyTotal(t, db, processID, styleID); n != 150 {
		t.Errorf("hourly total = %d, want 150 (written at confirm)", n)
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
