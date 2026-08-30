//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet/seerrds"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// The plant's one-bin-per-node invariant, taught to the simulator.
//
// A robot cannot lower a bin onto a position that already holds one, so in the
// field the block never reports FINISHED — the robot stalls until it clears. The
// sim has no physics and completes blocks on a timer, so on 2026-07-13 it
// "delivered" the empty onto a press before the other robot had lifted the full bin
// out. Core, handed a physically impossible event, correctly concluded the bin
// still recorded there must be a stale ghost and evicted it. Core was right; the
// sim was lying. These pin the gate that stops it lying.

func TestPositionGate_HoldsWhenAnotherOrderOwnsTheBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	press := &nodes.Node{Name: "PLN-GATE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press node")
	bt := &bins.BinType{Code: "BT-GATE", Description: "gate test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	// The full bin sits at the press, CLAIMED by the removal order that is dwelling
	// there to collect it (the full-out half of a two-robot swap).
	fullOut := &orders.Order{EdgeUUID: "gate-full-out", StationID: "line-1", OrderType: "complex", Status: "in_transit", Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(fullOut), "create full-out order")
	full := &bins.Bin{BinTypeID: bt.ID, Label: "GATE-FULL", NodeID: &press.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(full), "create full bin")
	mustExec(t, db, `UPDATE bins SET claimed_by=$1 WHERE id=$2`, fullOut.ID, full.ID) // the removal order owns it

	// The empty-in half: a DIFFERENT order, whose robot is about to place an empty
	// onto that same press.
	emptyIn := &orders.Order{EdgeUUID: "gate-empty-in", StationID: "line-1", OrderType: "complex", Status: "in_transit", Quantity: 1, VendorOrderID: "sim-empty-in"}
	testutil.MustNoErr(t, db.CreateOrder(emptyIn), "create empty-in order")
	testutil.MustNoErr(t, db.UpdateOrderVendor(emptyIn.ID, "sim-empty-in", "RUNNING", "bot-1"), "set vendor id")

	drop := seerrds.BinTaskForAction(protocol.ActionDropoff)
	ok, why := eng.CanEnterPosition("sim-empty-in", press.Name, drop)
	if ok {
		t.Fatalf("the empty-in was allowed to place onto %s while it still holds bin %d (claimed by the "+
			"removal order %d).\nA robot cannot lower a bin onto an occupied position — it must HOLD. "+
			"Completing here is what made Core evict a good bin as a stale ghost.",
			press.Name, full.ID, fullOut.ID)
	}
	if why == "" {
		t.Error("a hold must explain itself — the sim log is the only place this is visible")
	}

	// The removal order lifts the full bin out (bin leaves the press). The position
	// is now free and the empty-in may land — the real choreography, in order.
	transit, err := db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "get _TRANSIT")
	_, err = eng.BinService().ApplyArrival(full.ID, transit.ID, false, nil, 0)
	testutil.MustNoErr(t, err, "lift the full bin out of the press")

	if ok, why := eng.CanEnterPosition("sim-empty-in", press.Name, drop); !ok {
		t.Fatalf("the press is empty now, but the empty-in is still held: %s", why)
	}
}

// A PICKUP is never held. The robot is there to REMOVE the bin, so occupancy cannot
// obstruct it — and the bin is very often claimed by NOBODY, because ApplyArrival
// clears the claim when the bin lands. An ownership-only gate held two compound
// restock legs at their own shuffle slots for six minutes before the sim caught it.
func TestPositionGate_NeverHoldsAPickup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	press := &nodes.Node{Name: "PLN-OWN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create node")
	bt := &bins.BinType{Code: "BT-OWN", Description: "gate test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	pickup := &orders.Order{EdgeUUID: "gate-own", StationID: "line-1", OrderType: "complex", Status: "in_transit", Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(pickup), "create order")
	testutil.MustNoErr(t, db.UpdateOrderVendor(pickup.ID, "sim-own", "RUNNING", "bot-1"), "set vendor id")

	// The bin is at the node and claimed by NOBODY — exactly the compound-restock
	// shape. ApplyArrival clears a bin's claim when it lands, so the restock leg
	// arrives to collect a bin nobody owns.
	target := &bins.Bin{BinTypeID: bt.ID, Label: "GATE-PICKUP", NodeID: &press.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(target), "create bin")

	load := seerrds.BinTaskForAction(protocol.ActionPickup)
	if ok, why := eng.CanEnterPosition("sim-own", press.Name, load); !ok {
		t.Fatalf("order %d was HELD at %s while trying to PICK UP bin %d: %s\n"+
			"A pickup REMOVES the bin; occupancy cannot obstruct it. Holding here deadlocks the "+
			"robot against the very bin it came for — it stalled two compound restock legs for "+
			"six minutes on the sim.", pickup.ID, press.Name, target.ID, why)
	}

	wait := seerrds.BinTaskForAction(protocol.ActionWait)
	if ok, why := eng.CanEnterPosition("sim-own", press.Name, wait); !ok {
		t.Fatalf("order %d was HELD at %s on a WAIT block: %s — a robot dwelling beside a bin is "+
			"not placing onto it", pickup.ID, press.Name, why)
	}
}

