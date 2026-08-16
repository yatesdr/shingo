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

// cross_flow_race_docker_test.go — scenario 13a, the race B WINS.
//
// Two flows converge on one lane. Dig A's excavation needs somewhere to park a
// blocker and the only place left is a slot in lane B; lane B is meanwhile being
// dug for its own demand. 13b (stale_dig_docker_test.go) is what happens when A's
// part lands FIRST and B's plan goes stale. This is the other branch: B's leg gets
// in first, does its work, and A's part lands afterwards.
//
// The two guards that make that branch safe are each pinned already, SEPARATELY:
// the D83a shuffle-slot guard by TestFindShuffleSlots_TwoDigsMustNotShareASlot
// (reshuffle_shuffle_slot_test.go), and the one-inside rule by
// TestCompound_LaneGateHoldsWhileOccupied (compound_concurrency_test.go). Neither
// says anything about the other, and neither observes an INTERLEAVING — they each
// assert one fact at one moment. What is untested is the property the catalog
// actually states: one-inside THROUGHOUT, while both flows are live and stepping
// over each other.
//
// So the invariant here is not checked at the end. It is checked after every
// production call, and a scenario that only ever satisfies it at the start and the
// finish fails.
//
// NO GOROUTINES, and that is deliberate — the same argument
// TestCompound_RefusedSourcingClaimDoesNotDispatch makes (lane_occupancy_test.go).
// The interleaving is DRIVEN: dig B plans and dispatches, then dig A plans against
// the lane B has already taken. That is the state real concurrency produces, and it
// is the state the guards are written against. A raced version would reproduce it
// sometimes and read as flaky when it caught the bug.

