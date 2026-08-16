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

// setupTwoLanesWithShuffles builds an NGRP with TWO buried lanes and `shuffles`
// group-level shuffle slots between them:
//
//	‹prefix›
//	├── LANE-A: A1 (depth 1, blocker) · A2 (depth 2, target)
//	├── LANE-B: B1 (depth 1, blocker) · B2 (depth 2, target)
//	└── SHUF-1 … SHUF-N (the only places a blocker can go)
//
// Every lane slot is occupied, so the SHUF nodes are the only free ones in the
// group — which is the point: the two digs are forced to want the same pool, and
// the count is what decides whether they compete for one slot or divert onto two.
// Both arms of the capacity gate are the same geometry with a different N.
func setupTwoLanesWithShuffles(t *testing.T, db *store.DB, prefix string, shuffles int) (grp *nodes.Node, laneA, laneB *nodes.Node, slotsA, slotsB []*nodes.Node, shufs []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(name string) (*nodes.Node, []*nodes.Node) {
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
	laneA, slotsA = mkLane(prefix + "-LANE-A")
	laneB, slotsB = mkLane(prefix + "-LANE-B")

	for i := 1; i <= shuffles; i++ {
		s := &nodes.Node{Name: fmt.Sprintf("%s-SHUF-%d", prefix, i), ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(s), "create shuffle slot")
		shufs = append(shufs, s)
	}

	grp, _ = db.GetNode(grp.ID)
	return grp, laneA, laneB, slotsA, slotsB, shufs, bp
}

// setupTwoLanesOneShuffle is the starvation geometry: the same two lanes with a
// pool of exactly one.
func setupTwoLanesOneShuffle(t *testing.T, db *store.DB) (grp *nodes.Node, laneA, laneB *nodes.Node, slotsA, slotsB []*nodes.Node, shuf *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grp, laneA, laneB, slotsA, slotsB, shufs, bp := setupTwoLanesWithShuffles(t, db, "1SHUF", 1)
	return grp, laneA, laneB, slotsA, slotsB, shufs[0], bp
}

// TestFindShuffleSlots_TwoDigsMustNotShareASlot pins the bug the houseserver sim
// exposed on 2026-07-13 (D83a).
//
// Shuffle slots are a GROUP-scoped shared resource, but the lane lock is keyed on
// the lane being dug -- so two digs in DIFFERENT lanes take different locks and
// both proceed. findShuffleSlots used to ask only "is this node empty RIGHT NOW",
// which is true of a slot that another dig already has a blocker in flight to. So
// both digs picked the same slot, the second blocker landed on the first, and
// EvictStaleGhostsTx threw the first bin to _TRANSIT. On the sim, lane 1 and lane
// 2 each unburied into SMN_008 + SMN_009 three seconds apart: two bins orphaned,
// and lane 1's restore compound left with nothing to restock.
//
// The dig legs carry delivery_node, so CheckDropoffCapacity -- the gate every
// other dropoff in the system passes -- already counted them. findShuffleSlots
// just never asked.
//
// ── RE-POINTED TO THE MOMENT THE CHOICE IS NOW MADE ───────────────────────
//
// Under the outbound dwell a dig leg is dispatched with NO destination and picks
// one at release, so the collision this test is about cannot happen at plan time
// any more — there is nothing to collide. The defect is unchanged and so is the
// mechanism that prevents it (findShuffleSlots + CheckDropoffCapacity); only the
// moment moved. So the fixture drives both digs to their release and asserts the
// same invariant there: exactly one leg inbound to the one slot, and the other
// WAITS holding its blocker rather than landing on top.
//
// ONE BEHAVIOUR GENUINELY CHANGED, and it is recorded rather than absorbed. Dig B
// now PLANS where it used to be refused: the plan-time count check asks whether
// the group has room right now, and dig A's plan no longer books anything, so on
// a pool of one both digs start. The wait moves from a `pending` row to a robot
// holding a bin — the residual SHAPE §7.8 names and accepts, bounded by the same
// congestion that would have blocked the leg anyway. What must NOT change is that
// two blockers never share a slot, which is what this now pins.
func TestFindShuffleSlots_TwoDigsMustNotShareASlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, laneA, laneB, slotsA, slotsB, shuf, bp := setupTwoLanesOneShuffle(t, db)

	// Both lanes buried: a blocker at the mouth, the target behind it.
	createTestBinAtNode(t, db, bp.Code, slotsA[0].ID, "BIN-A-BLK")
	targetA := createTestBinAtNode(t, db, bp.Code, slotsA[1].ID, "BIN-A-TGT")
	createTestBinAtNode(t, db, bp.Code, slotsB[0].ID, "BIN-B-BLK")
	targetB := createTestBinAtNode(t, db, bp.Code, slotsB[1].ID, "BIN-B-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Dig 1 (lane A) plans first and takes the only shuffle slot.
	orderA := &orders.Order{EdgeUUID: "dig-a", StationID: "line-1", OrderType: OrderTypeRetrieve, Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-2L"}
	testutil.MustNoErr(t, db.CreateOrder(orderA), "create order A")
	_, peA := d.planner.planBuriedReshuffle(orderA, &BuriedError{Bin: targetA, Slot: slotsA[1], LaneID: laneA.ID})
	if peA != nil {
		t.Fatalf("dig A should have planned (the shuffle slot was free): %s: %s", peA.Code, peA.Detail)
	}

	// A's first leg is dispatched and dwelling in lane A, holding its blocker with
	// no destination. Releasing it is where the slot is chosen and claimed.
	legsA := legsOf(t, db, orderA.ID)
	if len(legsA) != 2 {
		t.Fatalf("dig A planned %d leg(s), want an unbury and a retrieve", len(legsA))
	}
	if legsA[0].DeliveryNode != "" {
		t.Fatalf("dig A's unbury was born aimed at %q — the destination is chosen at release now, so a "+
			"plan-time binding means the dwell was bypassed", legsA[0].DeliveryNode)
	}
	releasedA := releaseDwell(t, d, db, legsA[0])
	if releasedA.DeliveryNode != shuf.Name {
		t.Fatalf("dig A's blocker went to %q, want the group's only free slot %s",
			releasedA.DeliveryNode, shuf.Name)
	}

	// Dig 2 (lane B) plans while dig A's blocker is in flight to SHUF-1. Its leg
	// must NOT take that slot. Nothing physically occupies SHUF-1 yet -- that is
	// exactly the trap.
	orderB := &orders.Order{EdgeUUID: "dig-b", StationID: "line-1", OrderType: OrderTypeRetrieve, Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-2L"}
	testutil.MustNoErr(t, db.CreateOrder(orderB), "create order B")
	_, peB := d.planner.planBuriedReshuffle(orderB, &BuriedError{Bin: targetB, Slot: slotsB[1], LaneID: laneB.ID})
	if peB != nil {
		// The count check is asked against the group as it stands, and A's leg has
		// booked the only slot by now, so this arm is the pre-dwell disposition
		// still holding — a refusal here must still be the transient one.
		if peB.Code != codeNoShuffleSlot {
			t.Fatalf("dig B refused with code %q (%s), want %q — a crowded group is not a broken lane; "+
				"it must WAIT for a slot (D18-Q4 wait-not-fail)", peB.Code, peB.Detail, codeNoShuffleSlot)
		}
		if !peB.Transient() {
			t.Errorf("dig B's refusal is not Transient() — it will terminally fail the order instead of retrying")
		}
	} else {
		// It planned. Then its leg must DWELL rather than take the booked slot.
		legsB := legsOf(t, db, orderB.ID)
		if len(legsB) == 0 {
			t.Fatal("dig B planned but wrote no legs")
		}
		d.EvaluateWaitLaneForStagedOrder(legsB[0].ID)
		heldB, err := db.GetOrder(legsB[0].ID)
		testutil.MustNoErr(t, err, "reload dig B's leg")
		if heldB.DeliveryNode != "" {
			t.Fatalf("dig B's leg was released to %q while dig A's blocker is already inbound to %s — "+
				"the second blocker lands on the first and ApplyArrival evicts the incumbent to _TRANSIT "+
				"(the sim's SMN_008/SMN_009 orphaning, D83a)", heldB.DeliveryNode, shuf.Name)
		}
		if QueueCause(heldB.QueueCause) != CauseNoShuffleSlot {
			t.Errorf("dig B's dwelling leg carries cause %q, want %q — a robot standing in a lane holding "+
				"a bin with nowhere to put it is the one wait that must say so on the row",
				heldB.QueueCause, CauseNoShuffleSlot)
		}
		if protocol.IsTerminal(heldB.Status) {
			t.Errorf("dig B's leg is %s — no free shuffle slot is congestion and WAITS, it does not "+
				"terminate the demand", heldB.Status)
		}
	}

	// THE INVARIANT, WHICHEVER WAY DIG B WENT: exactly one leg is inbound to SHUF-1.
	var inbound int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM orders WHERE delivery_node = $1 AND parent_order_id IS NOT NULL`, shuf.Name,
	).Scan(&inbound), "count legs inbound to the shuffle slot")
	if inbound != 1 {
		t.Fatalf("%d dig legs are inbound to %s, want exactly 1.\n"+
			"Two digs in different lanes booked the same shuffle slot: the second blocker will land on "+
			"the first, and ApplyArrival evicts the incumbent to _TRANSIT. This is the sim's SMN_008/"+
			"SMN_009 orphaning (D83a).", inbound, shuf.Name)
	}
	_ = grp
}

// TestFindShuffleSlots_TwoDigsDivertOntoDifferentSlots is the gate's OTHER arm,
// and the one the D83a fix was actually for.
//
// The test above proves the gate REFUSES when the pool is one deep. That is the
// starvation shape, and on its own it is compatible with a gate that has simply
// become too strict — a `return false` in shuffleSlotFree passes it. What the fix
// is supposed to buy is the opposite behaviour on a group with room: the second
// dig sees the first one's slot as spoken for and TAKES THE OTHER ONE, rather than
// waiting for it or landing on top of it. Neither of those two facts implies the
// other, and only one of them was pinned.
//
// So this asserts the diversion and then follows both digs to the end. "Both
// complete" is the half a refusal test can never reach, and it is where a gate
// that diverts correctly and then leaks — a slot never released, a lock held past
// the compound — would show up.
//
// MUTATION (verified): revert shuffleSlotFree (reshuffle.go) to the pre-D83a body,
// `cnt, _ := db.CountBinsByNode(n.ID); return cnt == 0`. Both digs then pick the
// same empty slot and the "different drop-offs" assertion fires — the second
// blocker lands on the first, which is the SMN_008/SMN_009 orphaning the sibling
// test above describes.
func TestFindShuffleSlots_TwoDigsDivertOntoDifferentSlots(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneA, laneB, slotsA, slotsB, shufs, bp := setupTwoLanesWithShuffles(t, db, "2SHUF", 2)

	blockerA := testdb.CreateBinAtNode(t, db, bp.Code, slotsA[0].ID, "2SHUF-A-BLK")
	targetA := testdb.CreateBinAtNode(t, db, bp.Code, slotsA[1].ID, "2SHUF-A-TGT")
	blockerB := testdb.CreateBinAtNode(t, db, bp.Code, slotsB[0].ID, "2SHUF-B-BLK")
	targetB := testdb.CreateBinAtNode(t, db, bp.Code, slotsB[1].ID, "2SHUF-B-TGT")
	lineA := lineNode(t, db, "2SHUF-LINE-A")
	lineB := lineNode(t, db, "2SHUF-LINE-B")

	mkDemand := func(uuid, delivery string) *orders.Order {
		return testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = uuid
			o.OrderType = OrderTypeRetrieve
			o.PayloadCode = bp.Code
			o.DeliveryNode = delivery
			o.Status = protocol.StatusPending
		})
	}
	demandA := mkDemand("2shuf-a", lineA.Name)
	demandB := mkDemand("2shuf-b", lineB.Name)

	if _, pe := d.planner.planBuriedReshuffle(demandA, &BuriedError{Bin: targetA, Slot: slotsA[1], LaneID: laneA.ID}); pe != nil {
		t.Fatalf("dig A must plan against an empty pool of two, got %s: %s", pe.Code, pe.Detail)
	}
	// Dig B plans while dig A's blocker is IN FLIGHT — nothing physically occupies
	// A's slot yet, which is the trap the gate exists for. With room in the group
	// the answer is not "wait", it is "take the other one".
	if _, pe := d.planner.planBuriedReshuffle(demandB, &BuriedError{Bin: targetB, Slot: slotsB[1], LaneID: laneB.ID}); pe != nil {
		t.Fatalf("dig B was refused (%s: %s) with a free slot standing in the group. A gate that cannot "+
			"tell 'spoken for' from 'no room' turns every second dig into a wait", pe.Code, pe.Detail)
	}

	legsA, legsB := legsOf(t, db, demandA.ID), legsOf(t, db, demandB.ID)
	if len(legsA) != 2 || len(legsB) != 2 {
		t.Fatalf("legs = %d / %d, want an unbury and a retrieve each", len(legsA), len(legsB))
	}

	// ── TWO DWELLERS RELEASED IN THE SAME BREATH ──────────────────────────────
	//
	// This is the diversion, moved to the moment it now happens. Both legs are
	// standing in their own lanes holding blockers with no destination; releasing
	// them back to back is the concurrency this test always described (dig B
	// choosing while dig A's blocker is in flight and nothing physically occupies
	// A's slot) and it is now literal rather than simulated by planning order.
	//
	// They must come out on DIFFERENT slots. What makes that true is that the
	// resolver claims the slot at the moment it selects, so the second chooser
	// cannot see the first's as free — the two are not serialized by anything
	// else, being in different lanes and therefore under different mutexes.
	releasedA := releaseDwell(t, d, db, legsA[0])
	releasedB := releaseDwell(t, d, db, legsB[0])
	if releasedA.DeliveryNode == releasedB.DeliveryNode {
		t.Fatalf("both digs were released onto %s — the capacity gate did not divert them, so the second "+
			"blocker lands on the first and ApplyArrival evicts the incumbent to _TRANSIT",
			releasedA.DeliveryNode)
	}
	legsA, legsB = legsOf(t, db, demandA.ID), legsOf(t, db, demandB.ID)
	for _, s := range shufs {
		n, err := db.CountInFlightOrdersByDeliveryNodeExcluding(s.Name, 0)
		testutil.MustNoErr(t, err, "inbound count for "+s.Name)
		if n != 1 {
			t.Errorf("%d orders inbound to %s, want exactly 1 — with two digs and two slots the pool "+
				"should be exactly consumed", n, s.Name)
		}
	}
	for _, leg := range []*orders.Order{legsA[0], legsB[0]} {
		if leg.VendorOrderID == "" {
			t.Fatalf("unbury leg %d never went out (queue_cause %q) — the two digs are in different "+
				"lanes and neither is inside the other's", leg.ID, leg.QueueCause)
		}
	}

	// ── AND BOTH RUN OUT ──────────────────────────────────────────────────────
	for _, demand := range []*orders.Order{demandA, demandB} {
		legs := legsOf(t, db, demand.ID)
		landLeg(t, d, db, legs[0])
		testutil.MustNoErr(t, d.AdvanceCompoundOrder(demand.ID), "re-drive onto the retrieve")
		legs = legsOf(t, db, demand.ID)
		if legs[1].VendorOrderID == "" {
			t.Fatalf("demand %d's retrieve never went out (queue_cause %q) — its lane is clear and its "+
				"blocker has gone", demand.ID, legs[1].QueueCause)
		}
		landLeg(t, d, db, legs[1])
		testutil.MustNoErr(t, d.AdvanceCompoundOrder(demand.ID), "close the compound")
	}

	for _, demand := range []*orders.Order{demandA, demandB} {
		done, err := db.GetOrder(demand.ID)
		testutil.MustNoErr(t, err, "reload demand")
		if done.Status != protocol.StatusConfirmed {
			t.Errorf("demand %d is %q, want confirmed — slot competition costs time and must cost "+
				"nothing else", demand.ID, done.Status)
		}
	}

	// The blockers really are in two different places, physically. The order rows
	// agreeing is the plan; the bins agreeing is the outcome.
	landedA, err := db.GetBin(blockerA.ID)
	testutil.MustNoErr(t, err, "reload blocker A")
	landedB, err := db.GetBin(blockerB.ID)
	testutil.MustNoErr(t, err, "reload blocker B")
	if landedA.NodeID == nil || landedB.NodeID == nil || *landedA.NodeID == *landedB.NodeID {
		t.Errorf("the two blockers ended at nodes %v and %v — one bin is on top of the other",
			landedA.NodeID, landedB.NodeID)
	}
	for _, want := range []struct {
		bin  int64
		line *nodes.Node
	}{{targetA.ID, lineA}, {targetB.ID, lineB}} {
		delivered, gErr := db.GetBin(want.bin)
		testutil.MustNoErr(t, gErr, "reload a retrieved bin")
		if delivered.NodeID == nil || *delivered.NodeID != want.line.ID {
			t.Errorf("bin %d is at node %v, want the line %d — the retrieve is what the dig was FOR",
				want.bin, delivered.NodeID, want.line.ID)
		}
	}

	// THE LEDGER IS CLEAN — both locks lifted, no occupancy left behind.
	for _, lane := range []*nodes.Node{laneA, laneB} {
		if d.laneLock.IsLocked(lane.ID) {
			t.Errorf("lane %s is still locked after its dig completed", lane.Name)
		}
		if occ, _ := reservations.OccupantsOf(db.DB, lane.ID); len(occ) != 0 {
			t.Errorf("lane %s still has occupants %v", lane.Name, occ)
		}
	}
}
