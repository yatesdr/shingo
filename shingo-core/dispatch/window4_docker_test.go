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
	"shingocore/store/payloads"
)

// window4_docker_test.go — a complex order's reserved bin gets buried, and the
// order stops waiting forever on something it can no longer reach.
//
// Window 2 wired this for the PLAIN held-bin path (3326c1bb): the scanner asks
// reachability, a buried verdict becomes a dig, the order resumes. The complex
// path never had it, and three separate things kept anyone from noticing:
//
//   - the burial guard permits it. Hard claims only, by ruling — a soft hold is a
//     plan, and plans recalculate.
//   - widenSupplyPickups SKIPS a need the order already holds. Rightly: the
//     finder is owner-blind and would park the order on its own bin. But "I hold
//     it" and "I can still get it" are different questions.
//   - admitComplexLanes deliberately skips reachability, because a buried refusal
//     there has no dig wired to it.
//
// So the swap waited on a bin behind a wall. The recalculation is the fix: the
// reserve is a plan, and a plan that is out of date gets redone.

// window4Dispatcher wires a REAL resolver, which newTestDispatcher does not.
//
// It matters here and nowhere else in this file's setup: a fungible substitute
// for a group-anchored need is found through the finder's NGRP tier, and that
// tier self-guards on `f.resolver != nil`. With a nil resolver the group falls
// through to the node-local tier, which reads the group node itself, finds no
// bins on it, and reports finder-node-empty — so every recalculation would look
// like "no substitute" and dig, and the substitute arm would be untestable while
// appearing to pass its dig sibling.
func window4Dispatcher(t *testing.T, db *store.DB) *Dispatcher {
	t.Helper()
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	return NewDispatcher(db, testdb.NewSuccessBackend(), &mockEmitter{}, "core", "shingo.dispatch",
		&DefaultResolver{DB: db, DebugLog: d.dbg})
}

// window4Lane builds an NGRP with one 3-deep lane plus a free direct child, and
// returns the pieces a complex need is built from.
func window4Lane(t *testing.T, db *store.DB, prefix string) (grp, lane *nodes.Node, slots []*nodes.Node, spare *nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	lane = &nodes.Node{Name: prefix + "-LANE", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	for d := 1; d <= 3; d++ {
		depth := d
		s := &nodes.Node{Name: fmt.Sprintf("%s-LANE-S%d", prefix, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		slots = append(slots, s)
	}
	// A direct child of the group: reachable by construction, which is what makes
	// it a candidate substitute and also a parking spot for a dig.
	spare = &nodes.Node{Name: prefix + "-SPARE", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(spare), "create spare")
	extra := &nodes.Node{Name: prefix + "-SPARE2", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(extra), "create second spare")

	grp, _ = db.GetNode(grp.ID)
	lane, _ = db.GetNode(lane.ID)
	return grp, lane, slots, spare, bp
}

// TestWindow4_FungibleNeedBuried_SourcesASubstitute is the cheap arm, and the
// reason the ruling is "recalculate first, dig only if you must".
//
// The order softly holds a bin that is now walled in. Another bin of the same
// payload is sitting in the open. Shopping it is faster than excavating, and the
// reserve was only ever a plan.
//
// THE GROUP IS SET TO COST, and that is the substance rather than fixture
// convenience: the recalculation does not invent a second sourcing policy, it
// re-asks the one the plant configured. COST means "oldest ACCESSIBLE bin,
// reshuffle only when none is accessible", so under it an available substitute
// wins. FIFO answers differently on purpose, and its answer is pinned in the
// sibling test below.
//
// MUTATION (verified): make heldNeedUnreachable always return (nil, nil). The
// step is never rewritten and the order keeps a hold on the buried bin — the
// assertion on the rewritten node fires.
func TestWindow4_FungibleNeedBuried_SourcesASubstitute(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, spare, bp := window4Lane(t, db, "W4-SUB")
	testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, "retrieve_algorithm", "COST"), "set COST")
	d := window4Dispatcher(t, db)

	// The order's reserved bin sits at depth 2, reachable when it reserved.
	held := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "W4-SUB-HELD")
	// A fungible alternative, out in the open.
	substitute := createTestBinAtNode(t, db, bp.Code, spare.ID, "W4-SUB-ALT")

	order := mkComplexOrder(t, db, "w4-sub", grp.Name, "", "W4-SUB-LINE", bp.Code,
		[]resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}})
	testdb.ReserveBin(t, db, order.ID, held.ID)

	// A store buries it. The guard permits this: the hold is soft.
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "W4-SUB-WALL")

	steps := []resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}}
	out, changed, hold := d.widenSupplyPickups(order, steps)

	if hold != nil {
		t.Fatalf("the order was held (%v) — a fungible need with an unburied alternative should "+
			"re-source, not wait and not dig", hold.QueueCause)
	}
	if !changed {
		t.Fatal("nothing changed — the step still points at a bin behind a wall")
	}
	if out[0].Node != spare.Name {
		t.Fatalf("step node = %s, want %s (the substitute). The reserve is a plan; when the plan "+
			"stops being reachable the cheapest correction is another bin, not an excavation",
			out[0].Node, spare.Name)
	}

	// THE STALE HOLD IS GONE. Leaving it would mean the order holds two bins for
	// one need — the second reserved by the re-resolve, the first still on the
	// books pointing at something walled in.
	rows, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	for _, r := range rows {
		if r.BinID == held.ID {
			t.Errorf("the order still holds the buried bin %d (%s) — one need, two books", held.ID, r.State)
		}
	}
	_ = substitute
}

