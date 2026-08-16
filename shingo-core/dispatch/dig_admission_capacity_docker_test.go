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

// dig_admission_capacity_docker_test.go — the usable-capacity claim (§R.75/§R.76).
//
// THE CLAIM UNDER TEST: a group admits only as many digs as it can feed. Every
// running dig owes one slot's worth from its start until it binds a destination
// for its blocker, and a dig that does not fit on top of what is already owed does
// not start — it waits, named, holding nothing.
//
// WHAT IT IS FOR, measured 2026-08-13 on the lane-stress rig: three digs in one
// group each counted the same six mouth slots, all three were admitted, and each
// one's slots turned out to be the others' only source of space. Three digs, three
// loaded robots, frozen byte-identical across two reads 5m39s apart. The group
// could afford one dig and was running three, and nothing in the system had ever
// asked whether it could afford them.
//
// SERIALIZATION UNDER FAMINE IS THE RULED ANSWER, not a regression. A group with
// room for one dig runs one dig at a time. Slow beats frozen.

// setupCapacityGroup builds a group with TWO diggable lanes and a single free
// slot between them:
//
//	CAP-GRP
//	├── CAP-A    depth 2: S1 (blocker) · S2 (target)
//	├── CAP-B    depth 2: S1 (blocker) · S2 (target)
//	└── CAP-PARK a direct child of the group — THE ONLY usable slot
//
// The arithmetic is the fixture: each dig needs exactly one slot, and there is
// exactly one. Whichever dig gets there first can afford itself; the second cannot
// afford itself on top of the first.
func setupCapacityGroup(t *testing.T, db *store.DB, prefix string) (grp, laneA, laneB, park *nodes.Node,
	aSlots, bSlots []*nodes.Node, bp *payloads.Payload) {
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
		for i := 1; i <= 2; i++ {
			at := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), ParentID: &lane.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	laneA, aSlots = mkLane(prefix + "-A")
	laneB, bSlots = mkLane(prefix + "-B")

	park = &nodes.Node{Name: prefix + "-PARK", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(park), "create the one free slot")

	grp, _ = db.GetNode(grp.ID)
	return grp, laneA, laneB, park, aSlots, bSlots, bp
}

// ── A NOTE ON WHAT COUNTS AS A DIG IN THESE FIXTURES ──────────────────────
//
// Every dig here is REAL: planBuriedReshuffle mints the compound, takes the lane
// lock and creates the unbury legs, exactly as the plant does.
//
// That is not fussiness, it is the only way these tests can work. The older
// fixtures in this package fake a rival dig as a bare order holding the lane's
// mouth row and nothing else — no compound, no legs, no plan — which is a state
// the plant cannot produce. It was adequate while the only question anyone asked
// about a dig was "does it hold this lane", because right of way reads the mouth
// row and cannot tell the difference. It is not adequate now: a dig also OWES
// capacity, owing is derived from its legs, and a legless dig owes nothing. A
// fixture like that would read as a rival excavation while counting as zero, and
// this whole file would pass without exercising anything.

