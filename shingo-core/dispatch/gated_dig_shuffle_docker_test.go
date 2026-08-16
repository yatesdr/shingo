//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// setupGatedDigGroup builds the geometry that broke the lane-stress rig: a group
// holding a gated lane to dig out of, ANOTHER gated lane standing empty, and one
// ungated lane also standing empty.
//
//	GRP-GD
//	├── GD-DUG   (MARKED)   D1 blocker · D2 target      <- the dig happens here
//	├── GD-GATED (MARKED)   G1 empty   · G2 empty       <- the trap
//	└── GD-OPEN  (unmarked) O1 empty   · O2 empty       <- the only legal answer
//
// Both empty lanes look equally attractive to findShuffleSlots on every measure
// it used to consult: enabled, in the group, not the dug lane, slots free, no
// inbound traffic. The one thing separating them is whether a plan that touches
// the dug lane can legally also touch them.
func setupGatedDigGroup(t *testing.T, db *store.DB, withOpenLane bool) (grp, dug, gated, open *nodes.Node, dugSlots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: "PGD"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: "GRP-GD", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(name, mark string) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		if mark != "" {
			testutil.MustNoErr(t, db.SetNodeProperty(lane.ID, PropLaneGatePoint, mark), "mark "+name)
		}
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
	dug, dugSlots = mkLane("GD-DUG", "GD-DUG-WAIT")
	gated, _ = mkLane("GD-GATED", "GD-GATED-WAIT")
	if withOpenLane {
		open, _ = mkLane("GD-OPEN", "")
	}
	return
}

