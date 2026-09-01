//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// store_redirect_docker_test.go — a destination is a plan too.
//
// Selection already diverts off dig-locked lanes, so a store CHOOSING now never
// picks one. The gap was the store that chose EARLIER: intake resolved a group to
// a concrete slot, the order queued behind inventory, and a dig took that lane
// while it waited. Nothing re-selected, admission refused it at dispatch, and it
// sat out the whole excavation with a sibling lane standing empty.

// twoLaneGroup builds an NGRP with two 2-deep lanes, both empty.
func twoLaneGroup(t *testing.T, db *store.DB, prefix string) (grp *nodes.Node, laneA, laneB *nodes.Node, slotsA, slotsB []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mk := func(name string) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		var slots []*nodes.Node
		for d := 1; d <= 2; d++ {
			depth := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	laneA, slotsA = mk(prefix + "-LANE-A")
	laneB, slotsB = mk(prefix + "-LANE-B")
	grp, _ = db.GetNode(grp.ID)
	return grp, laneA, laneB, slotsA, slotsB, bp
}

// parkedStore builds a store order already aimed at a concrete lane slot, holding
// a soft reservation on that slot and on its bin — the state a store queued at
// intake actually sits in.
func parkedStore(t *testing.T, db *store.DB, d *Dispatcher, uuid string, dest *nodes.Node, bin int64) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = protocol.OrderTypeMove
		o.DeliveryNode = dest.Name
		o.Status = protocol.StatusQueued
	})
	testutil.MustNoErr(t, db.ReserveSlot(dest.ID, o.ID), "reserve the destination slot")
	testdb.ReserveBin(t, db, o.ID, bin)
	if err := db.UpdateOrderBinID(o.ID, bin); err != nil {
		t.Fatalf("stamp bin_id: %v", err)
	}
	reloaded, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	return reloaded
}

func holdsSlot(t *testing.T, db *store.DB, orderID, nodeID int64) bool {
	t.Helper()
	rows, err := db.ListReservationsByOrder(orderID)
	testutil.MustNoErr(t, err, "list reservations")
	for _, r := range rows {
		if r.Kind == reservations.KindSlot && r.NodeID == nodeID {
			return true
		}
	}
	return false
}

func holdsBin(t *testing.T, db *store.DB, orderID, binID int64) bool {
	t.Helper()
	rows, err := db.ListReservationsByOrder(orderID)
	testutil.MustNoErr(t, err, "list reservations")
	for _, r := range rows {
		if r.Kind == reservations.KindBin && r.BinID == binID {
			return true
		}
	}
	return false
}