// TestDigAdmission_SecondDigWaitsWhenTheGroupCannotAffordIt is the arm itself.
//
// Dig A is admitted and holds one slot's worth: its blocker is lifted-or-pending
// with no destination bound. Dig B then asks for the same single slot. The old
// count check said yes — one slot exists, B needs one — and that answer is what
// the famine is made of, because the slot A is owed and the slot B is counting are
// the same slot.
//
// MUTATION (verified): drop `+outstanding` from planUnbury's count check. B is
// admitted, both digs run against one slot, and the fixture becomes the rig
// specimen in miniature.
func TestDigAdmission_SecondDigWaitsWhenTheGroupCannotAffordIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneA, laneB, park, aSlots, bSlots, bp := setupCapacityGroup(t, db, "CAPWAIT")

	createTestBinAtNode(t, db, bp.Code, aSlots[0].ID, "CAPWAIT-A-BLK")
	createTestBinAtNode(t, db, bp.Code, aSlots[1].ID, "CAPWAIT-A-TGT")
	createTestBinAtNode(t, db, bp.Code, bSlots[0].ID, "CAPWAIT-B-BLK")
	createTestBinAtNode(t, db, bp.Code, bSlots[1].ID, "CAPWAIT-B-TGT")

	// DIG A: admitted, because at this moment the group owes nothing.
	demandA := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "capwait-a"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "CAPWAIT-LINE-A").Name
		o.Status = protocol.StatusPending
	})
	if _, pe := d.planner.planBuriedReshuffle(demandA, &BuriedError{
		Bin: mustBinAt(t, db, aSlots[1]), Slot: aSlots[1], LaneID: laneA.ID}); pe != nil {
		t.Fatalf("dig A must be admitted — the group owes nothing yet and %s is free: %s: %s",
			park.Name, pe.Code, pe.Detail)
	}

	// DIG B: the same single slot, now spoken for.
	demandB := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "capwait-b"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "CAPWAIT-LINE-B").Name
		o.Status = protocol.StatusPending
	})
	_, pe := d.planner.planBuriedReshuffle(demandB, &BuriedError{
		Bin: mustBinAt(t, db, bSlots[1]), Slot: bSlots[1], LaneID: laneB.ID})
	if pe == nil {
		t.Fatalf("dig B was ADMITTED. %s is the only usable slot in the group and dig A is already "+
			"owed it — admitting both is the famine: two excavations, one slot, and each one's "+
			"blocker is the other's only source of space", park.Name)
	}

	// AND THE WAIT IS NAMED (law 8): the demand carries the cause, not a fault.
	parked, err := db.GetOrder(demandB.ID)
	testutil.MustNoErr(t, err, "reload the refused demand")
	if QueueCause(parked.QueueCause) != CauseGroupRoomClaimed {
		t.Errorf("dig B parked under cause %q, want %q. The group is NOT full — %s is empty and "+
			"reachable — it is SPOKEN FOR, and those two clear on different events",
			parked.QueueCause, CauseGroupRoomClaimed, park.Name)
	}
	if protocol.IsTerminal(parked.Status) {
		t.Errorf("dig B's demand went to %s. Not affording a dig is congestion, and congestion "+
			"waits (law 1) — it must never terminate a demand", parked.Status)
	}
	_ = laneB
}

// ── ARM 3: RESOLVE WHAT IS ALREADY DUG BEFORE DIGGING MORE (§R.76) ────────
//
// A different rule from the affordability claim above, on the same gate, and the
// fixture has to keep them apart or it proves nothing. Here the group has room to
// spare — a SECOND parking slot, deliberately — so the only thing that can refuse
// the second dig is the ordering rule.
//
// IT APPLIES TO SERVICE DIGS ONLY, and that falls out of the two-shape rule
// rather than being a special case. A plain buried retrieve re-parents the demand,
// so its fetch is one of its own legs: the collection is guaranteed and the lane
// is held by legStillNeedsLane until it happens. Only a SERVICE dig — raised for
// somebody else, owning no retrieve — can finish with a bin uncovered and nobody
// committed to taking it. So the dig doing the holding below is built the way the
// plant builds one.

// serviceDigHolding runs a real service dig on lane to completion and leaves it
// holding for its uncollected target: plan, mint, lock, create, land the blocker,
// re-drive. Returns the holding parent.
func serviceDigHolding(t *testing.T, db *store.DB, d *Dispatcher, grp, lane, target *nodes.Node,
	bp *payloads.Payload, uuid string) *orders.Order {
	t.Helper()
	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid + "-req"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, uuid+"-LINE").Name
		o.Status = protocol.StatusPending
	})
	plan, err := PlanLaneMouthClear(db, target, lane, grp.ID, reservations.Anyone)
	testutil.MustNoErr(t, err, "plan the lane clear for "+lane.Name)
	parent, err := d.createServiceDigParent(lane, target, requester, plan)
	testutil.MustNoErr(t, err, "mint the service dig parent")
	if !d.laneLock.TryLock(lane.ID, parent.ID) {
		t.Fatalf("the service dig could not take %s", lane.Name)
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "create the service dig")

	for _, leg := range legsOf(t, db, parent.ID) {
		landLeg(t, d, db, leg)
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "re-drive the dig past its last leg")

	held, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the holding dig")
	if protocol.IsTerminal(held.Status) || !d.laneLock.IsLocked(lane.ID) {
		t.Fatalf("the service dig finished instead of holding (status %q, locked %t) — arm 2 is not "+
			"doing its job and this fixture cannot test arm 3", held.Status, d.laneLock.IsLocked(lane.ID))
	}
	return held
}