// Synthetic nodes (LANE / NGRP / _TRANSIT) hold many bins by design. Gating them
// would stall every lane store.
func TestPositionGate_ExemptsSyntheticNodes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	if ok, why := eng.CanEnterPosition("sim-whatever", "_TRANSIT", seerrds.BinTaskForAction(protocol.ActionDropoff)); !ok {
		t.Fatalf("_TRANSIT is synthetic and holds many bins by design, but the gate held: %s", why)
	}
}

// TestPositionGate_HoldsWhenTheOrderIsUnresolvable pins the ONE arm in this gate
// that fails CLOSED, against a header two lines above it that says lookup
// failures never block.
//
// It reads backwards from every other read failure here and it is right.
// Occupancy is a fact ALREADY READ by the time this arm is reached; the only
// unknown left is whether the resident belongs to the arriving order. Passing
// would let a robot lower a bin onto an occupied node, which is the impossible
// event the whole gate exists to keep out of Core's model. Every other failure
// in this function leaves occupancy itself unknown, where inventing a stall out
// of a missing row is the worse error.
//
// TEST-CERTIFIED, NOT RUN-CERTIFIED. The population is an order the fleet knows
// and Core cannot look up by vendor id. No fixture produces one and the
// simulator never will, so a clean run says nothing about this arm either way.
// It also means the arm must be carved OUT of the extracted occupancy core, or
// anything that later reuses that core inherits a fail-closed decision it never
// asked for.
func TestPositionGate_HoldsWhenTheOrderIsUnresolvable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	press := &nodes.Node{Name: "PLN-UNRESOLVABLE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press node")
	bt := &bins.BinType{Code: "BT-UNRES", Description: "unresolvable-order test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	resident := &bins.Bin{BinTypeID: bt.ID, Label: "UNRES-RESIDENT", NodeID: &press.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(resident), "create resident bin")

	drop := seerrds.BinTaskForAction(protocol.ActionDropoff)
	// No order carries this vendor id.
	if ok, _ := eng.CanEnterPosition("sim-no-such-vendor-order", press.Name, drop); ok {
		t.Fatalf("the gate PASSED a placement onto %s, which holds bin %d, because it could not "+
			"resolve the order. Occupancy was already established; only ownership was unknown, "+
			"and the safe answer to an unknown owner is not 'drop onto it anyway'", press.Name, resident.ID)
	}

	// And the extracted occupancy core does NOT carry that decision: asked on its
	// own it reports the plain physical fact, with no order in the question.
	if _, occupied := eng.positionOccupiedBy(press.Name, 0); !occupied {
		t.Errorf("positionOccupiedBy said %s is free while bin %d stands on it", press.Name, resident.ID)
	}
	if _, occupied := eng.positionOccupiedBy("PLN-NO-SUCH-NODE", 0); occupied {
		t.Errorf("positionOccupiedBy reported a MISSING node as occupied. The core fails OPEN on " +
			"every read: it is a physics model, not a validator, and the gate's fail-closed arm " +
			"lives at the call site precisely so it does not ride along here")
	}
}

// TestPositionGate_OccupancyCoreExcludesTheOrdersOwnBin pins the multi-bin case
// through the extracted core, so the sim's physics and any later reader of
// "is this position obstructed" cannot answer it two different ways.
func TestPositionGate_OccupancyCoreExcludesTheOrdersOwnBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	press := &nodes.Node{Name: "PLN-OWNBIN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press node")
	bt := &bins.BinType{Code: "BT-OWNBIN", Description: "own-bin test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	owner := &orders.Order{EdgeUUID: "ownbin-order", StationID: "line-1", OrderType: "complex", Status: "in_transit", Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(owner), "create order")
	mine := &bins.Bin{BinTypeID: bt.ID, Label: "OWNBIN-MINE", NodeID: &press.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(mine), "create bin")
	mustExec(t, db, `UPDATE bins SET claimed_by=$1 WHERE id=$2`, owner.ID, mine.ID)

	if _, occupied := eng.positionOccupiedBy(press.Name, owner.ID); occupied {
		t.Errorf("a bin order %d already owns read as an obstruction TO ITSELF at %s — a multi-bin "+
			"order placing beside its own load would hold against itself forever", owner.ID, press.Name)
	}
	if _, occupied := eng.positionOccupiedBy(press.Name, owner.ID+1000); !occupied {
		t.Errorf("the same bin did NOT obstruct a different order at %s", press.Name)
	}
	if _, occupied := eng.positionOccupiedBy(press.Name, 0); !occupied {
		t.Errorf("forOrderID=0 means 'obstructed by anything' and must not excuse a claimed bin")
	}

	// A retired bin is a row kept for audit; the carrier is off the floor.
	mustExec(t, db, `UPDATE bins SET status='retired' WHERE id=$1`, mine.ID)
	if _, occupied := eng.positionOccupiedBy(press.Name, 0); occupied {
		t.Errorf("a RETIRED bin obstructed %s. The row survives for audit; the carrier does not "+
			"survive on the floor, and holding a robot against it stalls a real placement", press.Name)
	}
}