// TestStoreRedirect_DugLaneWithAFreeSibling_ReSelects is the residual 3b left,
// closed.
//
// MUTATION (verified): delete the redirectStoreOffDugLane call from
// ReserveStorageDropoff. The order keeps its destination in the dug lane, and the
// assertion on the new delivery node fires — the store waits out the excavation
// with a free sibling lane beside it.
func TestStoreRedirect_DugLaneWithAFreeSibling_ReSelects(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	grp, laneA, laneB, slotsA, _, bp := twoLaneGroup(t, db, "SR-DIV")
	d := window4Dispatcher(t, db) // a dispatcher with a real resolver

	bin := createTestBinAtNode(t, db, bp.Code, grp.ID, "SR-DIV-BIN")
	order := parkedStore(t, db, d, "sr-div", slotsA[1], bin.ID)

	// A dig takes lane A after the destination was chosen.
	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "sr-div-digger" })
	if !d.laneLock.TryLock(laneA.ID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	// The re-entry every dispatch attempt makes.
	sd := d.ReserveStorageDropoff(order)
	settled, rErr := sd.Node, sd.Err
	testutil.MustNoErr(t, rErr, "re-entry")

	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")

	// THE RETURNED NODE IS THE RE-AIMED ONE. The row being right was never the
	// whole job: the callers read the destination BEFORE this call and used to
	// keep it, so a re-aimed order declared the old lane, confirmed the slot whose
	// reservation this just released, and planned the robot into the dug lane the
	// re-aim exists to avoid — with the record showing the new one throughout.
	if settled == nil {
		t.Fatal("re-entry returned no destination; a nil settle makes the caller keep its stale node")
	}
	if settled.Name != after.DeliveryNode {
		t.Fatalf("re-entry returned %s but the row says %s — a caller using the return value and one "+
			"reading the row would send the robot to different lanes", settled.Name, after.DeliveryNode)
	}
	if after.DeliveryNode == slotsA[1].Name {
		t.Fatalf("the order is still aimed at %s, inside dug lane %s — admission will refuse it and "+
			"it will wait out the whole excavation while lane %s stands empty",
			after.DeliveryNode, laneA.Name, laneB.Name)
	}
	newDest, err := db.GetNodeByDotName(after.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the new destination")
	if newDest.ParentID == nil || *newDest.ParentID != laneB.ID {
		t.Errorf("re-aimed to %s, which is not in the free sibling lane %s", after.DeliveryNode, laneB.Name)
	}

	// THE OLD SLOT IS GIVEN BACK. A hold on a slot the order is no longer going to
	// is a slot nobody else can use.
	if holdsSlot(t, db, order.ID, slotsA[1].ID) {
		t.Error("the order still holds its reservation on the abandoned slot")
	}

	// AND THE BIN HOLD IS UNTOUCHED — the scope guard. The keep-your-holds
	// discipline exists because the finders exclude pending-reserved bins
	// owner-blind, so an order that dropped its bin hold to re-shop would
	// double-source. Slots were never that hazard; bins still are.
	if !holdsBin(t, db, order.ID, bin.ID) {
		t.Error("the order's BIN hold was released by a DESTINATION re-selection — it will now " +
			"re-shop for material it already had, and the finder cannot see the bin it just let go")
	}
	if after.BinID == nil || *after.BinID != bin.ID {
		t.Errorf("bin_id = %v, want the bin it already holds (%d)", after.BinID, bin.ID)
	}
}

// TestStoreRedirect_EveryLaneDug_ParksUnderTheExistingShape is the other end:
// with nowhere to divert to there is nothing to do, and the order must be left
// exactly as it was rather than half-re-aimed.
func TestStoreRedirect_EveryLaneDug_ParksUnderTheExistingShape(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	grp, laneA, laneB, slotsA, _, bp := twoLaneGroup(t, db, "SR-ALL")
	d := window4Dispatcher(t, db)

	bin := createTestBinAtNode(t, db, bp.Code, grp.ID, "SR-ALL-BIN")
	order := parkedStore(t, db, d, "sr-all", slotsA[1], bin.ID)

	digA := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "sr-all-dig-a" })
	digB := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "sr-all-dig-b" })
	if !d.laneLock.TryLock(laneA.ID, digA.ID) || !d.laneLock.TryLock(laneB.ID, digB.ID) {
		t.Fatal("both lanes must lock")
	}

	nowhereErr := d.ReserveStorageDropoff(order).Err
	testutil.MustNoErr(t, nowhereErr, "re-entry with nowhere to go")

	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.DeliveryNode != slotsA[1].Name {
		t.Errorf("delivery node moved to %s with every lane dug — there was nowhere better, so the "+
			"order should keep what it had and wait", after.DeliveryNode)
	}
	if !holdsSlot(t, db, order.ID, slotsA[1].ID) {
		t.Error("the order gave up its slot hold and got nothing for it — a half-re-aimed order is " +
			"worse off than one that simply waited")
	}
	if !holdsBin(t, db, order.ID, bin.ID) {
		t.Error("the bin hold was released")
	}

	// AND THE DIG COMPLETING RE-DRIVES IT. Releasing the lane is what the compound
	// terminal does; the next re-entry then finds its own lane usable again.
	d.laneLock.Unlock(laneA.ID, digA.ID)
	clearedErr := d.ReserveStorageDropoff(order).Err
	testutil.MustNoErr(t, clearedErr, "re-entry after the dig cleared")
	reloaded, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the clear")
	if reloaded.DeliveryNode != slotsA[1].Name {
		t.Errorf("delivery node = %s — its own lane is free again, so there is nothing to re-aim",
			reloaded.DeliveryNode)
	}
}

// TestStoreRedirect_LeavesAnOperatorsChoiceAlone is the narrowness assertion.
//
// An order stamped no-demand had its destination named by a human at a door — the
// bin move, the spot order. Re-aiming that is not a recalculation, it is Core
// overruling somebody who picked a node on purpose. Those wait, which is the
// honest outcome for a destination Core did not choose.
func TestStoreRedirect_LeavesAnOperatorsChoiceAlone(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	grp, laneA, _, slotsA, _, bp := twoLaneGroup(t, db, "SR-OP")
	d := window4Dispatcher(t, db)

	bin := createTestBinAtNode(t, db, bp.Code, grp.ID, "SR-OP-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sr-op"
		o.OrderType = protocol.OrderTypeMove
		o.DeliveryNode = slotsA[1].Name
		o.Status = protocol.StatusQueued
		o.OriginClass = protocol.OriginClassNoDemand // the bin-move door's stamp
	})
	testutil.MustNoErr(t, db.ReserveSlot(slotsA[1].ID, order.ID), "reserve the chosen slot")
	testdb.ReserveBin(t, db, order.ID, bin.ID)

	digger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "sr-op-digger" })
	if !d.laneLock.TryLock(laneA.ID, digger.ID) {
		t.Fatal("TryLock must succeed")
	}

	_ = d.ReserveStorageDropoff(order)

	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.DeliveryNode != slotsA[1].Name {
		t.Fatalf("delivery node = %s — a human named %s, and moving their bin somewhere else "+
			"without telling them is Core overruling the person at the door",
			after.DeliveryNode, slotsA[1].Name)
	}
}

