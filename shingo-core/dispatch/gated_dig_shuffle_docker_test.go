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

	// WHERE THE BLOCKER WENT, READ AT THE MOMENT IT IS DECIDED.
	//
	// This used to read delivery_node straight off the leg after planning, because
	// planning is where the slot was picked. Under the outbound dwell the leg is
	// dispatched with no destination and stands in GD-DUG holding the blocker until
	// Core chooses — so the read moves to after the release, which is the same fact
	// one moment later. The exclusion being pinned (never park into a second gated
	// lane) is unchanged and still lives in findShuffleSlots; only the caller moved.
	legs := legsOf(t, db, order.ID)
	if len(legs) == 0 {
		t.Fatal("the dig wrote no legs")
	}
	released := releaseDwell(t, d, db, legs[0])
	dest := released.DeliveryNode

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

// TestDig_NeverParksIntoALaneAnotherDigIsHolding is F-19 — the defect that
// stopped the lane-stress plant dead at order 70.
//
// THE CASCADE, as measured 2026-08-10. A complex parent digs LS_D1 out to expose
// the PANEL-A it wants. The dig completes in EXPOSE mode, so its lane lock is not
// released — it is transferred to the parent and held until the parent picks the
// bin up (extendLaneLockForExposeMode), whose documented purpose is "closes the
// post-compound / pre-pickup re-burial window".
//
// Before the parent picks up, it re-plans for a second buried bin elsewhere — and
// findShuffleSlots hands that dig the slots LS_D1 just freed. Pass 2 fills
// DEEPEST-FIRST, so it packs from the back forward and entombs the exposed bin
// behind a full lane. The parent then digs the third lane, buries the second
// lane's target bin, and so on: three generations, twelve legs, eight minutes, bins
// 2/3/4 dug out twice, nothing ever picked up, and a lane lock accumulated per
// generation until eight of twenty-two lanes were held and the plant stopped
// creating orders.
//
// The lock stopped another robot ENTERING. It never stopped the lane's freed
// slots being handed out as shuffle space, because this function did not ask.
//
// THE EXCLUSION IS OWNER-BLIND ON PURPOSE, which is what this test pins. Every
// other ownership test in the system exempts the owner — ownsDig, the claim CAS,
// admission — and exempting it here is precisely what let a parent bury its own
// target bin. So the dig below is owned by the SAME order that holds the other lane's
// lock, and it still must not park there.
//
// MUTATION (run 2026-08-10): delete the `digLocked[c.ID]` skip in Pass 2. The
// blocker is planned straight into the lane the same parent is holding open, on
// top of the bin it is waiting to collect.
func TestDig_NeverParksIntoALaneAnotherDigIsHolding(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, dug, _, open, dugSlots, bp := setupGatedDigGroup(t, db, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "BIN-F19-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "BIN-F19-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := &orders.Order{EdgeUUID: "f19", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-F19"}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	// THE OPEN LANE HOLDS A BIN SOMEBODY IS COMING FOR. It sits at the BACK; the
	// slots in front of it are the ones the last excavation emptied, and are
	// exactly what must not be refilled.
	//
	// ── THE SPELLING CHANGED HERE; THE MEANING DID NOT ───────────────────
	//
	// This used to register a pending_lane_extensions row — the expose bridge's
	// record that a completed dig had uncovered this bin and its parent was walking
	// back to collect it. The bridge is deleted (the parent no longer walks back at
	// all), and the fact it recorded is now carried by the CLAIM: a hard claim IS
	// "a robot is on its way to this bin". So the fixture claims it, and
	// findShuffleSlots refuses the slots in front of it through the same clause the
	// store selector uses.
	//
	// Same owning order as the dig about to be planned, deliberately and still:
	// exempting the owner is what let a parent bury its own target bin, and the
	// claims-keyed exclusion is owner-blind for exactly that reason.
	openSlots, err := db.ListLaneSlots(open.ID)
	testutil.MustNoErr(t, err, "list open-lane slots")
	exposed := createTestBinAtNode(t, db, bp.Code, openSlots[len(openSlots)-1].ID, "BIN-F19-EXPOSED")
	testdb.ClaimBinForTest(t, db, exposed.ID, order.ID)

	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	if pe == nil {
		var dest string
		_ = db.DB.QueryRow(
			`SELECT delivery_node FROM orders WHERE parent_order_id = $1 ORDER BY sequence LIMIT 1`,
			order.ID).Scan(&dest)
		destNode, _ := db.GetNodeByDotName(dest)
		destLane, _ := db.LaneForNode(destNode.ID)
		if destLane != nil && destLane.ID == open.ID {
			t.Fatalf("the dig parked its blocker into %s, which THIS SAME ORDER is holding under a "+
				"dig lock. That lane's free slots are the product of an excavation somebody is still "+
				"waiting on; filling them re-buries the bin the lock exists to protect. Deepest-first "+
				"packing means it fills from the back, so the exposed bin ends up behind a full lane. "+
				"That is the cascade that stopped the plant at order 70.", open.Name)
		}
		t.Fatalf("blocker went to %q — the only free slots were in a dig-locked lane, so this must "+
			"have waited with no-shuffle-slot instead of planning", dest)
	}
	if pe.Code != codeNoShuffleSlot {
		t.Fatalf("refused with %q (%s), want %q — every candidate lane was dig-locked, which is "+
			"congestion and must WAIT, not geometry", pe.Code, pe.Detail, codeNoShuffleSlot)
	}
	if !pe.Transient() {
		t.Errorf("no-shuffle-slot came back NON-transient, which terminates the demand. A lock frees "+
			"as soon as any dig completes. Detail: %s", pe.Detail)
	}
}
