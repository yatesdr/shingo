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
	_, err = eng.BinService().ApplyArrival(full.ID, transit.ID, false, nil)
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
