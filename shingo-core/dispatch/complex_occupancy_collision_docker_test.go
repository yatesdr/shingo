//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// complex_occupancy_collision_docker_test.go — the pair, proved from the fleet's
// record rather than from the ledger under test.
//
// ── WHY THESE DO NOT READ OCCUPANCY ROWS ───────────────────────────────────
//
// The defect was a missing occupancy row. A test that asserts the fix by reading
// occupancy rows asserts that the bookkeeping agrees with itself, which is the
// one thing that was never in question — the rows were consistent, there were
// just too few of them. So the collision assertion below counts ORDERS THE FLEET
// IS EXECUTING whose path lies in the lane: an order with a vendor order id has
// had a robot committed to it, and that fact is written by the handover, not by
// the reservation.
//
// It is not perfect ground truth and the difference is worth naming, because
// over-claiming here would repeat the mistake. Real ground truth is where the
// robot physically is. Two candidates exist and neither is reachable from a
// docker test today:
//
//   - scenesim moves robot tokens over a real scene and could witness two
//     robots in one lane directly (committedTo, scenesim/checkers.go). It does
//     not consume Core's dispatch — no import in either direction — so a
//     complex order dispatched here never becomes a token there. Bridging them
//     needs a scene-aware fleet.Backend that does not exist.
//   - fleet.RobotStatus carries CurrentStation from the vendor and is already
//     polled into the engine's robot cache. Nothing joins it to lane geometry.
//
// What this test asserts is therefore: Core did not COMMIT a second robot to a
// lane that already had one. That is the decision Core owns and the decision the
// defect corrupted. Where the robot then physically goes is the vendor's, and it
// is [L] first-light work.

// committedInLane counts the orders the fleet is currently executing whose
// source or destination lies in this lane — the fleet's record of committed
// robots, deliberately NOT reservations.
func committedInLane(t *testing.T, db *store.DB, laneID int64) []int64 {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT DISTINCT o.id
		FROM orders o
		JOIN nodes s ON s.name IN (o.source_node, o.delivery_node)
		WHERE s.parent_id = $1
		  AND o.vendor_order_id <> ''
		  AND NOT (o.status = ANY($2))
		ORDER BY o.id`, laneID, terminalStatusArray())
	if err != nil {
		t.Fatalf("count committed orders in lane %d: %v", laneID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func terminalStatusArray() []string {
	var out []string
	for _, s := range protocol.AllStatuses() {
		if protocol.IsTerminal(s) {
			out = append(out, string(s))
		}
	}
	return out
}

// TestCollision_PlainStoreIsRefusedFromACorridorAComplexOrderOccupies is the
// direction the defect broke.
//
// Before the fix the complex order left no trace, so this plain store's
// admission found an empty lane and let it in — LAWFULLY. Nobody did anything
// wrong and two robots ended up in one single-file corridor.
//
// MUTATION (verified 2026-08-10): drop the entering nodes from complex's
// commitToFleet call (`d.commitToFleet(order, req, "scanner")`). This fires with
// two committed orders in one lane, which is the collision itself rather than a
// proxy for it.
func TestCollision_PlainStoreIsRefusedFromACorridorAComplexOrderOccupies(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	srcNode, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "COLL-A")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// The complex order goes in first and is executing.
	testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "COLL-A-CPLX-BIN")
	cplx := &orders.Order{
		EdgeUUID: "coll-a-complex", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: srcNode.Name, DeliveryNode: mouth.Name, ProcessNode: srcNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + srcNode.Name + `"},` +
			`{"action":"dropoff","node":"` + mouth.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(cplx), "create the complex order")
	cplx, _ = db.GetOrder(cplx.ID)
	testutil.MustNoErr(t, d.DispatchPreparedComplex(cplx), "the complex order dispatches")

	if got := committedInLane(t, db, laneID); len(got) != 1 {
		t.Fatalf("after the complex dispatch the lane has %v committed — the fixture needs exactly "+
			"one before the second order is tried", got)
	}

	// Now a plain store wants the same corridor.
	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "COLL-A-PLAIN-BIN")
	plain := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "coll-a-plain"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(plain.ID, bin.ID), "stamp the bin")
	plain, _ = db.GetOrder(plain.ID)

	admitted, cause, lane, err := d.AcquireLanesForOrder(plain, srcNode, mouth, EntryHeldBin)
	testutil.MustNoErr(t, err, "the admission ask must not error")
	if admitted {
		// Follow through so the failure names the real consequence rather than a
		// verdict: actually send it, then count what is in the corridor.
		_, _ = d.DispatchDirect(plain, srcNode, mouth)
		t.Fatalf("the plain store was ADMITTED into a lane order %d is already executing in.\n"+
			"Committed orders in the corridor: %v.\n"+
			"Nothing here is illegal — that is the point. The ledger said the lane was empty "+
			"because the complex arm never wrote its page, so the admission was lawful and two "+
			"robots went into one single-file lane.", cplx.ID, committedInLane(t, db, laneID))
	}
	if cause != CauseLaneOccupied {
		t.Errorf("refused with cause %q, want %q — the operator sentence has to say a robot is in "+
			"the lane, not something vaguer", cause, CauseLaneOccupied)
	}
	if lane == "" {
		t.Error("the refusal names no lane; the queue row cannot tell an operator where to look")
	}
}

