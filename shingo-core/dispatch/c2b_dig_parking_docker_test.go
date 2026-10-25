//go:build docker

package dispatch

import (
	"errors"
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

// c2b_dig_parking_docker_test.go — a dig pencils its parking (RULINGS-C2b-dig-parking-2026-10-09).
//
// The jam of 2026-10-09 (c2b-demo-visible-923254b1-20m): two digs into a nearly
// full SYN_MARKET, 959 clearing Lane_04 and 963 clearing Lane_13, 90 ms apart.
// Each dig's fit check COUNTED the group's free slots and held none of them; an
// ordinary store then took the slot one leg had picked (SMN_022), and each dig's
// lifted leg (965, 966) needed a slot that existed only inside the other dig's
// locked lane. Seventeen orders stranded with no releaser. Main's two-day jam of
// 2026-10-05 is the same shape with one dig.
//
// The ruling: at a dig's birth each blocker's leg takes a pending reservation on
// one parking slot, chosen by the shuffle walk; a dig that cannot reserve a slot
// per blocker does not start. Every chooser then sees the room on paper
// (SlotSpokenForByStrangerSQL), and a leg places into its own slot or a better
// free one, moving its reservation.

// parkingGroup builds a marked group of digs and parking:
//
//	PK-GRP
//	├── PK-LA   d1 blocker · d2 target       <- dig A
//	├── PK-LB   d1 blocker · d2 target       <- dig B (when twoDigs)
//	├── PK-LS   parkLane free slots          <- parking in a lane
//	└── PK-P1..Pn  parks direct children     <- parking beside the lanes
//
// It returns the group, the two dug lanes and their slots, the parking lane's
// slots and the direct parks.
type parkingFixture struct {
	grp, la, lb, ls *nodes.Node
	aSlots, bSlots  []*nodes.Node
	lsSlots, parks  []*nodes.Node
	bp              *payloads.Payload
}

func parkingGroup(t *testing.T, db *store.DB, prefix string, twoDigs bool, laneDepthA, parkLane, parks int) parkingFixture {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "LANE type")
	f := parkingFixture{bp: &payloads.Payload{Code: prefix + "-P"}}
	testutil.MustNoErr(t, db.CreatePayload(f.bp), "create payload")
	f.grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(f.grp), "create group")
	testdb.MarkGroup(t, db, f.grp.ID)
	mkLane := func(name string, depth int) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &f.grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		var slots []*nodes.Node
		for i := 1; i <= depth; i++ {
			at := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), ParentID: &lane.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, err := db.GetNode(lane.ID)
		testutil.MustNoErr(t, err, "reload "+name)
		return reloaded, slots
	}
	f.la, f.aSlots = mkLane(prefix+"-LA", laneDepthA)
	for i, s := range f.aSlots {
		label := fmt.Sprintf("%s-A-BLK%d", prefix, i+1)
		if i == len(f.aSlots)-1 {
			label = prefix + "-A-TGT"
		}
		createTestBinAtNode(t, db, f.bp.Code, s.ID, label)
	}
	if twoDigs {
		f.lb, f.bSlots = mkLane(prefix+"-LB", 2)
		createTestBinAtNode(t, db, f.bp.Code, f.bSlots[0].ID, prefix+"-B-BLK1")
		createTestBinAtNode(t, db, f.bp.Code, f.bSlots[1].ID, prefix+"-B-TGT")
	}
	if parkLane > 0 {
		f.ls, f.lsSlots = mkLane(prefix+"-LS", parkLane)
	}
	for i := 1; i <= parks; i++ {
		p := &nodes.Node{Name: fmt.Sprintf("%s-P%d", prefix, i), ParentID: &f.grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(p), "create park")
		f.parks = append(f.parks, p)
	}
	return f
}

