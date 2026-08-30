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

// TestGatedDig_BlockerMayGoToAnotherGatedLane pins the LIFTING of an exclusion
// that had outlived its reason.
//
// -- WHAT THIS TEST USED TO ASSERT, AND WHY THAT EXPIRED -------------------
//
// It was TestGatedDig_BlockerNeverGoesToAnotherGatedLane, and it was right when
// it was written (lane-stress rig, 2026-08-09). spliceLaneWait then allowed ONE
// gated lane per plan and refused a second outright, so a dig out of a marked
// lane whose blocker landed in a marked lane failed at the splice -- failing the
// parent, the swap it supplied, and the evac. Four terminal orders, and nothing
// self-cleared, because both marks stay where they are.
//
// lane_gate_dispatch.go rule 2 is now "A WAIT PER GATED LANE THE PLAN ENTERS".
// The plan this test was built to prevent is expressible: it dispatches with a
// wait at each mark, each released by its own lane's admission. The refusal that
// forced the exclusion is gone, and findShuffleSlots' own comment said so and
// asked for a measurement before lifting it.
//
// -- THE MEASUREMENT -------------------------------------------------------
//
// demo.yaml 2026-08-31, all 16 lanes marked for the first time anywhere. With
// every lane gated, "park in an ungated lane" named no slot in the plant, so
// every dig held from the first minute of the run -- six of them, each with a
// partner leg behind it and a starved line behind that. A wait whose releaser is
// "somebody un-marks a lane" is not a wait.
//
// So the blocker may now land in GD-GATED, and the geometry that used to be a
// trap is just another candidate.
func TestGatedDig_BlockerMayGoToAnotherGatedLane(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, dug, gated, _, dugSlots, bp := setupGatedDigGroup(t, db, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "BIN-GD-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "BIN-GD-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	order := &orders.Order{EdgeUUID: "dig-gated", StationID: "line-1", OrderType: OrderTypeRetrieve,
		Status: StatusPending, Quantity: 1, PayloadCode: bp.Code, DeliveryNode: "LINE-GD"}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	// NO UNGATED LANE EXISTS IN THIS FIXTURE, deliberately: under the old
	// exclusion this is the shape that had no candidates at all and waited
	// forever. It must now plan.
	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	if pe != nil {
		t.Fatalf("the only free slots are in the other MARKED lane %s and the dig refused to plan "+
			"(%s: %s). That is the exclusion still in place -- and with every lane marked it means no "+
			"dig anywhere can ever start.", gated.Name, pe.Code, pe.Detail)
	}

	legs := legsOf(t, db, order.ID)
	if len(legs) == 0 {
		t.Fatal("the dig wrote no legs")
	}
	released := releaseDwell(t, d, db, legs[0])
	destNode, err := db.GetNodeByDotName(released.DeliveryNode)
	if err != nil || destNode == nil {
		t.Fatalf("resolve the leg's delivery node %q: %v", released.DeliveryNode, err)
	}
	destLane, err := db.LaneForNode(destNode.ID)
	if err != nil {
		t.Fatalf("lane for %q: %v", released.DeliveryNode, err)
	}
	if destLane == nil {
		return // a direct group child is in no lane and cannot collide
	}
	if destLane.ID == dug.ID {
		t.Fatalf("the blocker was parked back into %s, the lane being dug out of -- that exclusion "+
			"is a different one and it stays", dug.Name)
	}
	if destLane.ID != gated.ID {
		t.Errorf("blocker went to lane %s, want the other marked lane %s", destLane.Name, gated.Name)
	}
}

// TestGatedDig_NoSlotAnywhereWaitsRatherThanFailing is the other half, and it is
// the half that keeps the whole thing safe.
//
// A candidate list can always come up empty, and that outcome must WAIT:
// ErrNoShuffleSlot is transient and retries, and a slot frees the moment any
// other order clears one. What changed when the gated-lane exclusion came out is
// only WHEN empty happens -- it used to include "the only free slots are behind
// another mark", a condition nothing in the plant clears, and now it means what
// it says.
//
// The same reasoning is written next to shuffleSlotFree for an earlier
// tightening of this function. It is load-bearing every time.
//
// MUTATION: drop codeNoShuffleSlot from the transient set in
// planning_service.go's classifier. The Transient() assertion fires -- the
// difference between a dig that resumes when a slot frees and a demand that is
// gone.
func TestGatedDig_NoSlotAnywhereWaitsRatherThanFailing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, dug, gated, _, dugSlots, bp := setupGatedDigGroup(t, db, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "BIN-GD-BLK")
	target := createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "BIN-GD-TGT")
	// FILL THE OTHER MARKED LANE. It is a legal candidate now, so the only way to
	// reach "no slot anywhere" is for there to genuinely be none.
	gatedSlots, gsErr := db.ListLaneSlots(gated.ID)
	testutil.MustNoErr(t, gsErr, "list the other marked lane slots")
	for gi, gs := range gatedSlots {
		createTestBinAtNode(t, db, bp.Code, gs.ID, fmt.Sprintf("BIN-GD-FILL%d", gi))
	}

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

	// -- WHAT THIS TEST ASSERTED BEFORE, AND WHY THE ASSERTION MOVED -------
	//
	// It used to expect a REFUSAL here: GD-OPEN was excluded by the claim, GD-GATED
	// was excluded for being a second marked lane, so nothing was left and the dig
	// waited on no-shuffle-slot. The second of those exclusions is gone (see
	// findShuffleSlots), so this fixture now has a legal answer and the dig must
	// take it.
	//
	// The F-19 claim itself is untouched and is what the assertions below still
	// pin: whatever the pool contains, the blocker must never land in the lane a
	// dig is holding open. "No slot anywhere still waits" moved to
	// TestGatedDig_NoSlotAnywhereWaitsRatherThanFailing, which builds a fixture
	// where there genuinely is none.
	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: dugSlots[1], LaneID: dug.ID})
	if pe != nil {
		t.Fatalf("the dig refused (%s: %s), but the other marked lane was free and is a legal "+
			"candidate now -- only %s is off-limits, because this same order holds it under a dig "+
			"lock", pe.Code, pe.Detail, open.Name)
	}

	legs := legsOf(t, db, order.ID)
	if len(legs) == 0 {
		t.Fatal("the dig wrote no legs")
	}
	released := releaseDwell(t, d, db, legs[0])
	destNode, err := db.GetNodeByDotName(released.DeliveryNode)
	if err != nil || destNode == nil {
		t.Fatalf("resolve the leg delivery node %q: %v", released.DeliveryNode, err)
	}
	destLane, err := db.LaneForNode(destNode.ID)
	testutil.MustNoErr(t, err, "lane for the leg destination")
	if destLane != nil && destLane.ID == open.ID {
		t.Fatalf("the dig parked its blocker into %s, which THIS SAME ORDER is holding under a "+
			"dig lock. That lane free slots are the product of an excavation somebody is still "+
			"waiting on; filling them re-buries the bin the lock exists to protect. Deepest-first "+
			"packing means it fills from the back, so the exposed bin ends up behind a full lane. "+
			"That is the cascade that stopped the plant at order 70.", open.Name)
	}
}
