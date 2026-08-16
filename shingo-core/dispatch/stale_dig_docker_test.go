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

// stale_dig_docker_test.go — a dig's plan is written once, and the lane does not
// hold still.
//
// The shape (sim scenario 13b, the owner's two-flows-one-lane what-if): dig A
// packs a blocker into lane B while B's own dig is mid-flight. B's remaining leg
// then has a bin in front of it that is not in B's plan — because B's plan was
// written before it landed — and nothing in B's compound will ever move it. The
// leg parks on a reachability refusal that cannot self-clear, and the demand that
// would plan a dig for the new blocker is the parent, imprisoned in `reshuffling`
// inside this very compound.
//
// Occupancy and a foreign dig do not do this: somebody leaves, the redrive
// re-admits. Reachability is the one refusal whose cause is a FACT ABOUT THE PLAN
// rather than about the moment.
//
// THE WINDOW IS AFTER A LEG PLACES, and the fixtures below reproduce that rather
// than a simpler-looking state that would not exercise the code. While leg 1 is
// in flight its occupancy row refuses leg 2 first (admission asks occupancy
// before reachability), so a walled leg 2 reads as lane-occupied and self-clears.
// The stale case needs leg 1 finished — its blocker parked, its occupancy gone —
// and the wall landing in the gap before leg 2 is admitted.

// staleDigLane builds an ungated 4-deep lane in a group with parking beside it,
// and returns the lane plus its slots shallowest-first.
func staleDigLane(t *testing.T, db *store.DB, prefix string) (grp *nodes.Node, lane *nodes.Node, slots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	for d := 1; d <= 4; d++ {
		depth := d
		s := &nodes.Node{Name: fmt.Sprintf("%s-LANE-S%d", prefix, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		slots = append(slots, s)
	}
	// Parking, well clear of the lane, so a dig is never refused for want of a
	// shuffle slot — the only refusal these tests want is reachability.
	for i := 1; i <= 4; i++ {
		p := &nodes.Node{Name: fmt.Sprintf("%s-PARK-%d", prefix, i), ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(p), "create parking")
	}
	grp, _ = db.GetNode(grp.ID)
	lane, _ = db.GetNode(lane.ID)
	return grp, lane, slots, bp
}

// planStaleDigFixture sets a lane up as [empty, blocker, target], plans the dig
// against it, and then RUNS LEG 1 TO COMPLETION — the blocker lands in parking,
// the leg terminalizes, and its occupancy row goes with it.
//
// That is the state the stale case needs: the dig is live, holds its lane, and
// its next leg is one admission away from entering a corridor that is, for this
// instant, clear.
func planStaleDigFixture(t *testing.T, db *store.DB, d *Dispatcher, prefix string) (parent *orders.Order, lane *nodes.Node, slots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	_, lane, slots, bp = staleDigLane(t, db, prefix)

	blocker := testdb.CreateBinAtNode(t, db, bp.Code, slots[1].ID, prefix+"-BLK")
	target := testdb.CreateBinAtNode(t, db, bp.Code, slots[2].ID, prefix+"-TGT")

	lineNode(t, db, prefix+"-LINE")
	parent = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-parent"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = prefix + "-LINE"
		o.Status = protocol.StatusPending
	})

	if _, pe := d.planner.planBuriedReshuffle(parent, &BuriedError{Bin: target, Slot: slots[2], LaneID: lane.ID}); pe != nil {
		t.Fatalf("fixture: the dig must plan cleanly, got %s: %s", pe.Code, pe.Detail)
	}
	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("fixture: the dig must hold its lane")
	}

	// Leg 1 does its job: the blocker leaves the lane for parking and the leg
	// terminalizes, which releases its occupancy (TerminalizeOrder →
	// reservations.ReleaseByOrder is kind-agnostic).
	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "fixture: list legs")
	if len(legs) < 2 {
		t.Fatalf("fixture: the dig planned %d leg(s), want at least an unbury and a retrieve", len(legs))
	}
	first := legs[0]
	if first.VendorOrderID == "" {
		t.Fatalf("fixture: leg %d was never dispatched (queue_cause %q) — the lane was supposed to be "+
			"clear at plan time", first.ID, first.QueueCause)
	}
	testutil.MustNoErr(t, db.MoveBinClearingStaging(blocker.ID, parkingNodeID(t, db, prefix+"-PARK-1"), false),
		"fixture: leg 1 parks its blocker")
	_, err = db.TerminalizeOrder(first.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "fixture: leg 1 completes")

	return parent, lane, slots, bp
}