// TestCollision_ComplexIsRefusedFromACorridorAPlainStoreOccupies is the other
// direction, and it already worked — complex has always ASKED.
//
// It is here so the pair is one test file rather than a fix and a fact recorded
// in different places. If a later change breaks the read half, the two failures
// arrive together and the shape is obvious.
func TestCollision_ComplexIsRefusedFromACorridorAPlainStoreOccupies(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	srcNode, _, bp := setupTestData(t, db)
	laneID, mouth := seamLane(t, db, "COLL-B")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "COLL-B-PLAIN-BIN")
	plain := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "coll-b-plain"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(plain.ID, bin.ID), "stamp the bin")
	plain, _ = db.GetOrder(plain.ID)
	_, dErr := d.DispatchDirect(plain, srcNode, mouth)
	testutil.MustNoErr(t, dErr, "the plain store dispatches")

	testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "COLL-B-CPLX-BIN")
	cplx := &orders.Order{
		EdgeUUID: "coll-b-complex", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusQueued, Quantity: 1, PayloadCode: bp.Code,
		SourceNode: srcNode.Name, DeliveryNode: mouth.Name, ProcessNode: srcNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + srcNode.Name + `"},` +
			`{"action":"dropoff","node":"` + mouth.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(cplx), "create the complex order")
	cplx, _ = db.GetOrder(cplx.ID)
	_ = d.DispatchPreparedComplex(cplx)

	if got := committedInLane(t, db, laneID); len(got) != 1 || got[0] != plain.ID {
		t.Fatalf("committed orders in the corridor = %v, want only the plain store (%d) — the "+
			"complex order was let into a lane a robot is already in", got, plain.ID)
	}
	after, _ := db.GetOrder(cplx.ID)
	if protocol.IsTerminal(after.Status) {
		t.Errorf("the complex order is %q — a busy lane is congestion, so it waits rather than "+
			"dying of it", after.Status)
	}
}

// TestSeamGuard_UnderDeclaringALaneRefusesTheDispatch pins the invariant at the
// place it is created, which is the difference between this and the soak query.
//
// soakstat's phantom-entrant check finds an executing order holding no occupancy
// row — after the fact, and by guessing where the robot is from the order's
// endpoint columns, which describe the whole order rather than the step it is
// on. That query stays as a backstop. But it is compensating for a missing
// invariant rather than expressing one, and its own comment concedes the
// attribution is coarse.
//
// At the seam there is nothing to guess. The blocks ARE the instruction, the
// declaration sits beside them, and the comparison is exact. An arm that
// under-declares does not dispatch at all.
//
// The direct call is deliberate: this is a contract between the seam and its
// callers, and driving it through an arm would test the arm instead. The arms
// are covered by the collision pair above.
func TestSeamGuard_UnderDeclaringALaneRefusesTheDispatch(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	srcNode, _, bp := setupTestData(t, db)
	_, mouth := seamLane(t, db, "SEAMGUARD")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "SEAMGUARD-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "seamguard"
		o.OrderType = OrderTypeMove
		o.PayloadCode = bp.Code
		o.SourceNode = srcNode.Name
		o.DeliveryNode = mouth.Name
		o.Status = StatusSourcing
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp the bin")
	order, _ = db.GetOrder(order.ID)

	// The blocks enter the lane; the declaration does not mention it.
	req := fleet.CreateOrderRequest{
		OrderID:    mintVendorOrderID(order.ID),
		ExternalID: order.EdgeUUID,
		Blocks: []fleet.OrderBlock{
			{BlockID: "1", Location: srcNode.Name},
			{BlockID: "2", Location: mouth.Name},
		},
	}
	err := d.commitToFleet(order, req, "test") // no entering nodes — the bug, in one argument

	if err == nil {
		t.Fatalf("the seam accepted a dispatch into lane %s that declared no lane. That order would "+
			"execute holding no occupancy row, and the next entrant would be admitted into the "+
			"corridor lawfully.", mouth.Name)
	}
	sent, _ := db.GetOrder(order.ID)
	if sent.VendorOrderID != "" {
		t.Errorf("a refused commit still reached the fleet as %q — the guard has to run BEFORE the "+
			"handover or it is a report rather than a guard", sent.VendorOrderID)
	}
	if got := occupants(t, db, mouthLaneOf(t, db, mouth)); len(got) != 0 {
		t.Errorf("occupants after a refused commit = %v, want none — nothing was sent, so any row "+
			"left behind wedges the lane with nothing alive to clear it", got)
	}
}

// mouthLaneOf resolves the lane a slot belongs to.
func mouthLaneOf(t *testing.T, db *store.DB, slot *nodes.Node) int64 {
	t.Helper()
	lane, err := db.LaneForNode(slot.ID)
	if err != nil || lane == nil {
		t.Fatalf("resolve lane for %s: %v", slot.Name, err)
	}
	return lane.ID
}
