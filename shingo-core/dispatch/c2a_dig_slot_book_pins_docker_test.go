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
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// c2a_dig_slot_book_pins_docker_test.go — C2a's three pins. 1 and 3 were
// committed red at 660fd228 and went green with the plan-time verdict
// (plan.DigFits); 2, 0b's pool half, has been green since main's shuffle walk
// read the slot book (rebase 4).
//
// Source: dig-starvation-review-2026-09-25/SYNTH-round1.md §3 (rows 0b and
// "4 (plan)") and PLAN-unification-phases-2026-09-24.md, the C2a row.

// TestC2aPin_DigOneSlotShortParksUnderNoShuffleSlot — GREEN since C2a A.2
// (red at 660fd228: parked under read-failed).
//
// A dig that is "1 slot(s) short" because the only lane with room has a robot
// in it (Hold B, shuffleSlotsFrom -> LaneOccupiedParkingError) parks its demand
// under `read-failed`. classifyPlanError has no arm for ErrLaneOccupiedParking,
// so the error falls to the readFailed() arm (any non-nil error that is not
// sql.ErrNoRows) and handleComplexBurial takes laneClearReadFailed. parkPair
// then mirrors that cause onto the pair's partner. This is the log line on the
// edge2 after-run (21 pair parks on demand 450) and on lane-stress (149 before,
// 132 after):
//
//	complex: could not read Lane_04 while planning a dig for demand 450
//	  (find shuffle slots: ... Lane_01 is occupied and this dig is 1 slot(s) short ...)
//	complex: pair parked on order N's cause (read-failed)
//
// Nothing failed to read. The true cause is CauseNoShuffleSlot (owner,
// 2026-09-26).
//
// Flat bug, fixed at the plan-time verdict (plan.DigFits, asked by planUnbury):
// a short count comes back as ErrNoShuffleSlot, whatever made it short, and the
// demand parks under CauseNoShuffleSlot.
func TestC2aPin_DigOneSlotShortParksUnderNoShuffleSlot(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, _, bp := setupDwellGroup(t, db, "C2A1", 1, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "C2A1-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "C2A1-TGT")

	// A robot from another order inside the only lane with room: the dig is one
	// slot short without it.
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "c2a1-inside" })
	_, aErr := reservations.AcquireOccupancy(db.DB, inside.ID, sib.ID)
	testutil.MustNoErr(t, aErr, "occupy the sibling lane")

	demand := mkQueuedComplexParent(t, db, "c2a1-demand", bp.Code)
	d.planBuriedReshuffleAtIntake(demand, bp.Code, "line-1",
		&BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})

	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand back")
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("the demand is %q (%q): the fixture no longer reaches the park", after.Status, after.ErrorDetail)
	}
	if QueueCause(after.QueueCause) != CauseNoShuffleSlot {
		t.Errorf("demand %d parked under %q (%q) for a dig one slot short; the true cause is %q",
			demand.ID, after.QueueCause, after.QueueReason, CauseNoShuffleSlot)
	}
}

// TestC2aPin_DigPlanAndPoolDoNotCountAReservedSlot — GREEN (0b), since main's
// "skip slots another order reserved in the shuffle walk" (rebase 4).
//
// The only free slot in the group carries another order's PENDING slot
// reservation (what reserveComplexPlan writes for a waiting complex order).
// Before, the shuffle pool offered it and the plan-time count was satisfied by
// it, while the leg's claim refused it (ErrReservationConflict at ReserveSlot):
// two books with two answers, the 950 failed ReserveSlot transactions of the
// dig-starvation run. The pool now reads the slot book through
// nodes.SlotTakeableSQL, once per ask, so neither the pool nor the plan-time
// count sees the spoken slot. C2a keeps the planner's read and the headroom
// verdict; this pin is the pool's half, landed by main.
func TestC2aPin_DigPlanAndPoolDoNotCountAReservedSlot(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, dug, _, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "C2A2", 1, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "C2A2-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "C2A2-TGT")
	reserved := sibSlots[0]

	// A stranger's waiting complex order holding a pending reservation on the one
	// free slot, as reserveComplexPlan leaves it.
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "c2a2-holder"
		o.OrderType = OrderTypeComplex
		o.PayloadCode = bp.Code
		o.Status = protocol.StatusQueued
		o.DeliveryNode = reserved.Name
	})
	testutil.MustNoErr(t, db.ReserveSlot(reserved.ID, holder.ID), "the holder's pending slot reservation")

	demand := mkQueuedComplexParent(t, db, "c2a2-demand", bp.Code)
	asker := digAskerFor(demand)

	slots, poolErr := findShuffleSlots(db, dug.ID, grp.ID, 1, asker, noClaimantYet, nil)
	for _, s := range slots {
		if s.ID == reserved.ID {
			t.Errorf("the shuffle pool offered %s, which order %d holds on the reservation book",
				s.Name, holder.ID)
		}
	}
	if !errors.Is(poolErr, ErrNoShuffleSlot) {
		t.Errorf("the pool answered %v (offered %v), want ErrNoShuffleSlot: the only free slot is spoken for",
			poolErr, nodeNames(slots))
	}

	target, err := db.GetBin(target.ID)
	testutil.MustNoErr(t, err, "reload the target")
	if _, _, planErr := planUnbury(db, target, dugSlots[1], dug, grp.ID, asker, asker.OrderID, nil); !errors.Is(planErr, ErrNoShuffleSlot) {
		t.Errorf("the plan-time count answered %v, want ErrNoShuffleSlot: the only free slot, %s, is "+
			"order %d's on the reservation book", planErr, reserved.Name, holder.ID)
	}
}