// TestGatedDig_BlockerNeverGoesToAnotherGatedLane pins the defect the lane-stress
// rig surfaced on 2026-08-09, minutes after it came up.
//
// THE FAILURE, END TO END. A dig out of a MARKED lane picked its shuffle slot in
// a different MARKED lane. The leg's plan therefore touched two gated lanes, and
// spliceLaneWait refuses that outright -- one wait per plan, because releasing
// per-wait is machinery the transform deliberately does not build
// (lane_gate_dispatch.go rule 2). The refusal is a bare error, so:
//
//	leg fails -> parent fails ("child order N failed during reshuffle")
//	          -> the two-robot swap the parent was supplying fails
//	          -> the evac is cancelled so it cannot strand the line
//	          -> the line is starved, and the demand is gone.
//
// Four terminal orders from one unexpressible plan. Worse than the cascade is
// that NOTHING SELF-CLEARS: both marks stay where they are, so the re-plan picks
// the same slot and fails the same way, forever. This is the wedge shape the
// stream refuses to build, arrived at from a direction nobody had walked --
// demo.yaml has zero marks, so no sim before this one could reach it.
//
// THE FIX IS PLAN-TIME AVOIDANCE, not a better refusal. The dig never wanted that
// particular slot; it wanted A slot. So the candidate list drops slots the plan
// could not legally use, which is the same shape as the exclusion immediately
// below it in findShuffleSlots -- never park a blocker back into the lane being
// dug out of.
//
// DESIGN 16 rule 7: the shuffle-slot pick is the first thing that can go wrong
// here. The lane is buried, the group resolves, the lock is free, and the
// blocker count is 1 -- so the candidate list is what the test is looking at.
//
// MUTATION (verified 2026-08-09): delete the `dugLaneGated && ...` skip in
// findShuffleSlots' Pass 2. This fires -- the blocker is planned into GD-GATED,
// and the plan it produces is one spliceLaneWait refuses.
func TestGatedDig_BlockerNeverGoesToAnotherGatedLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, dug, gated, open, dugSlots, bp := setupGatedDigGroup(t, db, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "BIN-GD-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "BIN-GD-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := &orders.Order{EdgeUUID: "dig-gated", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-GD"}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	if pe != nil {
		t.Fatalf("the dig had an ungated lane to park its blocker in and must have planned: %s: %s", pe.Code, pe.Detail)
	}

	// WHERE THE BLOCKER WENT. Read it off the leg rather than off the planner's
	// return, because the leg is what dispatch will splice.
	var dest string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT delivery_node FROM orders WHERE parent_order_id = $1 ORDER BY sequence LIMIT 1`, order.ID,
	).Scan(&dest), "read the leg's delivery node")

	destNode, err := db.GetNodeByDotName(dest)
	if err != nil || destNode == nil {
		t.Fatalf("resolve the leg's delivery node %q: %v", dest, err)
	}
	destLane, err := db.LaneForNode(destNode.ID)
	if err != nil {
		t.Fatalf("lane for %q: %v", dest, err)
	}
	if destLane == nil {
		return // a direct group child is not in a lane at all, and cannot collide
	}
	if destLane.ID == gated.ID {
		t.Fatalf("the dig out of MARKED lane %s parked its blocker in %s, which is ALSO marked.\n"+
			"spliceLaneWait refuses a plan touching two gated lanes, so this leg fails, which fails "+
			"the parent, which fails whatever the parent was supplying. Nothing clears it either -- "+
			"both marks stay put, so the re-plan picks this slot again. %s was free and ungated.",
			dug.Name, destLane.Name, open.Name)
	}
	if destLane.ID != open.ID {
		t.Errorf("blocker went to lane %s, want the ungated %s", destLane.Name, open.Name)
	}
}

// TestGatedDig_NoUngatedSlotWaitsRatherThanPickingOne is the other half, and it
// is the half that makes the exclusion safe to add.
//
// Tightening a candidate list makes "nothing available" more frequent, which is
// only acceptable because that outcome WAITS. ErrNoShuffleSlot is transient and
// retries; a slot frees the moment any other order clears one. Without this arm
// the fix above would trade a wedge for a different wedge -- a dig that can only
// reach gated lanes would have no candidates and no story about what happens
// next.
//
// The same reasoning is written next to shuffleSlotFree for the previous
// tightening of this function. It is load-bearing both times.
//
// TWO MUTATIONS, both run 2026-08-09, because this test carries two claims.
//
//  1. Neuter the exclusion in findShuffleSlots (`if false && dugLaneGated`). The
//     FIRST arm fires: the dig plans into the other marked lane even though that
//     plan cannot be spliced.
//  2. Drop codeNoShuffleSlot from the transient set in planning_service.go's
//     classifier. The Transient() assertion fires -- and on the rig that is the
//     difference between a dig that resumes when a slot frees and a demand that
//     is gone.
func TestGatedDig_NoUngatedSlotWaitsRatherThanPickingOne(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, dug, gated, _, dugSlots, bp := setupGatedDigGroup(t, db, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "BIN-GD-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "BIN-GD-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := &orders.Order{EdgeUUID: "dig-nogap", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-GD"}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	if pe == nil {
		var dest string
		// The FIRST leg by sequence is the unbury; the last is the retrieve, whose
		// delivery node is the line and would say nothing about the blocker.
		_ = db.DB.QueryRow(
			`SELECT delivery_node FROM orders WHERE parent_order_id = $1 ORDER BY sequence LIMIT 1`,
			order.ID).Scan(&dest)
		t.Fatalf("the only free slots were in the other MARKED lane %s, and the dig planned anyway "+
			"(blocker to %q). That plan cannot be spliced.", gated.Name, dest)
	}
	if pe.Code != codeNoShuffleSlot {
		t.Fatalf("refused with %q (%s), want %q", pe.Code, pe.Detail, codeNoShuffleSlot)
	}
	if !pe.Transient() {
		t.Errorf("no-shuffle-slot came back NON-transient, which terminates the demand. "+
			"A gated dig with nowhere legal to park waits for an ungated slot to free; it does not die. "+
			"Detail: %s", pe.Detail)
	}
	// And it left nothing behind to unwind.
	var legs int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT COUNT(*) FROM orders WHERE parent_order_id = $1`, order.ID).Scan(&legs), "count legs")
	if legs != 0 {
		t.Errorf("%d leg(s) survive a refused plan — a half-built compound is worse than no plan", legs)
	}
}