// TestDigAdmission_NoNewDigWhileTheGroupOwesACollection is arm 3.
//
// Dig A has finished excavating lane A and is holding it for the bin it uncovered.
// The group has a free slot — arm 1 is satisfied — and dig B is refused anyway,
// because resolving that collection RETURNS room (the bin leaves, the lane
// releases, the slot A's blocker took stays taken but the lane comes back) while
// starting B SPENDS it. Under famine, doing the second while the first is
// outstanding is how a group talks itself into having nothing left.
//
// MUTATION (verified): delete the ListDigsHoldingTargetsInGroup arm from
// planUnbury. B is admitted while a bin sits uncollected in an open lane, which is
// the ordering §R.76 rules against.
func TestDigAdmission_NoNewDigWhileTheGroupOwesACollection(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, laneA, laneB, park, aSlots, bSlots, bp := setupCapacityGroup(t, db, "OWES")

	// A SECOND free slot, so the group can afford dig B outright. Without it this
	// test would pass on the affordability claim and say nothing about ordering.
	spare := &nodes.Node{Name: "OWES-SPARE", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spare), "create the spare slot")

	createTestBinAtNode(t, db, bp.Code, aSlots[0].ID, "OWES-A-BLK")
	createTestBinAtNode(t, db, bp.Code, aSlots[1].ID, "OWES-A-TGT")
	createTestBinAtNode(t, db, bp.Code, bSlots[0].ID, "OWES-B-BLK")
	createTestBinAtNode(t, db, bp.Code, bSlots[1].ID, "OWES-B-TGT")

	holding := serviceDigHolding(t, db, d, grp, laneA, aSlots[1], bp, "owes-a")

	demandB := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "owes-b"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "OWES-LINE-B").Name
		o.Status = protocol.StatusPending
	})
	_, pe := d.planner.planBuriedReshuffle(demandB, &BuriedError{
		Bin: mustBinAt(t, db, bSlots[1]), Slot: bSlots[1], LaneID: laneB.ID})
	if pe == nil {
		t.Fatalf("dig B was ADMITTED while reshuffle %d holds %s for the uncollected bin at %s. The "+
			"group has room (%s and %s), so this is not a shortage — it is the ordering rule: "+
			"finishing that collection gives the group a lane back, starting another excavation "+
			"takes one", holding.ID, laneA.Name, aSlots[1].Name, park.Name, spare.Name)
	}
	parked, err := db.GetOrder(demandB.ID)
	testutil.MustNoErr(t, err, "reload the refused demand")
	if QueueCause(parked.QueueCause) != CauseGroupOwesCollection {
		t.Errorf("dig B parked under cause %q, want %q. This is not a full group and not spoken-for "+
			"room — it is a collection outstanding, and it clears on a different event than either",
			parked.QueueCause, CauseGroupOwesCollection)
	}
	if protocol.IsTerminal(parked.Status) {
		t.Errorf("dig B's demand went to %s. Ordering is congestion, and congestion waits (law 1)",
			parked.Status)
	}
}

