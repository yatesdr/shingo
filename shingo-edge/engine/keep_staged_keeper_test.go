package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// THE KEEPER: one decision for a keep-staged spot (decideSpot), asked by the
// sweep, by REQUEST and by an order's end. Most tests seed a keep-staged
// consume cell whose swap leg sits in the runtime slot still acquiring
// (queued), and a bare spot.

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

func keeperCell(t *testing.T) (*Engine, *store.DB, int64) {
	t.Helper()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {}})
	return eng, db, nodeID
}

// A refill that ended failed, skipped or cancelled is not re-created: a
// structural failure would repeat on a timer, and a cancel is someone saying
// stop (SPR 2026-10-08: five cancels, five identical re-creations). REQUEST
// re-arms it.
func TestKeepStagedKeeper_EndedRefillIsNotRecreated(t *testing.T) {
	t.Parallel()
	for _, ended := range []protocol.Status{protocol.StatusFailed, protocol.StatusSkipped, protocol.StatusCancelled} {
		t.Run(string(ended), func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID := keeperCell(t)
			holdingSwap(t, db, nodeID)
			endedRefill(t, eng, db, nodeID, ended)
			for i := 0; i < 5; i++ {
				eng.sweepCellLevels()
			}
			if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
				t.Fatalf("the keeper re-created %d refill(s) after one ended %s", got.refills, ended)
			}
		})
	}
}

// SPR ALN_011 2026-10-08. The swap was planned before keep-staged was switched
// on: it fetches its own carrier and drops it ON the spot as its staging stop,
// and a wrong bin stands there. The floor used to read the waiting swap as
// lifting a spare, and ordered a return of the standing bin plus two refills:
// the return queued behind the swap's own carrier, the swap behind the bin. It
// must order nothing while a live leg still drops onto the spot.
func TestKeepStagedKeeper_SwapThatDropsOnTheSpotOrdersNothing(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart},
			ksSpot: {Occupied: true, PayloadCode: "SOMETHING-ELSE", BinID: 150}})
	claim := keeperClaimByID(t, db, nodeID)
	planned := *claim
	planned.KeepStagedNode = "" // planned before the flag
	a, _ := BuildTwoRobotSwapSteps(&planned)
	leg := mkSwapLeg(t, db, nodeID, "pre-flag-swap", a, "")
	testutil.MustNoErr(t, db.UpdateOrderStatus(leg.ID, string(protocol.StatusQueued)), "queued")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &leg.ID, nil), "slot")

	for i := 0; i < 3; i++ {
		eng.sweepCellLevels()
	}
	got := readSpotOrders(t, db, nodeID)
	if got.refills != 0 || got.returns != 0 {
		t.Fatalf("refills=%d returns=%d, want 0 and 0: the swap drops onto the spot itself", got.refills, got.returns)
	}
}

func TestKeepStagedKeeper_ARefillOnItsWaySuppresses(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := keeperCell(t)
	holdingSwap(t, db, nodeID)
	_, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, false, 1, ksSpot, ksMarket, "", "standard", ksPart, true, false,
		orders.Attached("floor-coming"))
	testutil.MustNoErr(t, err, "refill on its way")
	eng.sweepCellLevels()
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Fatalf("refills = %d, want only the one already on its way", got.refills)
	}
}

// An idle keep-staged line with a bare spot is topped up: the keeper runs on
// the sweep whether or not a swap is in flight, and orders exactly one.
func TestKeepStagedKeeper_IdleBareSpotGetsOneRefill(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := keeperCell(t)
	for i := 0; i < 3; i++ {
		eng.sweepCellLevels()
	}
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Fatalf("refills = %d on an idle bare spot after three sweeps, want exactly 1", got.refills)
	}
}

// A REQUEST and the floor deciding in the same moment create the spot's orders
// once. Both hold the cell's prime lock from their read of what is coming to
// their create. Here the test holds it the way a request's apply does: the
// floor, started in that moment with its swap waiting and nothing coming yet,
// finds the cell being decided and leaves it to the decider; the "request"
// writes its refills. Without the lock the floor reads nothing coming and adds
// its own.
func TestKeepStagedKeeper_RequestAndFloorTogetherCreateOnce(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := keeperCell(t)
	holdingSwap(t, db, nodeID)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "node")
	claim := keeperClaimByID(t, db, nodeID)
	mu := eng.primeNodeLock(claim)
	mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.keepSpot(node, claim)
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
func TestKeepStagedKeeper_StartAbortingTheCellDoesNotWaitOnItself(t *testing.T) {
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

	// The start runs on the test's own goroutine, with no clock of its own. Its
	// return is the answer; how long it takes says nothing. Under -race with
	// the package's parallel tests sharing the SQLite driver's one allocator
	// lock, a start that waits on nothing has taken 32 s. A start that waits on
	// its own lock never returns, and go test -timeout fails it with every
	// goroutine's stack, the floor's among them.
	_, err = fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged")
	testutil.MustNoErr(t, err, "start")
}

// A produce refill is an empty-in. Edge writes it as retrieve with
// retrieve_empty set; Core promotes it to retrieve_empty and its projection
// overwrites the row's order_type. Under either spelling it is a refill: it is
// counted as coming, so the floor adds nothing while two are on their way, and
// it does not work the cell, so the level keeper still asks for the swap.
func TestKeepStagedKeeper_ProduceRefillsUnderCoresSpelling(t *testing.T) {
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