// parkingNodeID resolves one of the group's parking nodes by name.
func parkingNodeID(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	n, err := db.GetNodeByDotName(name)
	if err != nil || n == nil {
		t.Fatalf("resolve parking node %s: %v", name, err)
	}
	return n.ID
}

// TestStaleDig_UnclaimedObstruction_DissolvesAndReplans is 13b, and its checker
// is the campaign's: the demand survives with zero human intervention.
//
// MUTATION (verified): make handleStaleDigLeg fall through to the ordinary
// setQueueReason hold instead of dissolving. No leg is cancelled, the parent
// never leaves `reshuffling`, and the "back in the acquiring set" assertion fires
// — which is the wedge, restated as a failing test.
func TestStaleDig_UnclaimedObstruction_DissolvesAndReplans(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, bp := planStaleDigFixture(t, db, d, "SD-DISS")

	// DIG A PACKS INTO THE MOUTH. Nothing in this compound will move it: the
	// blocker list was written before it existed.
	testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-DISS-WALL")

	// Redrive one: what the lane-clearing event fires. The leg is walled, nobody
	// is coming for the wall, so the dig dissolves — legs cancelled, lane
	// released. The parent is deliberately NOT transitioned here; see below.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive the walled compound")

	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs after dissolve")
	sawDissolveMarker := false
	for _, l := range legs {
		if !protocol.IsTerminal(l.Status) {
			t.Errorf("leg %d is %q — a dissolve cancels the whole remaining set, or the re-plan races "+
				"a robot still executing the old plan", l.ID, l.Status)
		}
		if l.Status == protocol.StatusCancelled {
			if l.ErrorDetail != reshuffleDissolveDetail {
				t.Errorf("leg %d cancelled with %q, want the dissolve marker — without it the terminal "+
					"arm reads this as a leg going wrong and fails the parent", l.ID, l.ErrorDetail)
			}
			sawDissolveMarker = true
		}
	}
	if !sawDissolveMarker {
		t.Fatal("no leg was cancelled — the dig was not dissolved, so the walled leg is still waiting " +
			"on a bin nothing will ever move")
	}

	// THE LANE IS FREE. IsLocked is owner-blind, so a lock held past the dissolve
	// parks the re-plan on lane_locked with nothing alive to release it.
	if d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the lane is still locked after the dissolve — planBuriedReshuffle refuses a locked " +
			"lane whoever holds it, including the parent about to re-plan, so this is the wedge rebuilt")
	}

	// Redrive two: what the engine's cancel wiring does on its own goroutine
	// (wiring.go, EventOrderCancelled → AdvanceCompoundOrder). It is a separate
	// call rather than part of the dissolve BECAUSE the parent transition fires
	// the synchronous scanner, and the dissolve is reachable from inside it.
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "the cancel wiring's re-drive")

	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("parent is %q — the demand was terminated because its dig's plan went stale. "+
			"Nothing is wrong with the demand; the lane changed", after.Status)
	}
	if !protocol.IsAcquiring(after.Status) {
		t.Fatalf("parent is %q, want an acquiring status — `reshuffling` is outside IsAcquiring, so a "+
			"parent left there is one the scanner never looks at again", after.Status)
	}

	// AND THE RE-PLAN SEES THE NEW WORLD. The same planner, run again, plans
	// against the lane as it now stands — which is the whole point: the new plan
	// contains the bin that made the old one stale.
	target, err := db.GetBinByLabel("SD-DISS-TGT")
	testutil.MustNoErr(t, err, "reload target")
	if _, pe := d.planner.planBuriedReshuffle(after, &BuriedError{Bin: target, Slot: slots[2], LaneID: lane.ID}); pe != nil {
		t.Fatalf("the re-plan was refused (%s: %s) — the demand survived the dissolve and then could "+
			"not be served, which is the wedge one step further along", pe.Code, pe.Detail)
	}
	replanned, err := db.ListChildOrders(after.ID)
	testutil.MustNoErr(t, err, "list re-planned legs")
	live := 0
	for _, l := range replanned {
		if !protocol.IsTerminal(l.Status) {
			live++
		}
	}
	// The wall at depth 1 plus the retrieve of the target at depth 3. Depth 2 was
	// emptied by the first dig's leg 1 and stays empty — blockers lie where they
	// fall, and the re-plan simply sees fewer of them than the first plan did.
	if live < 2 {
		t.Errorf("the re-plan has %d live leg(s); it must cover the bin dig A landed AND the retrieve. "+
			"A plan that still misses the new bin goes stale again immediately", live)
	}
}

