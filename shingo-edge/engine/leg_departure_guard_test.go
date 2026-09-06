package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// The admission guards, and the one thing they must never do: disagree.
// CanAcceptOrders and hasActiveSwap read the runtime SLOTS;
// guardPositionSpokenFor's second arm reads the durable ROWS at the node. All
// three ask orderWorksTheCell.

// seedGuardRuntime creates the node's runtime row and points it at the claim —
// what a live cell always has, and what UpdateProcessNodeRuntimeOrders needs to
// write into.
func seedGuardRuntime(t *testing.T, db *store.DB, nodeID int64, claim *processes.NodeClaim) {
	t.Helper()
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claim.ID, claim.UOPCapacity), "set runtime")
}

// assertGuards runs all three admission surfaces against one cell and insists
// they agree. want=true means "the next swap may be ordered".
func assertGuards(t *testing.T, eng *Engine, db *store.DB, nodeID int64,
	node *processes.Node, claim *processes.NodeClaim, want bool, why string) {
	t.Helper()

	gotAccept, reason := eng.CanAcceptOrders(nodeID)
	if gotAccept != want {
		t.Errorf("CanAcceptOrders = (%v, %q), want admit=%v — %s", gotAccept, reason, want, why)
	}

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	gotSwap := !hasActiveSwap(eng, rt)
	if gotSwap != want {
		t.Errorf("hasActiveSwap says busy=%v, want busy=%v — %s", !gotSwap, !want, why)
	}

	gerr := eng.guardPositionSpokenFor(node, rt, claim)
	gotGuard := gerr == nil
	if gotGuard != want {
		t.Errorf("guardPositionSpokenFor = %v, want admit=%v — %s", gerr, want, why)
	}

	if gotAccept != gotSwap || gotAccept != gotGuard {
		t.Fatalf("THE THREE GUARDS DISAGREE (CanAcceptOrders=%v, notBusy=%v, guardPositionSpokenFor=%v). "+
			"The slot check and the durable-row check answer the same question about the same cell and "+
			"must never differ: %s", gotAccept, gotSwap, gotGuard, why)
	}
}

// TestDepartedLegAdmitsTheNextSwap_AllThreeGuardsAgree is the core pin: a
// departed, still-non-terminal leg in a runtime slot must be admitted by every
// guard, and an undeparted one refused by every guard. One test so a fix applied
// to two of the three fails here rather than in a plant.
func TestDepartedLegAdmitsTheNextSwap_AllThreeGuardsAgree(t *testing.T) {
	t.Parallel()
	for _, slot := range []string{"active", "staged"} {
		t.Run(slot, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobot, "")
			eng := testEngine(t, db)
			eng.logFn = func(string, ...any) {}
			seedGuardRuntime(t, db, nodeID, claim)

			_, evac := BuildTwoRobotSwapSteps(claim)
			leg := mkSwapLeg(t, db, nodeID, "uuid-guard-evac", evac, "")
			testutil.MustNoErr(t, db.UpdateOrderStatus(leg.ID, string(protocol.StatusInTransit)), "in_transit")
			if slot == "active" {
				testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &leg.ID, nil), "slot")
			} else {
				testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &leg.ID), "slot")
			}

			// BEFORE: the robot is still at the press. Everything refuses.
			assertGuards(t, eng, db, nodeID, node, claim, false,
				"an undeparted, non-terminal leg still occupies the cell")

			// The departure. (The evac's press pickup is its last cell step;
			// stamping directly here keeps this test about the GUARDS.)
			_, err := db.MarkOrderDeparted(leg.ID, time.Now().UTC())
			testutil.MustNoErr(t, err, "mark departed")

			// AFTER: still in_transit, still in the slot, still a live order —
			// and no longer the cell's.
			after, err := db.GetOrder(leg.ID)
			testutil.MustNoErr(t, err, "re-read leg")
			if protocol.IsTerminal(after.Status) {
				t.Fatal("fixture drift: the leg must still be NON-TERMINAL, or this proves nothing")
			}
			rt, err := db.GetProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "runtime")
			if rt.ActiveOrderID == nil && rt.StagedOrderID == nil {
				t.Fatal("fixture drift: the leg must still be in a runtime slot, or the slot guards " +
					"would admit for the wrong reason")
			}
			assertGuards(t, eng, db, nodeID, node, claim, true,
				"a departed leg is carrying a bin away and no longer occupies the cell")
		})
	}
}