// TestWindow4_NoSubstitute_TriggersTheDig is the arm that had no answer at all.
//
// Nothing else of the payload exists, so recalculating cannot help: the bin the
// order needs is the buried one. It gets the same buried-bin planner the plain
// path uses, and the parent resumes after — which is exactly what window 2 does,
// reached from the path that never reached it.
//
// MUTATION (verified): return (false, nil) instead of the reshuffle result. The
// order neither digs nor re-sources; the hold assertion fires and the swap is
// back to waiting on a walled bin forever.
func TestWindow4_NoSubstitute_TriggersTheDig(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := window4Lane(t, db, "W4-DIG")
	d := window4Dispatcher(t, db)

	// The ONLY bin of this payload, and it is about to be walled in.
	held := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "W4-DIG-HELD")

	order := mkComplexOrder(t, db, "w4-dig", grp.Name, "", "W4-DIG-LINE", bp.Code,
		[]resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}})
	testdb.ReserveBin(t, db, order.ID, held.ID)

	// The wall is a DIFFERENT payload, so it can never be mistaken for a substitute.
	other := &payloads.Payload{Code: "W4-DIG-OTHER"}
	testutil.MustNoErr(t, db.CreatePayload(other), "create other payload")
	createTestBinAtNode(t, db, other.Code, slots[0].ID, "W4-DIG-WALL")

	steps := []resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}}
	_, _, hold := d.widenSupplyPickups(order, steps)

	if hold == nil {
		t.Fatal("no hold returned — with no substitute anywhere, the order must ask for a dig; " +
			"otherwise it waits on a bin behind a wall with nothing coming to move it")
	}
	if MapFinderOutcome(*hold) != OutcomeReshuffle {
		t.Fatalf("outcome = %v, want OutcomeReshuffle — the caller routes exactly that to "+
			"handleComplexBuriedOnReplay, the same planner the plain path uses",
			MapFinderOutcome(*hold))
	}
	if hold.Buried == nil || hold.Buried.Bin.ID != held.ID {
		t.Fatalf("the dig names bin %v, want the held bin %d", hold.Buried, held.ID)
	}
	if hold.Buried.LaneID != lane.ID {
		t.Errorf("dig lane = %d, want %d", hold.Buried.LaneID, lane.ID)
	}

	// AND THE PLANNER ACCEPTS IT. A reshuffle result the planner then refuses
	// would be a wait wearing a dig's clothes.
	if _, pe := d.planner.planBuriedReshuffle(order, hold.Buried); pe != nil {
		t.Fatalf("the dig this window asks for cannot be planned (%s: %s)", pe.Code, pe.Detail)
	}
}

// TestWindow4_ReachableHeldNeedIsUntouched is the narrowness assertion, and it
// guards the skip the fix is built on top of.
//
// The own-hold skip exists because the finder is owner-blind: re-resolving a need
// the order already holds would reject its own bin and park it on the resource it
// is holding, a self-park with no exit. The reachability question must therefore
// change NOTHING when the answer is "still reachable" — no release, no re-resolve,
// no rewrite.
func TestWindow4_ReachableHeldNeedIsUntouched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, _, bp := window4Lane(t, db, "W4-OK")
	d := window4Dispatcher(t, db)

	// At the mouth: nothing in front of it.
	held := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "W4-OK-HELD")
	createTestBinAtNode(t, db, bp.Code, slots[2].ID, "W4-OK-DECOY")

	order := mkComplexOrder(t, db, "w4-ok", grp.Name, "", "W4-OK-LINE", bp.Code,
		[]resolvedStep{{Action: protocol.ActionPickup, Node: slots[0].Name, Group: grp.Name}})
	testdb.ReserveBin(t, db, order.ID, held.ID)

	steps := []resolvedStep{{Action: protocol.ActionPickup, Node: slots[0].Name, Group: grp.Name}}
	out, changed, hold := d.widenSupplyPickups(order, steps)

	if hold != nil {
		t.Fatalf("a reachable held need was held (%v) — this is the self-park the own-hold skip "+
			"exists to prevent", hold.QueueCause)
	}
	if changed || out[0].Node != slots[0].Name {
		t.Errorf("the step moved to %s — a reachable hold must be left completely alone", out[0].Node)
	}
	rows, err := db.ListReservationsByOrder(order.ID)
	testutil.MustNoErr(t, err, "list reservations")
	found := false
	for _, r := range rows {
		if r.BinID == held.ID {
			found = true
		}
	}
	if !found {
		t.Error("the order's hold on its still-reachable bin was released — the recalculation fired " +
			"when nothing was wrong, and the order now has to win a race for a bin it already had")
	}
}