// TestStaleDig_HardClaimedObstruction_Waits is the other arm of the ruling, and
// the one that keeps the fix from thrashing.
//
// A bin with a hard claim has a robot on its way to it. The lane frees itself,
// the existing lane-clearing redrive re-admits the leg, and dissolving would
// throw away a good plan to re-plan a dig for a bin that is seconds from gone.
// Dissolve is never triggered BY a claim — only by the absence of anyone coming.
//
// MUTATION (verified): drop the `if claimed` arm from handleStaleDigLeg so every
// reachability refusal dissolves. The "legs survive" assertion fires — the dig is
// torn down while a robot is already carrying the obstruction out.
func TestStaleDig_HardClaimedObstruction_Waits(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, slots, bp := planStaleDigFixture(t, db, d, "SD-WAIT")

	// The wall lands — but a robot is already coming for it.
	wall := testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-WAIT-WALL")
	hauler := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-wait-hauler"
		o.Status = protocol.StatusDispatched
	})
	testdb.ClaimBinForTest(t, db, wall.ID, hauler.ID)

	before, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs before")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive the walled compound")

	// THE DIG SURVIVES.
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload parent")
	if after.Status != protocol.StatusReshuffling {
		t.Fatalf("parent is %q, want %q — the obstruction is leaving under its own power, so the dig "+
			"waits for it rather than re-planning around it", after.Status, protocol.StatusReshuffling)
	}
	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs after")
	if len(legs) != len(before) {
		t.Fatalf("leg count changed %d → %d", len(before), len(legs))
	}
	for _, l := range legs {
		if l.Status == protocol.StatusCancelled {
			t.Errorf("leg %d was cancelled — the dig was torn down over a bin a robot is already "+
				"carrying out", l.ID)
		}
	}
	if !d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane was released while the dig is still live — another flow can now pack into it")
	}

	// AND THEN IT PROCEEDS. The obstruction leaves the lane exactly as the robot
	// carrying it would take it out; the next redrive admits the held leg. Without
	// this half the "wait" arm is indistinguishable from a stall.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(wall.ID, parkingNodeID(t, db, "SD-WAIT-PARK-2"), false),
		"the hauler takes the wall out")
	_, err = db.TerminalizeOrder(hauler.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "hauler completes")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "redrive after the lane cleared")

	resumed, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list legs after the clear")
	held := 0
	for _, l := range resumed {
		if l.Status == protocol.StatusPending {
			held++
		}
	}
	if held != 0 {
		t.Errorf("%d leg(s) still pending after the obstruction left — the wait had no releaser, "+
			"which makes it a stall wearing a queue reason", held)
	}
}