// TestSettle_ResolvesASyntheticDestinationToAChild is the scanner gap, closed.
//
// Intake defers resolution when a group is full: it leaves the group name on the
// order and queues it, on the promise dispatch narrows it to a child. That
// promise was kept on the planning path and not on the scanner's, which has no
// resolver — GetNodeByDotName FINDS a group, because it is a real row, so there
// was no error to catch and the create went out naming a node no robot can drive
// to. HK: 26 such orders since June, none completed.
//
// MUTATION: drop the resolveSyntheticDropoff call from ReserveStorageDropoff.
// The row keeps the group name and both assertions below fire.
func TestSettle_ResolvesASyntheticDestinationToAChild(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	grp, _, _, _, _, bp := twoLaneGroup(t, db, "SR-SYN")
	d := window4Dispatcher(t, db)

	bin := createTestBinAtNode(t, db, bp.Code, grp.ID, "SR-SYN-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sr-syn"
		o.OrderType = protocol.OrderTypeMove
		o.PayloadCode = bp.Code
		o.DeliveryNode = grp.Name // the group name, as intake leaves it
		o.Status = protocol.StatusQueued
	})
	testdb.ReserveBin(t, db, order.ID, bin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp bin_id")
	order, _ = db.GetOrder(order.ID)

	sd := d.ReserveStorageDropoff(order)
	settled, rErr := sd.Node, sd.Err
	testutil.MustNoErr(t, rErr, "settle a group with free children")

	if settled == nil || settled.IsSynthetic {
		t.Fatalf("settle returned %v — a synthetic node names a set of positions, and a create "+
			"carrying one is rejected by the fleet (SEER 50001)", settled)
	}
	after, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")
	if after.DeliveryNode == grp.Name {
		t.Fatalf("the row still names the group %s — the scanner dispatches DeliveryNode verbatim, "+
			"so this is the create the fleet refuses", grp.Name)
	}
	if settled.Name != after.DeliveryNode {
		t.Errorf("settle returned %s but the row says %s", settled.Name, after.DeliveryNode)
	}
}

// TestSettle_AFullGroupIsAWaitNotAFailure — every child occupied. Intake's
// deferral is only safe if "not yet" stays re-tryable, so this must refuse in a
// way the caller parks on rather than one it terminalises.
func TestSettle_AFullGroupIsAWaitNotAFailure(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	grp, _, _, slotsA, slotsB, bp := twoLaneGroup(t, db, "SR-SYNFULL")
	d := window4Dispatcher(t, db)

	// Fill every slot in the group so no child can take a bin.
	for i, s := range append(append([]*nodes.Node{}, slotsA...), slotsB...) {
		createTestBinAtNode(t, db, bp.Code, s.ID, fmt.Sprintf("SR-SYNFULL-OCC-%d", i))
	}

	bin := createTestBinAtNode(t, db, bp.Code, grp.ID, "SR-SYNFULL-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sr-synfull"
		o.OrderType = protocol.OrderTypeMove
		o.PayloadCode = bp.Code
		o.DeliveryNode = grp.Name
		o.Status = protocol.StatusQueued
	})
	testdb.ReserveBin(t, db, order.ID, bin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, bin.ID), "stamp bin_id")
	order, _ = db.GetOrder(order.ID)

	sd := d.ReserveStorageDropoff(order)
	settled, rErr := sd.Node, sd.Err
	if rErr == nil {
		t.Fatalf("a full group settled to %v — proceeding is what sent group names to the fleet", settled)
	}
	if !IsSyntheticUnresolved(rErr) {
		t.Errorf("refusal did not classify as IsSyntheticUnresolved: %v\nThe scanner picks its queue "+
			"reason from this; unclassified it parks under slot contention, blaming the slot layer "+
			"for a resolution that never ran.", rErr)
	}
}