// TestSingleRobotWindow_RefusedAt4_AdmittedAt8OnceThePlacementIsRecorded is the
// window that started this, pinned at both ends and at the fact in the middle.
//
// single_robot lifts the old bin off the press at step 4, puts the new one down
// at step 7, and lifts the old one off OutboundStaging at step 8. Between 4 and
// 8 the cell is genuinely occupied — a second swap's step 5 would drop onto the
// staging slot the old bin is in — and every guard must refuse. From step 8 the
// robot is driving away with a full cell behind it and every guard must admit,
// or the cell stays shut for the whole supermarket trip.
//
// "A full cell behind it" is the part that was assumed rather than checked. The
// robot's departure proves the robot has gone; it does not prove the carrier it
// set down is on anyone's books, and between those two facts the cell reads
// EMPTY to Core, to the level sweep and to the downgrade — while being filtered
// out of the one witness that knew better. So the step-7 record is now driven
// here explicitly, and the sibling below is the same run without it.
//
// It used to be only the durable-row arm that refused at 4, because the pickup
// handler nulled ActiveOrderID there and the two slot surfaces had nothing left
// to read. Deleting that clear is what makes all three agree at both ends.
func TestSingleRobotWindow_RefusedAt4_AdmittedAt8OnceThePlacementIsRecorded(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeSingleRobot, "")
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	seedGuardRuntime(t, db, nodeID, claim)

	leg := mkSwapLeg(t, db, nodeID, "uuid-single-mid", BuildSingleSwapSteps(claim), "")
	testutil.MustNoErr(t, db.UpdateOrderStatus(leg.ID, string(protocol.StatusInTransit)), "in_transit")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &leg.ID, nil), "slot")

	assertGuards(t, eng, db, nodeID, node, claim, false,
		"the swap is released and the robot has not reached the press")

	// Step 4: the press pickup. Not a departure — the robot is holding the old
	// bin, the new one is not down yet, and both staging slots are still ours.
	eng.HandleBinPickedUp(leg.UUID, 0, claim.CoreNodeName)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveOrderID == nil || *rt.ActiveOrderID != leg.ID {
		t.Fatal("fixture drift: the press pickup cleared the cell-busy slot. That clear is what opened " +
			"the 4→8 window; the slot must stay set until the leg is terminal or departed")
	}
	o, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "re-read leg")
	if o.Departed {
		t.Fatal("the leg departed at its step-4 press pickup — it is still holding the old bin, has not " +
			"delivered the new one, and both staging slots are still the cell's")
	}
	assertGuards(t, eng, db, nodeID, node, claim, false,
		"single_robot between steps 4 and 8: the old bin is on outbound staging and a second swap's "+
			"step 5 would drop onto it")

	// Step 7: the placement. Core's intermediate dropoff binds the fresh carrier
	// to the cell. The robot is STILL at the cell — this must not release it.
	bindPlacement(t, eng, claim.CoreNodeName)
	assertGuards(t, eng, db, nodeID, node, claim, false,
		"the carrier is down but the robot has not left: the old bin is still on outbound staging")

	// Step 8: the outbound-staging pickup. Every cell position is filled, the
	// placement is on the books, and the robot is driving away.
	eng.HandleBinPickedUp(leg.UUID, 0, claim.OutboundStaging)
	o, err = db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "re-read leg after step 8")
	if !o.Departed {
		t.Fatal("the leg did not depart at its step-8 outbound-staging pickup with its placement recorded")
	}
	if protocol.IsTerminal(o.Status) {
		t.Fatal("fixture drift: the leg must still be non-terminal — that is the whole point")
	}
	assertGuards(t, eng, db, nodeID, node, claim, true,
		"single_robot after step 8: every cell position is filled and the robot is driving away")
}