// retrieveFor is a plain retrieve demand for the payload, delivering to a line.
func retrieveFor(t *testing.T, db *store.DB, f parkingFixture, uuid string) *orders.Order {
	t.Helper()
	return testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = f.bp.Code
		o.DeliveryNode = lineNode(t, db, uuid+"-LINE").Name
		o.Status = protocol.StatusPending
	})
}

// pencilOf is the slot node a leg holds a PENDING slot reservation on, or 0.
func pencilOf(t *testing.T, db *store.DB, legID int64) int64 {
	t.Helper()
	rows, err := reservations.ListByOrder(db.DB, legID)
	testutil.MustNoErr(t, err, "list the leg's reservations")
	for _, r := range rows {
		if r.Kind == reservations.KindSlot && r.State == reservations.StatePending {
			return r.NodeID
		}
	}
	return 0
}

// unburyLegs are a dig's legs that lift a blocker (no delivery yet), in sequence.
func unburyLegs(t *testing.T, db *store.DB, parentID int64) []*orders.Order {
	t.Helper()
	var out []*orders.Order
	for _, l := range legsOf(t, db, parentID) {
		if l.DeliveryNode == "" {
			out = append(out, l)
		}
	}
	return out
}

// TestC2bPin_TwoDigsRoomForOne_TheSecondParks: a group with room for one blocker
// and two digs that each need one. The first holds its parking on paper; the
// second does not start and parks its demand under no-shuffle-slot, holding
// nothing; nothing deadlocks.
func TestC2bPin_TwoDigsRoomForOne_TheSecondParks(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK1", true, 2, 0, 1)

	a := retrieveFor(t, db, f, "dpk1-a")
	aLegs := planDigFor(t, db, d, f.bp, f.la, f.aSlots[1], a)
	blk := unburyLegs(t, db, a.ID)
	if len(aLegs) == 0 || len(blk) != 1 {
		t.Fatalf("dig A: %d leg(s), %d unbury; want its blocker's leg", len(aLegs), len(blk))
	}
	if got := pencilOf(t, db, blk[0].ID); got != f.parks[0].ID {
		t.Fatalf("dig A's leg holds parking %d on paper, want %s (%d): a dig pencils its parking at birth",
			got, f.parks[0].Name, f.parks[0].ID)
	}

	b := retrieveFor(t, db, f, "dpk1-b")
	_, pe := d.planner.planBuriedReshuffle(b, &BuriedError{
		Bin: mustBinAt(t, db, f.bSlots[1]), Slot: f.bSlots[1], LaneID: f.lb.ID})
	if pe == nil || pe.Code != codeNoShuffleSlot {
		t.Fatalf("dig B with no room left: %+v, want the demand parked under no-shuffle-slot", pe)
	}
	if legs := legsOf(t, db, b.ID); len(legs) != 0 {
		t.Fatalf("dig B started %d leg(s) with no parking for them", len(legs))
	}
}

// TestC2bPin_AStoreNeverTakesADigsParking: SMN_022 in the jam. A dig's parking
// in a lane is on paper from its birth, so the store selector never offers it.
func TestC2bPin_AStoreNeverTakesADigsParking(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK2", false, 2, 1, 0)

	a := retrieveFor(t, db, f, "dpk2-a")
	planDigFor(t, db, d, f.bp, f.la, f.aSlots[1], a)
	blk := unburyLegs(t, db, a.ID)
	if len(blk) != 1 || pencilOf(t, db, blk[0].ID) != f.lsSlots[0].ID {
		t.Fatalf("dig A's leg must hold %s on paper", f.lsSlots[0].Name)
	}

	store := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status = protocol.OrderTypeStore, protocol.StatusQueued
	})
	if got, err := db.FindStoreSlotInLaneExcluding(f.ls.ID, store.ID); err == nil {
		t.Fatalf("the store selector offered %s, a dig's pencilled parking", got.Name)
	}
	if err := claimStoreSlot(db, store, f.lsSlots[0]); !errors.Is(err, ErrSlotContended) {
		t.Fatalf("a store's claim of a dig's parking: %v, want ErrSlotContended", err)
	}
}

