//go:build docker

package dispatch

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

// twoLegCompound builds a parent with two pending children, both working the
// same lane: leg one lifts the mouth slot, leg two the slot behind it. It
// returns the parent, the children in sequence order, and the lane.
//
// The lane and its slots are REAL nodes and the children are REAL orders. The
// scenario is driven through AdvanceCompoundOrder and the production occupancy
// release, never by writing the state a test wants to see — a fixture that sets
// its own answer proves nothing, which this branch has already paid for twice.
func twoLegCompound(t *testing.T, db *store.DB, prefix string) (*orders.Order, []*orders.Order, int64, []*nodes.Node) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, prefix+"-LANE", 3)
	slots, err := db.ListLaneSlots(lane)
	if err != nil || len(slots) < 2 {
		t.Fatalf("list lane slots: %v (got %d)", err, len(slots))
	}

	parent := &orders.Order{
		EdgeUUID: prefix + "-parent", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: protocol.StatusReshuffling, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	var children []*orders.Order
	for i := range 2 {
		c := &orders.Order{
			EdgeUUID: prefix + "-child" + string(rune('1'+i)), StationID: "line-1",
			OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
			ParentOrderID: &parent.ID, Sequence: i + 1,
			SourceNode: slots[i].Name, DeliveryNode: sd.LineNode.Name,
		}
		testutil.MustNoErr(t, db.CreateOrder(c), "create child")
		children = append(children, c)
	}
	return parent, children, lane, slots
}

func inFlight(t *testing.T, db *store.DB, id int64) bool {
	t.Helper()
	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("get order %d: %v", id, err)
	}
	return o.VendorOrderID != "" && !protocol.IsTerminal(o.Status)
}

// TestCompound_TwoChildrenInFlightAtOnce is the positive proof for retiring the
// sibling-in-flight guard, and it asserts the property rather than inferring it
// from a green suite.
//
// Removing a constraint proves nothing on its own. A run in which no two
// children were ever dispatched together passes identically whether the guard is
// there or not, which is exactly how three cases in the scenesim harness came to
// be vacuous. So this asserts the thing the step exists to make possible: TWO
// CHILDREN OF ONE RESHUFFLE, BOTH CARRYING VENDOR ORDER IDS, NEITHER TERMINAL,
// AT THE SAME MOMENT.
//
// The sequence is the real one:
//
//  1. leg one dispatches and takes the lane (Hold B)
//  2. leg two is refused while leg one is inside — the lane gate, not the
//     sibling rule
//  3. leg one PLACES its bin: occupancy released, leg one still non-terminal
//     because whole-order FINISHED has not arrived
//  4. leg two dispatches into that window
//
// Step 3 is where the old guard and the new rule differ. The guard asked "is any
// sibling non-terminal" and leg one still is; the lane gate asks "is anyone
// inside the lane" and nobody is.
//
// MUTATION (verified): restore the sibling-in-flight loop at the top of
// AdvanceCompoundOrder. Step 4 then leaves leg two pending and this test's own
// assertion fails with "only one child is in flight" — no checker involved,
// because this is the dispatcher and there are none.
func TestCompound_TwoChildrenInFlightAtOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "CONC")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// 1. Leg one goes out and takes the lane.
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (leg one): %v", err)
	}
	if !inFlight(t, db, children[0].ID) {
		t.Fatal("leg one was not dispatched")
	}
	if occ, _ := reservations.OccupantsOf(db.DB, lane); len(occ) != 1 {
		t.Fatalf("lane occupants after leg one dispatched = %v, want exactly leg one", occ)
	}

	// 2. Leg two must wait — the lane is occupied, not the siblings busy.
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (leg two, blocked): %v", err)
	}
	if inFlight(t, db, children[1].ID) {
		t.Fatal("leg two entered a lane leg one is still inside — the Hold B gate did not hold")
	}

	// 3. Leg one PLACES its bin. Occupancy ends; the order does not.
	d.ReleaseLaneOccupancy(children[0].ID)
	if occ, _ := reservations.OccupantsOf(db.DB, lane); len(occ) != 0 {
		t.Fatalf("lane occupants after leg one placed = %v, want none", occ)
	}
	if !inFlight(t, db, children[0].ID) {
		t.Fatal("leg one must still be IN FLIGHT after placing — if it is already terminal this test " +
			"cannot distinguish the new rule from the old one, and proves nothing")
	}

	// 4. Leg two enters that window.
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (leg two, lane clear): %v", err)
	}

	// THE ASSERTION. Both legs of one reshuffle, in flight, at the same moment.
	oneUp, twoUp := inFlight(t, db, children[0].ID), inFlight(t, db, children[1].ID)
	if !oneUp || !twoUp {
		c0, _ := db.GetOrder(children[0].ID)
		c1, _ := db.GetOrder(children[1].ID)
		t.Fatalf("only one child is in flight — leg one (%s, vendor %q) and leg two (%s, vendor %q). "+
			"Two legs of one reshuffle must be able to overlap once the lane is clear; that overlap IS "+
			"what removing the sibling-in-flight guard was for",
			c0.Status, c0.VendorOrderID, c1.Status, c1.VendorOrderID)
	}
	if occ, _ := reservations.OccupantsOf(db.DB, lane); len(occ) != 1 || occ[0] != children[1].ID {
		t.Errorf("lane occupants = %v, want exactly leg two (%d) — one inside while two are in flight",
			occ, children[1].ID)
	}
}

// TestCompound_LaneGateHoldsWhileOccupied is the other half: overlap is allowed
// only when the lane is clear. It is the assertion that replaces the retired
// cascade guard, stated as a property of the LANE rather than of the sibling set.
//
// MUTATION (verified): delete admission's occupancy arm -- the OccupantsOf
// refusal in admitLane (admission.go), which is the check AdvanceCompoundOrder
// now asks for. Leg two then dispatches straight into the occupied lane and
// this test's own "entered a lane leg one is still inside" assertion fires.
func TestCompound_LaneGateHoldsWhileOccupied(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, lane, _ := twoLegCompound(t, db, "GATE")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance (leg one): %v", err)
	}

	// Re-drive repeatedly: the production path calls this on every block
	// completion, so a gate that only holds on the first attempt is not a gate.
	for range 5 {
		if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
			t.Fatalf("advance (repeat): %v", err)
		}
	}

	if inFlight(t, db, children[1].ID) {
		t.Fatal("leg two entered a lane leg one is still inside")
	}
	if occ, _ := reservations.OccupantsOf(db.DB, lane); len(occ) != 1 || occ[0] != children[0].ID {
		t.Errorf("lane occupants = %v, want exactly leg one (%d)", occ, children[0].ID)
	}
}