// TestSingleRobotWindow_StaysRefusedAt8WithoutThePlacement is the case the whole
// fix exists for, and it is the same run as above with one event removed.
//
// The robot has left the cell's nodes and its own step-7 placement is on nobody's
// books: active_bin_id is nil because the leg's press pickup cleared it, and
// Core's intermediate dropoff has not been seen. The cell therefore looks bare
// to every position question, and the ONLY thing that knows a carrier is down is
// the leg's own row. It must stay visible — all three guards refuse, and they
// agree, which is what a second witness bolted onto one guard could not deliver.
func TestSingleRobotWindow_StaysRefusedAt8WithoutThePlacement(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeSingleRobot, "")
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	seedGuardRuntime(t, db, nodeID, claim)

	leg := mkSwapLeg(t, db, nodeID, "uuid-single-unrecorded", BuildSingleSwapSteps(claim), "")
	testutil.MustNoErr(t, db.UpdateOrderStatus(leg.ID, string(protocol.StatusInTransit)), "in_transit")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &leg.ID, nil), "slot")

	eng.HandleBinPickedUp(leg.UUID, 0, claim.CoreNodeName)    // step 4
	eng.HandleBinPickedUp(leg.UUID, 0, claim.OutboundStaging) // step 8, no step-7 record

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveBinID != nil {
		t.Fatalf("fixture drift: active_bin_id = %d, want nil — the window under test is exactly the "+
			"interval where nothing is bound", *rt.ActiveBinID)
	}
	o, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "re-read leg")
	if o.Departed {
		t.Fatal("the leg departed at step 8 with its own placement unrecorded — every reader now believes " +
			"the cell is free, and the next tick mints a bare move into the position this leg just filled")
	}
	if o.CellLeftAt == nil {
		t.Error("cell_left_at was not stamped: the robot HAS left the cell's nodes, and without that half " +
			"recorded the bind has nothing to complete")
	}
	assertGuards(t, eng, db, nodeID, node, claim, false,
		"the robot has left but its placement is unrecorded — the cell's own row is the only witness left")

	// And the escape, so this is not the cell shut forever: the record lands and
	// the departure completes from the other trigger.
	bindPlacement(t, eng, claim.CoreNodeName)
	o, err = db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "re-read leg after the bind")
	if !o.Departed {
		t.Fatal("the placement was recorded and the leg still has not departed — the cell would stay shut " +
			"until this leg went terminal at the supermarket")
	}
	assertGuards(t, eng, db, nodeID, node, claim, true,
		"placement recorded and robot gone: the next swap may be ordered")
}

// bindPlacement drives Core's step-7 intermediate dropoff: the Edge binds the
// arrived carrier to the cell through the real UOPAdjustment handler, which is
// also where settleCellPlacement is wired.
func bindPlacement(t *testing.T, eng *Engine, coreNodeName string) {
	t.Helper()
	eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        placedBinID,
		CoreNodeName: coreNodeName,
		NewRemaining: 40,
		Epoch:        1,
		Bound:        true,
	})
}

// TestCanAcceptOrders_ReasonStringsUnchanged pins the operator-facing half. The
// predicate moved; the sentence an operator is refused with did not, and it
// still names which slot is holding them.
func TestCanAcceptOrders_ReasonStringsUnchanged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobot, "")
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}
	seedGuardRuntime(t, db, nodeID, claim)

	supply, evac := BuildTwoRobotSwapSteps(claim)
	a := mkSwapLeg(t, db, nodeID, "uuid-reason-a", supply, "")
	b := mkSwapLeg(t, db, nodeID, "uuid-reason-b", evac, "")
	testutil.MustNoErr(t, db.UpdateOrderStatus(a.ID, string(protocol.StatusInTransit)), "a in_transit")
	testutil.MustNoErr(t, db.UpdateOrderStatus(b.ID, string(protocol.StatusStaged)), "b staged")

	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &a.ID, nil), "active slot")
	if ok, reason := eng.CanAcceptOrders(nodeID); ok || reason != "active order in progress" {
		t.Errorf("CanAcceptOrders = (%v, %q), want (false, \"active order in progress\")", ok, reason)
	}
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &b.ID), "staged slot")
	if ok, reason := eng.CanAcceptOrders(nodeID); ok || reason != "staged order in progress" {
		t.Errorf("CanAcceptOrders = (%v, %q), want (false, \"staged order in progress\")", ok, reason)
	}
}