// TestDigAdmission_TheDigIsAdmittedOnceTheCollectionIsMade is arm 3's control,
// and without it the test above is satisfied by a rule that refuses every dig in
// a group where anything has ever been dug.
//
// Same fixture, same holding dig — and then the bin is collected. The lane
// releases, the dig finishes, the group owes nothing, and B is admitted.
func TestDigAdmission_TheDigIsAdmittedOnceTheCollectionIsMade(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, laneA, laneB, _, aSlots, bSlots, bp := setupCapacityGroup(t, db, "COLL")

	spare := &nodes.Node{Name: "COLL-SPARE", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spare), "create the spare slot")

	createTestBinAtNode(t, db, bp.Code, aSlots[0].ID, "COLL-A-BLK")
	createTestBinAtNode(t, db, bp.Code, aSlots[1].ID, "COLL-A-TGT")
	createTestBinAtNode(t, db, bp.Code, bSlots[0].ID, "COLL-B-BLK")
	createTestBinAtNode(t, db, bp.Code, bSlots[1].ID, "COLL-B-TGT")

	serviceDigHolding(t, db, d, grp, laneA, aSlots[1], bp, "coll-a")

	// THE COLLECTION. Any mover ends it — that is arm 2's whole property.
	tgt := mustBinAt(t, db, aSlots[1])
	testutil.MustNoErr(t, db.MoveBinToTransit(tgt.ID, transitNode(t, db, "COLL-TRANSIT").ID),
		"collect the bin the dig uncovered")
	d.maybeReleaseDigOnLastBlockerOut(laneA.ID)

	demandB := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "coll-b"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "COLL-LINE-B").Name
		o.Status = protocol.StatusPending
	})
	if _, pe := d.planner.planBuriedReshuffle(demandB, &BuriedError{
		Bin: mustBinAt(t, db, bSlots[1]), Slot: bSlots[1], LaneID: laneB.ID}); pe != nil {
		t.Fatalf("dig B was refused after the collection was made and %s released: %s: %s. The rule "+
			"is an ordering, not a ban — a group that has ever dug must still be able to dig again",
			laneA.Name, pe.Code, pe.Detail)
	}
}

// TestDigAdmission_TheSameDigIsAdmittedWhenNothingIsOwed is the control, and
// without it the test above is satisfied by a planner that refuses every second
// dig for any reason at all.
//
// Identical fixture, identical dig B, one difference: dig A never runs. The group
// owes nothing, the single slot is genuinely available, and B is admitted.
//
// MUTATION (verified): ask planUnbury for len(blockers)+outstanding+1. This fires
// — B is refused with nothing owed and the slot standing free. Any over-count has
// that shape, and over-counting is the direction that turns the claim into a
// famine of its own making: a group that refuses digs it can actually afford.
//
// Note what this test CANNOT catch, so nobody reads more into it than it proves:
// no dig runs here, so a mutation to how claims are COUNTED (dropping the
// unbound-destination clause, dropping the asker exemption) leaves this green.
// The counting rules are the sibling test's job; this one pins the zero case.
func TestDigAdmission_TheSameDigIsAdmittedWhenNothingIsOwed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, _, laneB, park, aSlots, bSlots, bp := setupCapacityGroup(t, db, "CAPOK")

	createTestBinAtNode(t, db, bp.Code, aSlots[0].ID, "CAPOK-A-BLK")
	createTestBinAtNode(t, db, bp.Code, aSlots[1].ID, "CAPOK-A-TGT")
	createTestBinAtNode(t, db, bp.Code, bSlots[0].ID, "CAPOK-B-BLK")
	createTestBinAtNode(t, db, bp.Code, bSlots[1].ID, "CAPOK-B-TGT")

	demandB := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "capok-b"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "CAPOK-LINE-B").Name
		o.Status = protocol.StatusPending
	})
	if _, pe := d.planner.planBuriedReshuffle(demandB, &BuriedError{
		Bin: mustBinAt(t, db, bSlots[1]), Slot: bSlots[1], LaneID: laneB.ID}); pe != nil {
		t.Fatalf("dig B was refused with NO dig running in the group and %s standing free: %s: %s. "+
			"The claim is counting room that nobody owes", park.Name, pe.Code, pe.Detail)
	}
}