// window2Recheck is the window-2 door, driven as three production calls in the
// order fulfillment/scanner.go dispatchHeldBin makes them.
//
// It is not a re-implementation: dispatchHeldBin's own body between the admit and
// the dig is the scanner's bookkeeping (its status move, its queue reason), and
// the DECISION is these three calls. They live in this package; the scanner is a
// package above and its harness has no database. So the composition is driven
// here, where the real store is, rather than against a fake one tier up.
//
// It fails the test if admission did NOT say buried, because every assertion after
// it would then be about a door that never opened.
func window2Recheck(t *testing.T, d *Dispatcher, db *store.DB, order *orders.Order) error {
	t.Helper()
	sourceNode, err := db.GetNodeByDotName(order.SourceNode)
	testutil.MustNoErr(t, err, "resolve the held-bin order's source")
	destNode, err := db.GetNodeByDotName(order.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the held-bin order's destination")

	admitted, cause, _, err := d.AcquireLanesForOrder(order, sourceNode, destNode, EntryHeldBin)
	testutil.MustNoErr(t, err, "held-bin admission")
	if admitted {
		t.Fatal("admission let the held-bin order through — its bin is behind a wall, and the whole " +
			"of window 2 is that the held-bin caller never called the finder and so nothing else looked")
	}
	if cause != CauseLaneTargetBuried {
		t.Fatalf("held-bin admission refused with %q, want %q — only the buried cause routes to a dig; "+
			"every other refusal is a park with no excavation behind it", cause, CauseLaneTargetBuried)
	}
	buried, err := d.BuriedForHeldBin(order)
	testutil.MustNoErr(t, err, "describe the dig the refusal asks for")
	return d.PlanBuriedReshuffle(order, buried)
}

// TestStaleDig_Window2Dig_DissolvesReplansAndCompletes is catalog row 5.6: one
// self-heal window composed with another.
//
// The sibling tests above prove the dissolve on a dig planned from a BuriedError
// the finder produced. This one's dig comes through the OTHER door — the held-bin
// re-check (3326c1bb, window 2), which is the door with no finder behind it: the
// order claimed its bin on an earlier tick, a store buried it in between, and
// admission is the only thing that ever looks. That dig is a different object with
// a different origin and it had never been put under a dissolve.
//
// AND IT FOLLOWS THE RE-PLAN TO THE END. Neither this file's dissolve test nor
// window4_docker_test.go's dig test does: they stop at "plannable" and "the
// planner accepts it". Plannable is not served, and the whole claim of the
// self-heal windows is that the DEMAND is.
//
// The second dig re-enters through the SAME door, which is the composition rather
// than a convenience: after the dissolve the order is back in the acquiring set
// still holding its bin, so the scanner routes it to dispatchHeldBin again and
// admission refuses it again — this time for the wall, not the original blocker.
//
// ── THE ASSERTION THAT USED TO BE MISSING, AND NOW IS THE POINT ───────────
//
// This test stopped short of the parent's status, because the parent did not
// reach `confirmed` and the reason was a defect (F-07): digWasDissolved read
// ListChildOrders, which returns EVERY generation of children a parent has ever
// had. A re-plan writes its legs under the same parent, so the FIRST dig's
// cancelled-with-marker leg was still in the set when the SECOND dig finished
// cleanly — unanimity held, digWasDissolved answered true a second time, and the
// terminal arm filed a completed dig as another dissolve and returned the parent
// to `queued`, where the scanner retried a retrieve for a bin that had already
// left. Work done, demand never closed and never failed.
//
// RULED 2026-08-10, option (a), episode-grained: a superseded generation is a
// CLOSED CHAPTER. The parent's completion arithmetic reads only the open one; the
// demand's ledger keeps everything. compoundGenerations is the one spelling of
// that split and all four current-state readers take it. The assertion is now the
// last thing this test does.
//
// THREE MUTATIONS, all run 2026-08-10.
//
//  1. Make handleStaleDigLeg fall through to the ordinary setQueueReason hold
//     instead of dissolving — delete its `return d.dissolveCompound(...)` tail.
//     No leg is cancelled, the parent never leaves `reshuffling`, and the
//     dissolve-marker assertion fires. That is the wedge this row exists for: the
//     parent is imprisoned inside the very compound that would have to plan the
//     dig for the bin now in front of it.
//  2. Restore digWasDissolved's whole-family scan (the pre-ruling body). This
//     test fires on the LAST assertion — `the demand is "queued" after BOTH digs
//     finished and its bin reached the line` — which is F-07 exactly.
//     TestStaleDig_UnclaimedObstruction_DissolvesAndReplans stays GREEN under the
//     same mutation, which is the evidence that it stops one step short and why
//     this assertion had to live here.
//  3. Neuter the closed-chapter filter in compoundGenerations (`if false && ...`).
//     Three tests fire, and by a different route: with nothing closed, the
//     marker-cancelled legs make hasFailedOrCancelled true and the parent takes
//     the FAILURE cascade instead. Worth recording because it shows the scoping is
//     load-bearing for the failure arm too, not only for the dissolve arm.
func TestStaleDig_Window2Dig_DissolvesReplansAndCompletes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, lane, slots, bp := staleDigLane(t, db, "SD-W2")

	// [empty, blocker, HELD, empty] — the held bin was reachable when the order
	// claimed it and a store put something in front of it afterwards.
	testdb.CreateBinAtNode(t, db, bp.Code, slots[1].ID, "SD-W2-BLK")
	held := testdb.CreateBinAtNode(t, db, bp.Code, slots[2].ID, "SD-W2-HELD")
	line := lineNode(t, db, "SD-W2-LINE")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-w2-demand"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.SourceNode = slots[2].Name
		o.DeliveryNode = line.Name
		o.Status = protocol.StatusSourcing
	})
	testdb.ReserveBin(t, db, order.ID, held.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(order.ID, held.ID), "stamp the held bin")
	order, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the held-bin order")

	// ── THE WINDOW-2 DIG ──────────────────────────────────────────────────────
	testutil.MustNoErr(t, window2Recheck(t, d, db, order), "the window-2 dig must plan")
	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the window-2 dig does not hold its lane — the dissolve below would then be about " +
			"nothing, and every lane guard downstream has no owner to name")
	}

	legs := legsOf(t, db, order.ID)
	if len(legs) != 2 {
		t.Fatalf("the window-2 dig planned %d leg(s), want an unbury and the retrieve", len(legs))
	}
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig's unbury never went out (queue_cause %q) — the lane was clear at plan time",
			legs[0].QueueCause)
	}
	landLeg(t, d, db, legs[0])

	// ── AND THEN IT GOES STALE ────────────────────────────────────────────────
	// Another flow packs the mouth. Nothing in this compound will ever move it: the
	// blocker list was written before it existed.
	testdb.CreateBinAtNode(t, db, bp.Code, slots[0].ID, "SD-W2-WALL")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(order.ID), "redrive the walled window-2 compound")

	after, err := db.ListChildOrders(order.ID)
	testutil.MustNoErr(t, err, "list legs after the redrive")
	sawDissolveMarker := false
	for _, l := range after {
		if l.Status == protocol.StatusCancelled && l.ErrorDetail == reshuffleDissolveDetail {
			sawDissolveMarker = true
		}
	}
	if !sawDissolveMarker {
		t.Fatal("no leg carries the dissolve marker — a dig reached through the held-bin door is a dig " +
			"like any other, and its walled leg is waiting on a bin nothing will move")
	}
	if d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the lane is still locked after the dissolve — the re-plan below refuses a locked lane " +
			"whoever holds it, including the parent about to re-plan")
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(order.ID), "the cancel wiring's re-drive")

	replanning, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the parent after the dissolve")
	if protocol.IsTerminal(replanning.Status) {
		t.Fatalf("the demand is %q — it was terminated because its dig's plan went stale. Nothing is "+
			"wrong with the demand; the lane changed", replanning.Status)
	}
	if !protocol.IsAcquiring(replanning.Status) {
		t.Fatalf("the demand is %q, want an acquiring status — the scanner never looks at an order "+
			"outside {queued, sourcing} again", replanning.Status)
	}
	// IT STILL HOLDS ITS BIN, which is what makes the second pass a HELD-BIN pass
	// rather than a fresh find. Losing the hold here would send the order back
	// through the finder, and the finder is owner-blind.
	if replanning.BinID == nil || *replanning.BinID != held.ID {
		t.Fatalf("the demand's bin_id is %v after the dissolve, want the bin it still holds (%d) — the "+
			"dissolve tore down the dig AND the demand's grip on what the dig was for",
			replanning.BinID, held.ID)
	}

	// ── THE SECOND DIG, THROUGH THE SAME DOOR ─────────────────────────────────
	testutil.MustNoErr(t, window2Recheck(t, d, db, replanning), "the re-planned window-2 dig")

	replanned := legsOf(t, db, replanning.ID)
	live := make([]*orders.Order, 0, len(replanned))
	for _, l := range replanned {
		if !protocol.IsTerminal(l.Status) {
			live = append(live, l)
		}
	}
	// The wall at depth 1 plus the retrieve of the held bin at depth 3. Depth 2 was
	// emptied by the first dig and stays empty — blockers lie where they fall.
	if len(live) != 2 {
		t.Fatalf("the re-plan has %d live leg(s), want the wall's unbury and the retrieve. A plan that "+
			"still misses the new bin goes stale again immediately", len(live))
	}

	// ── AND IT COMPLETES ──────────────────────────────────────────────────────
	for i, leg := range live {
		if leg.VendorOrderID == "" {
			testutil.MustNoErr(t, d.AdvanceCompoundOrder(replanning.ID), "re-drive onto the next leg")
			reloaded, rErr := db.GetOrder(leg.ID)
			testutil.MustNoErr(t, rErr, "reload leg")
			leg = reloaded
		}
		if leg.VendorOrderID == "" {
			t.Fatalf("re-planned leg %d (index %d) never went out (queue_cause %q) — the lane is the "+
				"dig's own and nothing else is in it", leg.ID, i, leg.QueueCause)
		}
		landLeg(t, d, db, leg)
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(replanning.ID), "close the re-planned compound")

	done, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload the demand at the end")
	if done.Status == protocol.StatusFailed || done.Status == protocol.StatusCancelled {
		t.Fatalf("the demand is %q. Two digs, one dissolve and a wall are all congestion; none of them "+
			"is a fault, and the promise this row makes is that the demand is never terminated for "+
			"any of it", done.Status)
	}

	// ── AND THE DEMAND CLOSES ─────────────────────────────────────────────────
	//
	// THE ASSERTION F-07 LIVED BEHIND. "Not failed and not cancelled" was true of
	// the bug: the parent came back QUEUED, which is neither, so every check above
	// this line passed while the demand was never closing at all. It then went
	// round the scanner forever, retrieving a bin that had already left — the work
	// done, the demand never served and never failed. Silent, and on the branch's
	// headline feature.
	//
	// Terminating in the RIGHT terminal is the whole of the fix, so it is asserted
	// as its own step rather than folded into the negative above.
	if !protocol.IsTerminal(done.Status) {
		t.Fatalf("the demand is %q after BOTH digs finished and its bin reached the line — it never "+
			"closed.\n"+
			"This is F-07: the completion arithmetic read every child the parent ever had, saw the "+
			"FIRST dig's marker-cancelled legs still sitting in the list, and filed a clean second dig "+
			"as another dissolve. The parent goes back to the acquiring set and the scanner retries a "+
			"retrieve for a bin that already left. Nothing fails, nothing completes, forever.\n"+
			"A superseded generation is a closed chapter: the completion arithmetic reads the OPEN "+
			"one (compoundGenerations).", done.Status)
	}
	if done.Status != protocol.StatusConfirmed {
		t.Errorf("the demand terminated as %q, want %q — its bin was delivered, so any other terminal "+
			"is the wrong account of what happened", done.Status, protocol.StatusConfirmed)
	}
	atLine, err := db.GetBin(held.ID)
	testutil.MustNoErr(t, err, "reload the held bin")
	if atLine.NodeID == nil || *atLine.NodeID != line.ID {
		t.Errorf("the held bin is at node %v, want the line %d — the order is confirmed and its bin "+
			"never arrived", atLine.NodeID, line.ID)
	}

	// THE LEDGER IS CLEAN after two digs and a dissolve.
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is still locked after the second dig completed")
	}
	if occ, _ := reservations.OccupantsOf(db.DB, lane.ID); len(occ) != 0 {
		t.Errorf("lane %s still has occupants %v", lane.Name, occ)
	}
}

