package engine

import (
	"strconv"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// sequential_backfill_doors_test.go — census 28, 29, 30: the one-position,
// one-inbound-carrier interlock (handleSequentialBackfill + orderRefillsNodeItself)
// on the doors sequential_changeover_backfill_test.go does not drive.
//
// That file pins the changeover SWAP (no backfill) and the produce REQUEST (one
// backfill). The interlock reads what Order A does, not which door made it, so it
// should answer the same way for every door — which is exactly the claim these
// check, one door at a time.
//
// COVERAGE PINS. Expected to pass at bcbde0d2. MUTATION for all three: make
// orderRefillsNodeItself return true unconditionally (no backfill ever) or false
// unconditionally (a backfill every time) — each test fails in one direction.

// TestSequentialConsume_RemovalMintsOneBackfill is census 28: a CONSUME sequential
// cell (edge2.yaml's PRS_010 shape). Order A only lifts the drained carrier off,
// so the backfill is the only thing that brings the position a new one.
func TestSequentialConsume_RemovalMintsOneBackfill(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID := seedLineSwapClaim(t, db, "SEQC", protocol.SwapModeSequential)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	if _, err := eng.RequestNodeMaterial(nodeID, 1); err != nil {
		t.Fatalf("consume REQUEST on a sequential cell: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID == nil {
		t.Fatal("fixture: the REQUEST left no active order on the cell")
	}
	if before := ordersDroppingAt(t, db, "SEQC-LINE"); len(before) != 0 {
		t.Fatalf("fixture: %v already deliver to SEQC-LINE — Order A should only remove", before)
	}

	driveToInTransit(t, eng, *rt.ActiveOrderID, nodeID)

	after := ordersDroppingAt(t, db, "SEQC-LINE")
	if len(after) != 1 {
		t.Fatalf("after the consume removal went in_transit, %d orders deliver to SEQC-LINE (%v), want 1 — "+
			"the backfill is the only thing that refills a consume sequential position", len(after), after)
	}
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after")
	if rt.StagedOrderID == nil || *rt.StagedOrderID != after[0] {
		t.Errorf("StagedOrderID = %v, want the backfill %d", rt.StagedOrderID, after[0])
	}
}

// TestSequentialChangeoverEvacuate_OnePositionGetsOneCarrier is census 29: a
// tool change's leg on a sequential position (toolingPositionAction) has the
// same self-refilling tail as the changeover swap, so it must not get a
// backfill either. Its comment says the same read covers it; nothing checked.
func TestSequentialChangeoverEvacuate_OnePositionGetsOneCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, _, activeNodeID, parkedNodeID, co := seedSequentialScenario(t, db, true)
	orderA, _ := seqTaskOrders(t, db, co.ID, activeNodeID, parkedNodeID)
	if before := ordersDroppingAt(t, db, "SEQ-A"); len(before) != 1 || before[0] != orderA {
		t.Fatalf("fixture: orders dropping at SEQ-A = %v, want exactly [%d] (the evacuate's own refill)",
			before, orderA)
	}

	driveToInTransit(t, eng, orderA, activeNodeID)

	if after := ordersDroppingAt(t, db, "SEQ-A"); len(after) != 1 {
		t.Errorf("after the tooling evacuate went in_transit, %d orders deliver to SEQ-A (%v), want 1 — "+
			"the evacuate brings its own carrier back, and a second one wedges the position", len(after), after)
	}
	rt, err := db.GetProcessNodeRuntime(activeNodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.StagedOrderID != nil {
		t.Errorf("StagedOrderID = %d after an evacuate went in_transit; a self-refilling Order A has no backfill",
			*rt.StagedOrderID)
	}
}

// TestSequentialEmptyBin_RemovalMintsOneBackfill is census 30: REQUEST EMPTY BIN
// on a produce sequential cell builds the same removal-only Order A as REQUEST
// SWAP, and needs the same one backfill.
func TestSequentialEmptyBin_RemovalMintsOneBackfill(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, protocol.SwapModeSequential)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	if _, err := eng.RequestEmptyBin(nodeID, "WIDGET-A"); err != nil {
		t.Fatalf("REQUEST EMPTY BIN on a sequential cell: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID == nil {
		t.Fatal("fixture: REQUEST EMPTY BIN left no active order on the cell")
	}

	driveToInTransit(t, eng, *rt.ActiveOrderID, nodeID)

	after := ordersDroppingAt(t, db, "PRODUCE-NODE")
	if len(after) != 1 {
		t.Fatalf("after the removal went in_transit, %d orders deliver to PRODUCE-NODE (%v), want 1", len(after), after)
	}
	rt, err = db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime after")
	if rt.StagedOrderID == nil || *rt.StagedOrderID != after[0] {
		t.Errorf("StagedOrderID = %v, want the backfill %d", rt.StagedOrderID, after[0])
	}
}

// TestSequentialBackfill_IsCountedAndCarriesItsRemovalsOrigin: the removal's
// plan counts the backfill it leaves to be made (SwapDispatch.FollowOns), so
// the episode the REQUEST opens expects two orders, and the backfill made at
// the removal's lift carries the removal's own origin. It joined the cell's
// open episode instead: the episode expected one, received two, and counted a
// rerequest on every healthy cycle.
func TestSequentialBackfill_IsCountedAndCarriesItsRemovalsOrigin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID := seedLineSwapClaim(t, db, "SEQO", protocol.SwapModeSequential)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	if _, err := eng.RequestNodeMaterial(nodeID, 1); err != nil {
		t.Fatalf("consume REQUEST on a sequential cell: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID == nil {
		t.Fatal("fixture: the REQUEST left no active order on the cell")
	}
	removal, err := db.GetOrder(*rt.ActiveOrderID)
	testutil.MustNoErr(t, err, "the removal")
	open, err := db.ListOpenDemandOrigins()
	testutil.MustNoErr(t, err, "open episodes")
	if len(open) != 1 || removal.OriginID != open[0].OriginID {
		t.Fatalf("fixture: open episodes %+v, removal origin %q; want the REQUEST's one episode on the removal", open, removal.OriginID)
	}
	if e := open[0].ExpectedOrders; e == nil || *e != 2 {
		t.Errorf("the episode expects %s orders, want 2: the removal and the backfill it leaves", fmtExpected(e))
	}

	driveToInTransit(t, eng, removal.ID, nodeID)

	after := ordersDroppingAt(t, db, "SEQO-LINE")
	if len(after) != 1 {
		t.Fatalf("%d backfills, want 1", len(after))
	}
	backfill, err := db.GetOrder(after[0])
	testutil.MustNoErr(t, err, "the backfill")
	if backfill.OriginID != removal.OriginID || backfill.OriginClass != string(protocol.OriginClassAttached) {
		t.Errorf("the backfill carries origin %q (%s), want the removal's %q, attached",
			backfill.OriginID, backfill.OriginClass, removal.OriginID)
	}
	open, err = db.ListOpenDemandOrigins()
	testutil.MustNoErr(t, err, "open episodes after")
	if len(open) != 1 || open[0].RerequestCount != 0 {
		t.Errorf("after the backfill the episodes are %+v, want the one, asked again 0 times", open)
	}
}

func fmtExpected(e *int) string {
	if e == nil {
		return "unknown"
	}
	return strconv.Itoa(*e)
}