// TestC2bPin_ALegsPencilFollowsItToABetterFreeSlot: the leg places into its own
// pencilled slot or a better one that is free, and the reservation moves with
// it: it leaves no paper behind on the slot it did not use.
func TestC2bPin_ALegsPencilFollowsItToABetterFreeSlot(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK3", false, 2, 1, 0)

	a := retrieveFor(t, db, f, "dpk3-a")
	planDigFor(t, db, d, f.bp, f.la, f.aSlots[1], a)
	blk := unburyLegs(t, db, a.ID)
	if len(blk) != 1 || pencilOf(t, db, blk[0].ID) != f.lsSlots[0].ID {
		t.Fatalf("dig A's leg must hold %s on paper", f.lsSlots[0].Name)
	}

	// A better slot frees after the birth: a direct child of the group, which
	// the walk asks before any lane slot.
	better := &nodes.Node{Name: "DPK3-BETTER", ParentID: &f.grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(better), "create the better slot")

	arriveAtDwell(t, db, d, blk[0])
	leg, err := db.GetOrder(blk[0].ID)
	testutil.MustNoErr(t, err, "reload the leg")
	if leg.DeliveryNode != better.Name {
		t.Fatalf("the leg was released to %q (cause %q), want the better free slot %s",
			leg.DeliveryNode, leg.QueueCause, better.Name)
	}
	if held, err := db.SlotSpokenForByStranger(f.lsSlots[0].ID, 0); err != nil || held {
		t.Fatalf("%s still carries paper after the leg moved to %s (held %v, err %v)",
			f.lsSlots[0].Name, better.Name, held, err)
	}
}

// TestC2bPin_ATornDownDigLeavesNoPaper: cancelling the dig's demand ends its
// legs, and their parking reservations end with them.
func TestC2bPin_ATornDownDigLeavesNoPaper(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK4", false, 3, 0, 2)

	a := retrieveFor(t, db, f, "dpk4-a")
	planDigFor(t, db, d, f.bp, f.la, f.aSlots[2], a)
	blk := unburyLegs(t, db, a.ID)
	if len(blk) != 2 || pencilOf(t, db, blk[0].ID) == 0 || pencilOf(t, db, blk[1].ID) == 0 {
		t.Fatalf("dig A's %d unbury leg(s) must each hold a parking slot on paper", len(blk))
	}

	parent, err := db.GetOrder(a.ID)
	testutil.MustNoErr(t, err, "reload the demand")
	d.CancelOrderWithCascade(parent, "", "test: tear the dig down", CancelCause{})
	for _, p := range f.parks {
		if held, err := db.SlotSpokenForByStranger(p.ID, 0); err != nil || held {
			t.Fatalf("%s still carries paper after the dig was torn down (held %v, err %v)", p.Name, held, err)
		}
	}
}

// TestC2bPin_AWaitingDigLegCarriesACause: leg 968 in the jam, pending behind its
// sibling with no cause on its row. Every leg after the first waits its turn
// under a cause from its birth.
func TestC2bPin_AWaitingDigLegCarriesACause(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK5", false, 3, 0, 2)

	a := retrieveFor(t, db, f, "dpk5-a")
	planDigFor(t, db, d, f.bp, f.la, f.aSlots[2], a)
	for _, l := range legsOf(t, db, a.ID) {
		if l.Status == protocol.StatusPending && l.QueueCause == "" {
			t.Errorf("leg %d (seq %d) waits pending with no cause on its row", l.ID, l.Sequence)
		}
	}
}