// TestStaleDig_DissolveMarkerIsWhatSeparatesItFromAFailure is the discriminator
// under test on its own, because the two outcomes are one string apart and the
// wrong one terminates demand.
func TestStaleDig_DissolveMarkerIsWhatSeparatesItFromAFailure(t *testing.T) {
	t.Parallel()
	cancelled := func(detail string) *orders.Order {
		return &orders.Order{Status: protocol.StatusCancelled, ErrorDetail: detail}
	}

	if !digWasDissolved([]*orders.Order{cancelled(reshuffleDissolveDetail)}) {
		t.Error("a set cancelled by the dissolve must read as dissolved")
	}
	if digWasDissolved([]*orders.Order{cancelled("operator terminated it")}) {
		t.Error("an operator cancel must NOT read as a dissolve — the compound really did stop, and " +
			"returning its parent to the acquiring set would resurrect work a human cancelled")
	}
	if digWasDissolved([]*orders.Order{
		cancelled(reshuffleDissolveDetail),
		{Status: protocol.StatusFailed},
	}) {
		t.Error("a FAILED leg vetoes the dissolve reading — something else went wrong in the same " +
			"moment, and the failure cascade is the honest disposition for that")
	}
	if digWasDissolved([]*orders.Order{{Status: protocol.StatusConfirmed}}) {
		t.Error("a cleanly finished set is not a dissolve")
	}
}