// TestWindow4_SiblingGateStillHoldsThroughARecalc is challenge hook 2, asked as a
// test rather than assumed.
//
// A two-robot swap's evac leg must not pull the line's bin until its supply
// sibling has secured one (swapLegHeld, the ALN_003 starvation fix). The
// recalculation releases and re-takes the supply's hold, so the question is
// whether the gate can see a half-recalculated supply and let the evac go.
//
// It cannot, and the reason is structural rather than lucky: swapLegHeld reads
// the SIBLING's claim state, and the recalc runs inside one widen pass on the
// supply's own goroutine — it never publishes an intermediate state the gate
// could sample, because a released hold and a re-taken one are both just "not
// yet claimed" to the gate, which is the state it already holds the evac for.
func TestWindow4_SiblingGateStillHoldsThroughARecalc(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, spare, bp := window4Lane(t, db, "W4-SWAP")
	testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, "retrieve_algorithm", "COST"), "set COST")
	d := window4Dispatcher(t, db)
	line := lineNode(t, db, "W4-SWAP-LINE")

	held := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "W4-SWAP-HELD")
	createTestBinAtNode(t, db, bp.Code, spare.ID, "W4-SWAP-ALT")
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "W4-SWAP-WALL") // buries the held bin

	supply := mkComplexOrder(t, db, "w4-swap-supply", grp.Name, line.Name, line.Name, bp.Code,
		[]resolvedStep{
			{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name},
			{Action: protocol.ActionDropoff, Node: line.Name},
		})
	testdb.ReserveBin(t, db, supply.ID, held.ID)

	// The evac: takes the line's bin and puts none back, which is what makes it
	// the leg that can strand the line.
	evac := mkComplexOrder(t, db, "w4-swap-evac", line.Name, line.Name, spare.Name, bp.Code,
		[]resolvedStep{
			{Action: protocol.ActionPickup, Node: line.Name},
			{Action: protocol.ActionDropoff, Node: spare.Name},
		})
	if _, err := db.LinkOrderSiblingsByEdgeUUID(evac.EdgeUUID, supply.EdgeUUID); err != nil {
		t.Fatalf("link swap siblings: %v", err)
	}
	evac, err := db.GetOrder(evac.ID)
	testutil.MustNoErr(t, err, "reload evac")

	evacSteps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: spare.Name},
	}
	if held, _ := d.swapLegHeld(evac, evacSteps); !held {
		t.Fatal("fixture: the evac must be held before the recalc, or this proves nothing")
	}

	// The supply recalculates its buried need.
	supplySteps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	if _, _, hold := d.widenSupplyPickups(supply, supplySteps); hold != nil {
		t.Fatalf("the supply should have re-sourced to the spare, got hold %v", hold.QueueCause)
	}

	// THE EVAC IS STILL HELD. The supply has re-planned but has not claimed, and
	// an evac released against an unclaimed supply is the line stranded.
	if stillHeld, reason := d.swapLegHeld(evac, evacSteps); !stillHeld {
		t.Fatalf("the evac was released during the supply's recalculation (%q) — it would pull the "+
			"line's bin with nothing coming to replace it", reason)
	}
}

// TestWindow4_FIFOStillDigsForTheOlderBin is the other half of "recalculate", and
// it is a property rather than a limitation.
//
// The recalculation does not decide which bin to source — it re-asks the group,
// and the group answers with whatever policy the plant configured. Under FIFO
// that policy is oldest-first INCLUDING buried ones: a buried bin older than
// every accessible one is worth digging for, which is exactly what FIFO means and
// is why it clears skipBuriedIfAccessible.
//
// So a fungible substitute does NOT automatically win. It wins when the group
// says it should. Pinned because the tempting "fix" — prefer any reachable bin
// here — would be a second sourcing policy living in the recalculation, silently
// overriding the configured one for exactly the orders that had bad luck.
func TestWindow4_FIFOStillDigsForTheOlderBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, _, slots, spare, bp := window4Lane(t, db, "W4-FIFO") // default algorithm: FIFO
	d := window4Dispatcher(t, db)

	// The held bin is created FIRST, so it is the oldest — the one FIFO wants.
	held := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "W4-FIFO-HELD")
	createTestBinAtNode(t, db, bp.Code, spare.ID, "W4-FIFO-NEWER")

	order := mkComplexOrder(t, db, "w4-fifo", grp.Name, "", "W4-FIFO-LINE", bp.Code,
		[]resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}})
	testdb.ReserveBin(t, db, order.ID, held.ID)
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "W4-FIFO-WALL")

	steps := []resolvedStep{{Action: protocol.ActionPickup, Node: slots[1].Name, Group: grp.Name}}
	_, _, hold := d.widenSupplyPickups(order, steps)

	if hold == nil {
		t.Fatal("the recalculation took the newer accessible bin under FIFO — that overrides the " +
			"group's configured sourcing policy from inside a recovery path, which is a second " +
			"policy nobody asked for")
	}
	if MapFinderOutcome(*hold) != OutcomeReshuffle {
		t.Fatalf("outcome = %v, want OutcomeReshuffle — FIFO wants the oldest bin and is willing to "+
			"dig for it", MapFinderOutcome(*hold))
	}
}