// TestC2bPin_TheJamOf959And963: two digs into a group with two parking slots,
// each needing one. Both start, each holding its own slot on paper. An ordinary
// store then finds no slot to take (it took SMN_022 in the jam). Each leg lifts
// and places into its own parking; neither ends under no-shuffle-slot.
func TestC2bPin_TheJamOf959And963(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK6", true, 2, 0, 2)

	a := retrieveFor(t, db, f, "dpk6-a")
	planDigFor(t, db, d, f.bp, f.la, f.aSlots[1], a)
	b := retrieveFor(t, db, f, "dpk6-b")
	planDigFor(t, db, d, f.bp, f.lb, f.bSlots[1], b)
	aLeg, bLeg := unburyLegs(t, db, a.ID), unburyLegs(t, db, b.ID)
	if len(aLeg) != 1 || len(bLeg) != 1 {
		t.Fatalf("both digs must start: %d and %d unbury leg(s)", len(aLeg), len(bLeg))
	}

	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType, o.Status = protocol.OrderTypeStore, protocol.StatusQueued
	})
	for _, p := range f.parks {
		if err := claimStoreSlot(db, stranger, p); err == nil {
			t.Fatalf("an ordinary store took %s, parking a running dig needs", p.Name)
		}
	}

	for _, leg := range []*orders.Order{aLeg[0], bLeg[0]} {
		arriveAtDwell(t, db, d, leg)
		after, err := db.GetOrder(leg.ID)
		testutil.MustNoErr(t, err, "reload the leg")
		if after.DeliveryNode == "" {
			t.Fatalf("leg %d is still dwelling with its blocker (cause %q): the jam", leg.ID, after.QueueCause)
		}
	}
}

// TestC2bPin_APlanNeverBlocksACommittedLegsParking is the second jam of
// 2026-10-09 (c2b-demo-visible-8a2b01e4-20m: 782, 801, 803). Dig A's leg
// pencils the mouth of a parking lane at A's birth. Afterwards a never-sent
// demand, which has dug before, soft-holds the bin just behind that mouth. The
// shuffle search's burial guard then read the demand's paper as "coming for that
// bin" and refused the committed leg, standing with a lifted bin, its own
// pencilled slot: no-shuffle-slot, every pass, while the demand waited for room
// the stuck dig held. A plan never blocks a committed robot, in this chooser as
// in the store selector: the leg places, and the demand re-plans or digs.
//
// MUTATION: read ComingForBinSQL whatever the claimant in
// SlotsBlockedByHardClaims; the leg is refused its own pencil.
func TestC2bPin_APlanNeverBlocksACommittedLegsParking(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	f := parkingGroup(t, db, "DPK7", false, 2, 3, 0)
	behind := createTestBinAtNode(t, db, f.bp.Code, f.lsSlots[1].ID, "DPK7-BEHIND")
	createTestBinAtNode(t, db, f.bp.Code, f.lsSlots[2].ID, "DPK7-BACK")

	a := retrieveFor(t, db, f, "dpk7-a")
	planDigFor(t, db, d, f.bp, f.la, f.aSlots[1], a)
	blk := unburyLegs(t, db, a.ID)
	if len(blk) != 1 || pencilOf(t, db, blk[0].ID) != f.lsSlots[0].ID {
		t.Fatalf("dig A's leg must pencil %s, the one free slot", f.lsSlots[0].Name)
	}

	// The never-sent demand that dug before (it has a dig leg), soft-holding the
	// bin behind the pencilled mouth: 782.
	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.Status = "dpk7-demand", OrderTypeComplex, protocol.StatusQueued
	})
	testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.Status, o.ParentOrderID = "dpk7-demand-dug", OrderTypeMove, protocol.StatusPending, &demand.ID
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, demand.ID, demand.ID, behind.ID, "reserveForDispatch"),
		"the demand soft-holds the bin behind the pencil")

	arriveAtDwell(t, db, d, blk[0])
	leg, err := db.GetOrder(blk[0].ID)
	testutil.MustNoErr(t, err, "reload the leg")
	if leg.DeliveryNode != f.lsSlots[0].Name {
		t.Fatalf("the committed leg was released to %q (cause %q), want its own pencilled slot %s: a "+
			"never-sent demand's paper refused it", leg.DeliveryNode, leg.QueueCause, f.lsSlots[0].Name)
	}
}
