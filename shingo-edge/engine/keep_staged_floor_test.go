package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// THE FLOOR: a keep-staged swap waiting at Core for its spare, with nothing on
// its way to the spot. Each test seeds a keep-staged consume cell whose swap
// leg sits in the runtime slot still acquiring (queued), and a bare spot.

func holdingSwap(t *testing.T, db *store.DB, nodeID int64) {
	t.Helper()
	claim := keeperClaimByID(t, db, nodeID)
	a, _ := BuildTwoRobotSwapSteps(claim)
	leg := mkSwapLeg(t, db, nodeID, "floor-swap", a, "")
	testutil.MustNoErr(t, db.UpdateOrderStatus(leg.ID, string(protocol.StatusQueued)), "queued")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &leg.ID, nil), "slot")
}

func keeperClaimByID(t *testing.T, db *store.DB, nodeID int64) *processes.NodeClaim {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "node")
	return requestedClaimAtNode(db, node)
}

// a refill to the spot that has already ended with status.
func endedRefill(t *testing.T, eng *Engine, db *store.DB, nodeID int64, status protocol.Status) {
	t.Helper()
	o, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, ksSpot, ksMarket, "", "standard", ksPart, true, false,
		orders.Attached("floor-prior"))
	testutil.MustNoErr(t, err, "prior refill")
	testutil.MustNoErr(t, db.UpdateOrderStatus(o.ID, string(status)), "end the refill")
}

func floorCell(t *testing.T) (*Engine, *store.DB, int64) {
	t.Helper()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {}})
	return eng, db, nodeID
}

func TestKeepStagedFloor_FailedRefillIsNotRecreated(t *testing.T) {
	t.Parallel()
	for _, ended := range []protocol.Status{protocol.StatusFailed, protocol.StatusSkipped} {
		t.Run(string(ended), func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID := floorCell(t)
			holdingSwap(t, db, nodeID)
			endedRefill(t, eng, db, nodeID, ended)
			for i := 0; i < 5; i++ {
				eng.sweepCellLevels()
			}
			if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
				t.Fatalf("the floor re-created %d refill(s) after one ended %s: a structural failure on a timer",
					got.refills, ended)
			}
		})
	}
}

func TestKeepStagedFloor_CancelledRefillIsRecreatedOnce(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := floorCell(t)
	holdingSwap(t, db, nodeID)
	endedRefill(t, eng, db, nodeID, protocol.StatusCancelled)
	for i := 0; i < 5; i++ {
		eng.sweepCellLevels()
	}
	// Bare spot, the waiting swap will lift what lands: one for it, one to stand.
	if got := readSpotOrders(t, db, nodeID); got.refills != 2 {
		t.Fatalf("refills after five sweeps = %d, want the reconcile's 2 once and no more", got.refills)
	}
}

func TestKeepStagedFloor_ARefillOnItsWaySuppresses(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := floorCell(t)
	holdingSwap(t, db, nodeID)
	_, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, ksSpot, ksMarket, "", "standard", ksPart, true, false,
		orders.Attached("floor-coming"))
	testutil.MustNoErr(t, err, "refill on its way")
	eng.sweepCellLevels()
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Fatalf("refills = %d, want only the one already on its way", got.refills)
	}
}

// A keep-staged cell with no swap in flight costs the floor nothing: no read of
// the line's orders and no call to Core.
func TestKeepStagedFloor_NoSwapInFlightOrdersNothing(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := floorCell(t)
	eng.sweepCellLevels()
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
		t.Fatalf("refills = %d with no swap in flight, want 0", got.refills)
	}
}

// A REQUEST and the floor deciding in the same moment create the spot's orders
// once. Both hold the cell's prime lock from their read of what is coming to
// their create. Here the test holds it the way a request's apply does: the
// floor, started in that moment with its swap waiting and nothing coming yet,
// finds the cell being decided and leaves it to the decider; the "request"
// writes its refills. Without the lock the floor reads nothing coming and adds
// its own.
func TestKeepStagedFloor_RequestAndFloorTogetherCreateOnce(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := floorCell(t)
	holdingSwap(t, db, nodeID)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "node")
	claim := keeperClaimByID(t, db, nodeID)
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")

	mu := eng.primeNodeLock(claim)
	mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.keepStagedFloor(node, runtime, claim)
	}()
	eng.refillSpot(node, claim, 2, orders.Attached("floor-request"))
	mu.Unlock()
	<-done

	if got := readSpotOrders(t, db, nodeID); got.refills != 2 {
		t.Fatalf("refills = %d, want the request's 2 and nothing from the floor", got.refills)
	}
}

// A changeover start aborts the keep-staged line's waiting swap and the refill
// coming to its spot while it holds the line's prime lock. The refill's
// completion cascade is delivered on the start's own goroutine and kicks the
// floor; the floor must not wait for a lock its caller holds.
func TestKeepStagedFloor_StartAbortingTheCellDoesNotWaitOnItself(t *testing.T) {
	t.Parallel()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}, "SPOT": {}})
	// The production emitter: an abort's completion cascade runs in the caller.
	fx.eng.orderMgr = orders.NewManager(fx.db, &orderEmitter{bus: fx.eng.Events}, "test.station")
	nodeID := fx.nodeIDs["L1"]
	// The refill is the older row, so the start aborts it first, while the swap
	// still holds the line's slot and the floor goes as far as the lock.
	_, err := fx.eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, "SPOT", "SRC-OLD", "", "standard", "PART-OLD",
		true, false, orders.Attached("floor-start"))
	testutil.MustNoErr(t, err, "refill coming")
	holdingSwap(t, fx.db, nodeID)

	done := make(chan error, 1)
	go func() {
		_, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged")
		done <- err
	}()
	select {
	case err := <-done:
		testutil.MustNoErr(t, err, "start")
	case <-time.After(10 * time.Second):
		t.Fatal("the changeover start did not return: the floor waited on the cell lock the start holds")
	}
}

// A produce refill is an empty-in. Edge writes it as retrieve with
// retrieve_empty set; Core promotes it to retrieve_empty and its projection
// overwrites the row's order_type. Under either spelling it is a refill: it is
// counted as coming, so the floor adds nothing while two are on their way, and
// it does not work the cell, so the level keeper still asks for the swap.
func TestKeepStagedFloor_ProduceRefillsUnderCoresSpelling(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claim := keepStagedCell(t, protocol.ClaimRoleProduce, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true}, ksSpot: {}})
	holdingSwap(t, db, nodeID)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "node")
	eng.refillSpot(node, claim, 2, orders.Attached("produce-request"))
	_, err = db.DB.Exec(`UPDATE orders SET order_type=? WHERE delivery_node=?`, string(protocol.OrderTypeRetrieveEmpty), ksSpot)
	testutil.MustNoErr(t, err, "Core's spelling")

	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	for i := range rows {
		if rows[i].DeliveryNode == ksSpot && worksTheCell(&rows[i], claim) {
			t.Fatalf("refill %d (%s) works the cell: the level keeper would never ask for the swap", rows[i].ID, rows[i].OrderType)
		}
	}
	for i := 0; i < 3; i++ {
		eng.sweepCellLevels()
	}
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE delivery_node=?`, ksSpot).Scan(&n), "count")
	if n != 2 {
		t.Fatalf("orders to the spot = %d after three sweeps, want the 2 already coming", n)
	}
}