// TestStaleDig_DissolvedFolderIsCancelledNotRequeued is §R.93.1.
//
// ── THE THIRD KIND ────────────────────────────────────────────────────────
//
// The dissolve arm returns a parent to the acquiring set and says so in its own
// words: "Reshuffling → Queued for BOTH kinds", meaning a plain parent and a
// coordinated one. Both really are demands and really can re-plan.
//
// There is a THIRD kind and it had no arm. A SERVICE DIG parent is a FOLDER: no
// bin, no payload, no demand of its own, and — once its plan is gone — no
// destination either. Handed to the fulfillment scanner it is treated as a
// demand and sourced for.
//
// ── WHAT THAT COST, MEASURED ──────────────────────────────────────────────
//
// Rig, 2026-08-14, dig 51:
//
//	DISSOLVING dig 51 — child 52 is walled by a bin no order is coming for
//	engine: order 51 queued for payload ""
//	fulfillment: dest node "" not found for order 51      ← then ~1/sec, forever
//
// It sourced a bin and soft-held it away from real demands, parked on
// dest-node-unresolved — a named wait nothing in the world can end — and retried
// for the rest of the run.
//
// AND THE CORPSE BLOCKED ITS OWN REPLACEMENT, which is what actually stops the
// plant. A phantom stays NON-TERMINAL and keeps its dig_target_node, so arm 3's
// one-dig-per-episode gate counts it as a live excavation and refuses to raise
// another dig for that episode. Order 29 — the gate dweller the dig existed to
// free — sat at the mark for the whole 17-minute window.
//
// ── THE ASSERTION IS TERMINALITY, NOT THE STATUS NAME ─────────────────────
//
// What frees arm 3 is the parent leaving the non-terminal set; `cancelled` is
// how it gets there. So the test asserts terminal first and the marker second —
// a future disposition that terminalizes differently still passes the thing that
// matters, and the message says which property broke.
//
// MUTATION (verified): restore the unconditional Queue and this fails on the
// terminality assertion, naming arm 3.
func TestStaleDig_DissolvedFolderIsCancelledNotRequeued(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A SERVICE DIG parent: dig_target_node set, which is the one place that
	// column is ever written and the only shape that owns no retrieve of its own.
	folder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-folder-dissolve"
		o.OrderType = OrderTypeMove
		o.Status = protocol.StatusReshuffling
		o.DigTargetNode = "LSD_008"
	})
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "sd-folder-dissolve-leg"
		o.OrderType = OrderTypeMove
		o.ParentOrderID = &folder.ID
		o.Sequence = 1
		o.Status = protocol.StatusPending
	})
	// Cancel the leg carrying the dissolve marker — the state a dissolve leaves.
	_, cerr := db.TerminalizeOrder(leg.ID, protocol.StatusCancelled, reshuffleDissolveDetail)
	testutil.MustNoErr(t, cerr, "cancel the leg as a dissolve")

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(folder.ID), "re-drive the dissolved folder")

	after, err := db.GetOrder(folder.ID)
	testutil.MustNoErr(t, err, "reload the folder")

	// (a) TERMINAL. This is the property arm 3 reads.
	if !protocol.IsTerminal(after.Status) {
		t.Fatalf("the dissolved dig folder is %q — NOT terminal. arm 3's one-dig-per-episode gate "+
			"counts any non-terminal order carrying a dig_target_node as a live excavation, so this "+
			"row now refuses every replacement dig for its episode and the dweller it was raised for "+
			"waits with its only releaser already dead.", after.Status)
	}

	// (b) AND IT MUST NOT BE ACQUIRING, which is the specific wrong answer.
	if protocol.IsAcquiring(after.Status) {
		t.Errorf("the folder is %q — in the acquiring set. The fulfillment scanner will source a bin "+
			"for an order that carries no payload and has no destination: 'queued for payload \"\"' "+
			"followed by 'dest node \"\" not found' once a second, forever.", after.Status)
	}

	// (c) The marker says WHICH ending this was. soakstat counts dissolves off the
	// demand marker; a folder cancelled under that string would be reported as a
	// demand re-planning, which is the conflation this ruling is about.
	if after.Status == protocol.StatusCancelled && after.ErrorDetail != reshuffleDissolveFolderDetail {
		t.Errorf("the folder was cancelled with %q, want the folder marker — the two endings are "+
			"different facts and a reader counting dissolves has to tell them apart",
			after.ErrorDetail)
	}
}
