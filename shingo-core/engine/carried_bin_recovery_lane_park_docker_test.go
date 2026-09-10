//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ---------------------------------------------------------------------------
// The recovery door's LANE arm parks, like the sibling door it says it mirrors.
//
// carried_bin_recovery.go asks the lane question at the same point bin_move.go
// asks it, with the same EntryKind, and its own doc says the sequence "mirrors
// the bin-move door because it is the same situation". One arm did not: a busy
// corridor — self-clearing, the most ordinary refusal in the plant — TERMINALIZED
// the recovery order, while bin_move parked the identical verdict.
//
// The conversion is only safe if something re-drives the park, so that is what
// these prove. A park nobody releases is a wedge wearing a cause, which is worse
// than the loud fail it replaces.
// ---------------------------------------------------------------------------

// laneWithSlot builds an unmarked lane in a group, with one slot at the mouth.
//
// THE GROUP IS NOT DECORATION. resolveOrderLaneHolds takes no mouth hold when
// `lane.ParentID == nil` ("a lane with no group — no hold"), so a parentless lane
// cannot be held and this test would silently exercise nothing: the recovery
// order would sail past the lane question and dispatch.
func laneWithSlot(t *testing.T, db *store.DB, prefix string) (lane, slot *nodes.Node) {
	t.Helper()
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")

	grp := &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &ngrpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create lane group")
	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &laneType.ID, ParentID: &grp.ID,
		Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	d0 := 0
	slot = &nodes.Node{Name: prefix + "-LANE-S0", ParentID: &lane.ID, Enabled: true, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	return lane, slot
}

// TestRecoveryOrderParksOnAHeldLaneAndTheScannerDispatchesIt is the anti-wedge
// proof, and it is the whole justification for converting the arm.
//
// The worry is specific and written into this file's own header: "the scanner
// would never pick this up — it sources a move order by FINDING a bin at the
// source node, and every finder excludes synthetic nodes". True of the FIND
// path. A recovery order is born holding orders.bin_id, so the scanner routes it
// to dispatchHeldBin, which reuses the held bin and never calls a finder.
//
// Lane held → the order parks with a cause instead of dying. Lane clears → the
// scanner dispatches it, with the pinned unload-only plan the door would have
// sent itself.
func TestRecoveryOrderParksOnAHeldLaneAndTheScannerDispatchesIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	lane, slot := laneWithSlot(t, db, "CBR-PARK")
	bin := seedCarried(t, db, "AMR-PARK", slot.Name)
	cacheRobot(eng, dispatchableRobot("AMR-PARK"))

	// SOMEBODY ELSE IS IN THE CORRIDOR. Taken through the same public door the
	// recovery order will use, so the refusal is the real one and not a row this
	// test invented.
	//
	// Its delivery_node deliberately is NOT the recovery slot: usableDropPoint
	// counts active orders aimed at a node, so a blocker pointed at the same slot
	// would make the destination unusable and the door would refuse one tier
	// earlier, never reaching the lane question this test is about. The mouth hold
	// is resolved from the node PASSED to AcquireLanesForOrder, so the lane is
	// held either way.
	elsewhere := &nodes.Node{Name: "CBR-PARK-ELSEWHERE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the blocker's own destination")
	//
	// AND IT IS in_transit, NOT queued. A queued blocker is in the scanner's own
	// scan set: the synchronous scan this test triggers picked it up, failed it
	// for having no payload_code, and terminalizing RELEASED ITS LANE — so the
	// corridor was free by the time the assertion ran and the test proved nothing
	// about the park. in_transit is also the honest state for what this row
	// represents: a robot actually inside the lane.
	blocker := &orders.Order{
		EdgeUUID: "cbr-park-blocker", StationID: "edge.test", OrderType: "move",
		Status: protocol.StatusInTransit, Quantity: 1, DeliveryNode: elsewhere.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(blocker), "create blocking order")
	// IN ModeDig, which is the mode that excludes everyone. Two INBOUND holds
	// SHARE a lane (admitMouth: "only an exact same-mode share is admitted"), so a
	// blocker merely aimed into the lane refuses nothing — the recovery order
	// would take its own inbound row beside it and dispatch, and this test would
	// assert nothing while passing.
	//
	// Taken through the reservations API rather than AcquireLanesForOrder because
	// the latter's admission first resolves a PICKUP SLOT for the source, and this
	// blocker owns no bin — the fixture would fail on the setup rather than on the
	// thing under test.
	testutil.MustNoErr(t, reservations.AcquireLanes(db.DB, blocker.ID, reservations.ModeDig,
		"test-blocker", lane.ID), "blocker takes the lane in dig mode")

	// THE PRESS. It must not fail: a busy corridor is a wait, not an answer.
	order, _, rerr := eng.RecoverCarriedBin(bin.ID, "operator:test")
	if rerr != nil {
		t.Fatalf("recovery refused on a held lane: %v\n"+
			"A lane refusal is congestion and congestion waits. The sibling door (bin_move.go) parks "+
			"the identical verdict at the identical point; terminalizing here also burns this "+
			"(bin, robot) pair's one deterministic edge_uuid.", rerr)
	}
	if order == nil {
		t.Fatal("recovery returned no order")
	}

	parked, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read the parked order")
	if protocol.IsTerminal(parked.Status) {
		t.Fatalf("the parked recovery order is %s — a lane refusal must not terminalize it", parked.Status)
	}
	// ACQUIRING, not `queued` specifically. The door parks at `queued`; the emit
	// runs the scanner synchronously on this goroutine, and the scanner's own
	// re-ask finds the lane still held and re-parks at `sourcing`. Both are in the
	// acquiring set, both are re-driven, and pinning one of them would be pinning
	// how many times the scanner happened to run.
	if !protocol.IsAcquiring(parked.Status) {
		t.Errorf("parked recovery order status = %s, want it in the acquiring set {queued, sourcing}", parked.Status)
	}
	if parked.QueueCause == "" {
		t.Error("the parked order carries no queue_cause — a wait with no named cause is a stall")
	}
	if parked.BinID == nil {
		t.Fatal("the parked order lost its bin pointer — dispatchHeldBin is the arm that re-drives " +
			"it, and orders.bin_id is what routes it there")
	}

	// AND THE WAIT IS IN HISTORY, not only in a column the next park overwrites.
	history, herr := db.ListOrderHistory(order.ID)
	testutil.MustNoErr(t, herr, "list history")
	var queuedRow *orders.History
	for _, h := range history {
		if h.Status == protocol.StatusQueued {
			queuedRow = h
			break
		}
	}
	if queuedRow == nil {
		t.Fatal("no queued row in the parked recovery order's history")
	}
	if queuedRow.Code == "" {
		t.Error("the queued history row is blank — the code must ride the transition off the order struct")
	}

	// THE LANE CLEARS.
	testutil.MustNoErr(t, reservations.ReleaseLane(db.DB, blocker.ID, lane.ID), "blocker leaves the lane")

	// AND THE ORDINARY MACHINERY PICKS IT UP. No second implementation, no
	// bespoke redrive: the fulfillment scanner's held-bin path, which is what the
	// park is betting on.
	eng.fulfillment.RunOnce()

	done, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "re-read after the lane cleared")
	if protocol.IsAcquiring(done.Status) {
		t.Fatalf("the recovery order is still %s with a free lane and its cause is %q.\n"+
			"NOTHING RE-DRIVES IT, which makes this park a wedge and the old loud fail the better "+
			"disposition. The park is only correct if dispatchHeldBin drives it: bin_id=%v, "+
			"source=%q.", done.Status, done.QueueCause, done.BinID, done.SourceNode)
	}
	if done.VendorOrderID == "" {
		t.Error("the order left the acquiring set but holds no vendor order id — it did not reach the fleet")
	}
	// ── THE PIN SURVIVED THE PARK, WHICH IS THE OTHER HALF OF THE PROOF ──
	//
	// Asserted on the FLEET REQUEST and not on orders.robot_id, because the
	// vendor update overwrites that column at dispatch with the robot the fleet
	// assigned (UpdateVendor) — on this backend, none. The question is what Core
	// SENT, and for a carried-bin recovery the vehicle pin is the whole order:
	// unload-only, on the robot that is holding the bin.
	//
	// This is what the park nearly broke. orders.Create's INSERT has no robot_id,
	// so the pin lived only on the in-memory struct — invisible while the door
	// dispatched inside its own call, and gone the moment anything re-read the
	// order. The scanner dispatched with Vehicle="" until the door started
	// stamping the pin.
	reqs := backend.CreateRequests()
	if len(reqs) == 0 {
		t.Fatal("the fleet was never asked to create anything")
	}
	last := reqs[len(reqs)-1]
	if last.Vehicle != "AMR-PARK" {
		t.Errorf("the scanner dispatched the recovery with Vehicle=%q, want AMR-PARK.\n"+
			"An unpinned unload-only plan tells whichever robot the fleet picks to put down a bin "+
			"it is not carrying. orders.Create does not persist robot_id, so a parked recovery order "+
			"loses its pin unless the door writes it down.", last.Vehicle)
	}
}

// TestRecoveryFleetRefusalStaysTerminal pins the arm that does NOT convert.
//
// A fleet refusal is the sanctioned demand-by-hand exception: a person pressed
// Recover and is owed an answer, and nothing re-drives a plant whose fleet is
// down. Parking here would hand the operator a row with a cause and no releaser
// — the exact shape the lane conversion above is careful to avoid.
func TestRecoveryFleetRefusalStaysTerminal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	dest := &nodes.Node{Name: "CBR-FLEET-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-FLEET", dest.Name)
	cacheRobot(eng, dispatchableRobot("AMR-FLEET"))

	backend.SetFail(true)
	if _, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test"); err == nil {
		t.Fatal("a fleet refusal must still answer the operator, not park silently")
	}
}