// twoDigsOneGroup builds the geometry 13a needs — two lanes that must compete for
// the same parking, with lane B's mouth slot as the only fallback:
//
//	GRP
//	├── LANE-A: A1 (blocker) · A2 (target)          ← dig A's excavation
//	├── LANE-B: B1 (EMPTY) · B2 (blocker) · B3 (target)  ← dig B's excavation
//	└── PARK (the group's only direct-child parking)
//
// The shape is what forces the convergence. PARK is a single node, so the second
// dig to plan cannot have it; lane B's B1 is empty and at the mouth, so it is the
// one slot findShuffleSlots' second pass can offer instead — which puts dig A's
// blocker on a collision course with the lane dig B owns.
func twoDigsOneGroup(t *testing.T, db *store.DB, prefix string) (grp, laneA, laneB, park *nodes.Node, slotsA, slotsB []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	grpType, _ := db.GetNodeTypeByCode("NGRP")
	lanType, _ := db.GetNodeTypeByCode("LANE")

	bp = &payloads.Payload{Code: prefix + "-P"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	grp = &nodes.Node{Name: prefix + "-GRP", NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create group")

	mkLane := func(name string, depth int) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
		testutil.MustNoErr(t, db.CreateNode(lane), "create "+name)
		var slots []*nodes.Node
		for d := 1; d <= depth; d++ {
			at := d
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, d), ParentID: &lane.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	laneA, slotsA = mkLane(prefix+"-LANE-A", 2)
	laneB, slotsB = mkLane(prefix+"-LANE-B", 3)

	park = &nodes.Node{Name: prefix + "-PARK", ParentID: &grp.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(park), "create parking")

	grp, _ = db.GetNode(grp.ID)
	return grp, laneA, laneB, park, slotsA, slotsB, bp
}

// crossFlowInvariant is the pair of facts 13a is about, and it is a type rather
// than two loops inline because it is asked EIGHT times: once after every
// production call in the scenario below.
//
// Both halves read exactly what production reads. Occupancy comes from
// reservations.OccupantsOf, which is admission's own source (admitLane arm 2), and
// the inbound count is CountInFlightOrdersByDeliveryNodeExcluding, which is the
// count CheckDropoffCapacity consults on shuffleSlotFree's behalf. Asserting
// against a second definition of "inside" would let the two drift and call it a
// pass.
type crossFlowInvariant struct {
	t     *testing.T
	db    *store.DB
	lanes []*nodes.Node
	slots []*nodes.Node
}

func (inv crossFlowInvariant) holds(when string) {
	inv.t.Helper()
	for _, lane := range inv.lanes {
		occ, err := reservations.OccupantsOf(inv.db.DB, lane.ID)
		testutil.MustNoErr(inv.t, err, "occupants of "+lane.Name)
		if len(occ) > 1 {
			inv.t.Fatalf("%s: lane %s holds %d occupants (%v), want at most one. Two flows are inside "+
				"one single-file corridor at the same moment — the collision Hold B exists to prevent, "+
				"reached through the cross-flow door rather than the sibling one",
				when, lane.Name, len(occ), occ)
		}
	}
	for _, slot := range inv.slots {
		n, err := inv.db.CountInFlightOrdersByDeliveryNodeExcluding(slot.Name, 0)
		testutil.MustNoErr(inv.t, err, "inbound count for "+slot.Name)
		if n > 1 {
			inv.t.Fatalf("%s: %d orders are inbound to %s, want at most one. The second bin lands on the "+
				"first and ApplyArrival evicts the incumbent to _TRANSIT — the sim's SMN_008/SMN_009 "+
				"orphaning (D83a), arrived at from two different digs rather than one",
				when, n, slot.Name)
		}
	}
}

// landLeg runs a dig leg the way the floor does: the bin arrives at the leg's
// delivery node and the leg terminalizes, which is what releases its occupancy
// (TerminalizeOrder → reservations.ReleaseByOrder, kind-agnostic).
func landLeg(t *testing.T, db *store.DB, leg *orders.Order) {
	t.Helper()
	if leg.BinID == nil {
		t.Fatalf("leg %d carries no bin — the fixture is wrong, not the code", leg.ID)
	}
	dest, err := db.GetNodeByDotName(leg.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve leg destination "+leg.DeliveryNode)
	testutil.MustNoErr(t, db.MoveBinClearingStaging(*leg.BinID, dest.ID, false), "land the leg's bin")
	_, err = db.TerminalizeOrder(leg.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "terminalize the leg")
}

// legsOf returns a compound's children in sequence order, reloaded.
func legsOf(t *testing.T, db *store.DB, parentID int64) []*orders.Order {
	t.Helper()
	legs, err := db.ListChildOrders(parentID)
	testutil.MustNoErr(t, err, "list child orders")
	return legs
}

// TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace is catalog row 4.2, and its
// checker is the invariant above rather than the end state.
//
// The story, and every step is a production call:
//
//  1. dig B plans lane B. It takes the group's only parking and its first leg goes
//     out — B is now INSIDE lane B.
//  2. dig A plans lane A. Parking is spoken for (D83a: the gate counts dig B's leg
//     as inbound, so the node is not free even though it is empty), so A's blocker
//     is aimed at lane B's mouth slot. THAT IS "dig A packs lane B".
//  3. A's leg is refused at the lane, because a foreign dig excludes everything. B
//     WON THE RACE: B's leg is inside and A's part has not landed.
//  4. B's legs run out. The lane clears, the lock lifts.
//  5. A's leg is re-driven into the lane it was aimed at all along, and both
//     demands finish.
//
// MUTATION 1 (verified): revert shuffleSlotFree (reshuffle.go) to the pre-D83a
// body — `cnt, _ := db.CountBinsByNode(n.ID); return cnt == 0`. Dig A then takes
// the parking node dig B's leg is already flying to, and the INBOUND half of the
// invariant fires at step 2: "2 orders are inbound to XF-PARK".
//
// MUTATION 2 (verified): stop asking admission about the DESTINATION lane — in
// admit() (admission.go), replace the tail `return d.admitLane(s, s.destNode,
// false)` with `return Admitted(), nil`. This is the second of the three
// behaviours AdvanceCompoundOrder's comment says delegating ADDS ("a foreign dig
// on the DESTINATION lane now holds it"), and it is the arm this scenario is
// about: dig A picks in a lane it owns and PLACES in a lane it does not. The
// OCCUPANCY half fires at step 2 with two occupants on XF-LANE-B.
//
// MUTATION 3 (verified, and it is the interesting one): delete admission's dig arm
// alone — the `if digOwner != 0 && !d.ownsDig(...)` refusal in admitLane. NO
// collision opens, because the occupancy arm underneath it still refuses: dig B's
// leg is physically in the lane at that moment. What breaks is the LABEL — the leg
// holds under `lane-occupied` instead of `lane-dig-active` — and the cause
// assertion at step 3 is what catches it. Recorded because the two arms are
// redundant HERE and are not in general (a dig holds a lane through the gaps
// between its own legs, when nobody is inside it at all), so a reader must not
// take this test as proof that either one alone is sufficient.
//
// Three mutations breaking three different assertions is the point of composing
// them: each existing test sees only its own half, and passes while the other is
// gone.
func TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, laneA, laneB, park, slotsA, slotsB, bp := twoDigsOneGroup(t, db, "XF")

	blockerA := testdb.CreateBinAtNode(t, db, bp.Code, slotsA[0].ID, "XF-A-BLK")
	targetA := testdb.CreateBinAtNode(t, db, bp.Code, slotsA[1].ID, "XF-A-TGT")
	// Lane B is [empty, blocker, target]: the mouth slot is what dig A can be
	// offered, and the bin behind it is what dig B has to move.
	testdb.CreateBinAtNode(t, db, bp.Code, slotsB[1].ID, "XF-B-BLK")
	targetB := testdb.CreateBinAtNode(t, db, bp.Code, slotsB[2].ID, "XF-B-TGT")
	lineA := lineNode(t, db, "XF-LINE-A")
	lineB := lineNode(t, db, "XF-LINE-B")

	inv := crossFlowInvariant{
		t: t, db: db,
		lanes: []*nodes.Node{laneA, laneB},
		slots: append(append([]*nodes.Node{park}, slotsA...), slotsB...),
	}
	inv.holds("before anything ran")

	mkDemand := func(uuid, delivery string) *orders.Order {
		return testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = uuid
			o.OrderType = OrderTypeRetrieve
			o.PayloadCode = bp.Code
			o.DeliveryNode = delivery
			o.Status = protocol.StatusPending
		})
	}
	demandA := mkDemand("xf-demand-a", lineA.Name)
	demandB := mkDemand("xf-demand-b", lineB.Name)

	// ── 1. DIG B PLANS AND ENTERS ─────────────────────────────────────────────
	if _, pe := d.planner.planBuriedReshuffle(demandB, &BuriedError{Bin: targetB, Slot: slotsB[2], LaneID: laneB.ID}); pe != nil {
		t.Fatalf("dig B must plan cleanly against a quiet group, got %s: %s", pe.Code, pe.Detail)
	}
	inv.holds("after dig B planned")

	legsB := legsOf(t, db, demandB.ID)
	if len(legsB) != 2 {
		t.Fatalf("dig B planned %d leg(s), want an unbury and a retrieve", len(legsB))
	}
	if legsB[0].DeliveryNode != park.Name {
		t.Fatalf("dig B's unbury is aimed at %s, want the group's parking %s — the fixture's premise is "+
			"that B takes the only direct-child spot, leaving A nothing but lane B",
			legsB[0].DeliveryNode, park.Name)
	}
	if legsB[0].VendorOrderID == "" {
		t.Fatalf("dig B's first leg was never dispatched (queue_cause %q) — the group was quiet", legsB[0].QueueCause)
	}
	if occ, _ := reservations.OccupantsOf(db.DB, laneB.ID); len(occ) != 1 || occ[0] != legsB[0].ID {
		t.Fatalf("lane B occupants = %v, want exactly dig B's first leg (%d)", occ, legsB[0].ID)
	}

	// ── 2. DIG A PLANS, AND PACKS LANE B ──────────────────────────────────────
	if _, pe := d.planner.planBuriedReshuffle(demandA, &BuriedError{Bin: targetA, Slot: slotsA[1], LaneID: laneA.ID}); pe != nil {
		t.Fatalf("dig A was refused (%s: %s) — lane B's mouth slot is free and reachable, so there IS "+
			"somewhere to park and this dig must plan", pe.Code, pe.Detail)
	}
	inv.holds("after dig A planned into lane B")

	legsA := legsOf(t, db, demandA.ID)
	if len(legsA) != 2 {
		t.Fatalf("dig A planned %d leg(s), want an unbury and a retrieve", len(legsA))
	}
	if legsA[0].DeliveryNode != slotsB[0].Name {
		t.Fatalf("dig A's unbury is aimed at %s, want lane B's mouth slot %s. Without that this test is "+
			"not 13a at all — the two flows never converge and every assertion below is vacuous",
			legsA[0].DeliveryNode, slotsB[0].Name)
	}

	// ── 3. B WON THE RACE ─────────────────────────────────────────────────────
	// A's leg is aimed into a lane another dig owns, so it holds. Re-driven the way
	// every lane-clearing event re-drives it: a gate that only holds once is not a
	// gate.
	for range 3 {
		testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandA.ID), "re-drive dig A while lane B is B's")
		inv.holds("while dig A is re-driven against dig B's lane")
	}
	heldLeg, err := db.GetOrder(legsA[0].ID)
	testutil.MustNoErr(t, err, "reload dig A's first leg")
	if heldLeg.VendorOrderID != "" {
		t.Fatalf("dig A's leg was dispatched into lane %s while dig B owns it (vendor %q) — a foreign "+
			"dig excludes everything, and B's remaining leg would find a bin in front of it that its "+
			"plan does not contain", laneB.Name, heldLeg.VendorOrderID)
	}
	if heldLeg.QueueCause != string(CauseLaneDigActive) {
		t.Errorf("dig A's leg is holding under cause %q, want %q — a leg parked behind an excavation "+
			"must be distinguishable from one nobody has reached yet",
			heldLeg.QueueCause, CauseLaneDigActive)
	}
	if protocol.IsTerminal(heldLeg.Status) {
		t.Fatalf("dig A's leg is %q — a crowded lane is congestion, and congestion never terminates "+
			"anything", heldLeg.Status)
	}

	// ── 4. DIG B RUNS OUT ─────────────────────────────────────────────────────
	landLeg(t, db, legsB[0]) // the blocker reaches parking
	inv.holds("after dig B's unbury landed")
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandB.ID), "re-drive dig B onto its retrieve")
	inv.holds("after dig B's retrieve was admitted")

	legsB = legsOf(t, db, demandB.ID)
	if legsB[1].VendorOrderID == "" {
		t.Fatalf("dig B's retrieve never went out (queue_cause %q) — its own lane is clear and the bin "+
			"in front of it has gone", legsB[1].QueueCause)
	}
	landLeg(t, db, legsB[1])
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandB.ID), "close dig B")
	inv.holds("after dig B finished")

	doneB, err := db.GetOrder(demandB.ID)
	testutil.MustNoErr(t, err, "reload demand B")
	if doneB.Status != protocol.StatusConfirmed {
		t.Fatalf("demand B is %q, want confirmed — it is the flow that WON, and the winner completing "+
			"is the least this scenario owes", doneB.Status)
	}
	if d.laneLock.IsLocked(laneB.ID) {
		t.Fatal("lane B is still locked after its dig finished — dig A is aimed into that lane and " +
			"nothing alive would ever release it")
	}
	atLine, err := db.GetBin(targetB.ID)
	testutil.MustNoErr(t, err, "reload demand B's bin")
	if atLine.NodeID == nil || *atLine.NodeID != lineB.ID {
		t.Errorf("demand B's bin is at node %v, want the line %d — the retrieve is what the dig was FOR",
			atLine.NodeID, lineB.ID)
	}

	// ── 5. AND A'S PART LANDS ─────────────────────────────────────────────────
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandA.ID), "re-drive dig A now lane B is free")
	inv.holds("after dig A was admitted into the freed lane")

	legsA = legsOf(t, db, demandA.ID)
	if legsA[0].VendorOrderID == "" {
		t.Fatalf("dig A's leg is STILL holding (queue_cause %q) after lane %s cleared — the wait had no "+
			"releaser, which makes it a stall wearing a queue reason", legsA[0].QueueCause, laneB.Name)
	}
	landLeg(t, db, legsA[0])
	inv.holds("after dig A's blocker landed in lane B")
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandA.ID), "re-drive dig A onto its retrieve")
	inv.holds("after dig A's retrieve was admitted")

	legsA = legsOf(t, db, demandA.ID)
	if legsA[1].VendorOrderID == "" {
		t.Fatalf("dig A's retrieve never went out (queue_cause %q)", legsA[1].QueueCause)
	}
	landLeg(t, db, legsA[1])
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandA.ID), "close dig A")
	inv.holds("after both flows finished")

	doneA, err := db.GetOrder(demandA.ID)
	testutil.MustNoErr(t, err, "reload demand A")
	if doneA.Status != protocol.StatusConfirmed {
		t.Fatalf("demand A is %q, want confirmed — it lost the race, which costs it time and must cost "+
			"it nothing else", doneA.Status)
	}

	// THE BLOCKER IS WHERE THE PLAN SAID. Blockers lie where they fall, and this
	// one fell into the lane the other flow had just finished with — which is the
	// whole reason the two guards had to hold while both were live.
	landed, err := db.GetBin(blockerA.ID)
	testutil.MustNoErr(t, err, "reload dig A's blocker")
	if landed.NodeID == nil || *landed.NodeID != slotsB[0].ID {
		t.Errorf("dig A's blocker ended at node %v, want lane B's mouth slot %d", landed.NodeID, slotsB[0].ID)
	}

	// THE LEDGER IS CLEAN. Every hold released on every exit — an orphaned
	// occupancy row makes a lane look permanently busy to everything that follows.
	if d.laneLock.IsLocked(laneA.ID) {
		t.Error("lane A is still locked after its dig finished")
	}
	for _, lane := range []*nodes.Node{laneA, laneB} {
		if occ, _ := reservations.OccupantsOf(db.DB, lane.ID); len(occ) != 0 {
			t.Errorf("lane %s still has occupants %v after both flows completed", lane.Name, occ)
		}
	}
}