// TestC2aPin_SixDigsFourParks_TheFifthParksOnPaper — GREEN since C2a A.2
// (red at 660fd228: digs 5 and 6 started owing 4 and 5).
//
// Six buried demands, each one blocker deep, in a group with four free parking
// slots. Each dig's plan-time count (planUnbury -> findShuffleSlots with
// count = len(blockers)) asks only whether ITS blockers fit the pool as it
// stands, and a planned dig parks nothing until its leg dwells, so every count
// sees four free slots and all six digs start. Two of them can never park; on
// the rig that is the wedge (six started, lifted and froze).
//
// §2.3 ruling (the plan states whether it is satisfiable), extending the
// owner's standing ruling at planUnbury ("an unsatisfiable count is a capacity
// problem, not a dispatch one"). The plan-time verdict is len(blockers) + owed
// <= reachable, owed being the destination-less non-terminal legs of the
// group's dig-held lanes (store.DB.DigFitFacts). The fifth and sixth demands
// start nothing and wait under CauseNoShuffleSlot.
func TestC2aPin_SixDigsFourParks_TheFifthParksOnPaper(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lanes, bp := setupDigsAndParks(t, db, "C2A3", 6, 4)

	// Every lane is packed before the first dig plans, so the four parks are the
	// group's whole room for every dig. Seeded lane by lane inside the planning
	// loop, the lanes not yet seeded were empty parking for the digs before them.
	type buried struct {
		target *bins.Bin
		slot   *nodes.Node
	}
	var targets []buried
	for i, lane := range lanes {
		slots, err := db.ListLaneSlots(lane.ID)
		testutil.MustNoErr(t, err, "list lane slots")
		createTestBinAtNode(t, db, bp.Code, slots[0].ID, fmt.Sprintf("C2A3-BLK%d", i+1))
		target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, fmt.Sprintf("C2A3-TGT%d", i+1))
		targets = append(targets, buried{target, slots[1]})
	}
	var demands []*orders.Order
	for i, lane := range lanes {
		demand := mkQueuedComplexParent(t, db, fmt.Sprintf("c2a3-demand-%d", i+1), bp.Code)
		d.planBuriedReshuffleAtIntake(demand, bp.Code, "line-1",
			&BuriedError{Bin: targets[i].target, Slot: targets[i].slot, LaneID: lane.ID})
		demands = append(demands, demand)
	}

	started := func(o *orders.Order) (*orders.Order, bool) {
		after, err := db.GetOrder(o.ID)
		testutil.MustNoErr(t, err, "reload demand")
		kids, err := db.ListChildOrders(o.ID)
		testutil.MustNoErr(t, err, "list the dig's legs")
		return after, len(kids) > 0
	}

	for i, o := range demands[:4] {
		if after, ok := started(o); !ok {
			t.Fatalf("dig %d of the first four did not start (status %q, cause %q): the fixture has drifted",
				i+1, after.Status, after.QueueCause)
		}
	}
	for i, o := range demands[4:] {
		n := i + 5
		after, ok := started(o)
		switch {
		case ok:
			t.Errorf("dig %d (demand %d) started with %d blocker(s) already owed against a 4-park pool; "+
				"its own blocker has nowhere to go", n, o.ID, n-1)
		case QueueCause(after.QueueCause) == CauseNoShuffleSlot && !protocol.IsTerminal(after.Status):
			// waits on paper
		default:
			t.Fatalf("dig %d neither started nor waits under %q (status %q, cause %q): the fixture has drifted",
				n, CauseNoShuffleSlot, after.Status, after.QueueCause)
		}
	}

}

// setupDigsAndParks builds a group of `digs` two-deep lanes and `parks` free
// parking nodes directly under the group (Pass 1 of the shuffle pool). The dig
// lanes are the group's only lanes, so the parks are its whole room.
func setupDigsAndParks(t *testing.T, db *store.DB, prefix string, digs, parks int) (*nodes.Node, []*nodes.Node, *payloads.Payload) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "LANE type")

	bp := &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")
	grp := &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	var lanes []*nodes.Node
	for i := 1; i <= digs; i++ {
		lane := &nodes.Node{Name: fmt.Sprintf("%s-L%d", prefix, i), NodeTypeID: &lanType.ID,
			ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create dig lane")
		for depth := 1; depth <= 2; depth++ {
			at := depth
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", lane.Name, depth), ParentID: &lane.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create dig slot")
		}
		reloaded, err := db.GetNode(lane.ID)
		testutil.MustNoErr(t, err, "reload dig lane")
		lanes = append(lanes, reloaded)
	}
	for i := 1; i <= parks; i++ {
		p := &nodes.Node{Name: fmt.Sprintf("%s-PARK%d", prefix, i), ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(p), "create park")
	}
	grpRaw, err := db.GetNode(grp.ID)
	grp = testutil.Must(t, grpRaw, err, "reload group")
	return grp, lanes, bp
}

func nodeNames(ns []*nodes.Node) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}
