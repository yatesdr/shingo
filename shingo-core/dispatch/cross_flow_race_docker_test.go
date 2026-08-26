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
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// cross_flow_race_docker_test.go — scenario 13a, which right of way turned from
// a race B WINS into a race A NEVER ENTERS.
//
// Two flows converge on one lane. Dig A's excavation needs somewhere to park a
// blocker and the only place left is a slot in lane B; lane B is meanwhile being
// dug for its own demand. 13b (stale_dig_docker_test.go) is what happens when A's
// part lands FIRST and B's plan goes stale. This branch was B's leg getting in
// first and A's part landing afterwards — and right of way (§R.61) removed the
// convergence itself: A is refused at plan assembly and never enters. The
// geometry below is unchanged and now serves the opposite assertion.
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
// one slot findShuffleSlots' second pass could offer instead — which is exactly
// the offer right of way withdraws, because B1 is inside a lane another dig owns.
// The fixture is therefore the minimum shape in which the rule can bite at all:
// remove PARK's scarcity or lane B's free mouth slot and dig A is refused (or
// admitted) for some other reason entirely.
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

// releaseDwell drives a dwelling dig leg through the RELEASE the robot's arrival
// drives in production: Core chooses where the blocker goes, binds it, and
// appends the tail. Returns the leg reloaded, with its destination on it.
//
// It calls the production trigger rather than the resolver directly, so a
// fixture cannot release a leg by a route the plant does not have.
//
// A leg that is not dwelling comes back untouched, which is what lets landLeg
// call this unconditionally: an unbury leg dwells, a retrieve tail does not, and
// the two live side by side in every compound.
func releaseDwell(t *testing.T, d *Dispatcher, db *store.DB, leg *orders.Order) *orders.Order {
	t.Helper()
	if leg.DeliveryNode != "" {
		return leg
	}
	d.EvaluateWaitLaneForStagedOrder(leg.ID)
	fresh, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the dwelling leg")
	if fresh.DeliveryNode == "" {
		t.Fatalf("dwelling leg %d was not released: it is still holding its blocker with no destination "+
			"(queue_cause %q). Either the group genuinely had no free shuffle slot, or the resolver "+
			"declined — read the cause before changing the fixture", leg.ID, fresh.QueueCause)
	}
	return fresh
}

// landLeg runs a dig leg the way the floor does: the bin arrives at the leg's
// delivery node and the leg terminalizes, which is what releases its occupancy
// (TerminalizeOrder → reservations.ReleaseByOrder, kind-agnostic).
//
// IT RELEASES A DWELLER FIRST, because a dig leg no longer knows where it is
// going when it is dispatched — it stands in the lane it is digging until Core
// picks a slot (the outbound dwell). "Land this leg" therefore has to include
// the choice, or the fixture would be asserting against a destination that does
// not exist yet. A test that wants to observe the leg BETWEEN dispatch and
// release calls releaseDwell itself.
func landLeg(t *testing.T, d *Dispatcher, db *store.DB, leg *orders.Order) {
	t.Helper()
	if leg.BinID == nil {
		t.Fatalf("leg %d carries no bin — the fixture is wrong, not the code", leg.ID)
	}
	leg = releaseDwell(t, d, db, leg)
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

// TestCrossFlow_TwoDigsOneLane_ANeverStarts is catalog row 4.2, INVERTED IN PLACE
// by right of way (§R.61, ruled 2026-08-13), and the premise it used to assert is
// quoted below rather than deleted.
//
// ── WHAT THIS TEST USED TO SAY, AND WHY IT WAS RIGHT TO CHANGE IT ─────────
//
// It was TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace, and step 2 read: *"dig A
// plans lane A. Parking is spoken for, so A's blocker is aimed at lane B's mouth
// slot. THAT IS 'dig A packs lane B'."* Its own fatal message argued the premise:
// *"lane B's mouth slot is free and reachable, so there IS somewhere to park and
// this dig must plan."* Both statements are still TRUE of the lane and are now
// REFUSED anyway — the argument is in reshuffle.go's right-of-way header, and the
// short form is that "the mouth slot is free" is a fact about lane B and the
// question is about the SYSTEM: A parking there is A borrowing the only place B's
// own next blocker can go.
//
// The old test was run against this tree before it was replaced, and it failed at
// exactly the line that carried the premise, naming it:
//
//	dig A was refused (no_shuffle_slot: ... the parking this dig needs is inside
//	a lane another dig holds: 1 slot(s) short, and lane XF-LANE-B is held by dig 2)
//
// ── THE STORY NOW, and every step is a production call ────────────────────
//
//  1. dig B plans lane B. It takes the group's only parking and its first leg goes
//     out — B is now INSIDE lane B.
//  2. dig A tries to plan lane A. Parking is spoken for, and the only other space
//     in the group is inside lane B, which dig B holds. RIGHT OF WAY REFUSES IT.
//  3. AND DIG A HOLDS NOTHING — no lane, no leg, no claim, no robot. That is the
//     construction, and it is what the old shape could not give: there, A had a
//     robot standing in lane A with a blocker it could not put down.
//  4. B's legs run out. The lane clears, the lock lifts.
//  5. A plans NOW, against a group with room in it, and both demands finish.
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
//
// MUTATION 4 (verified) is right of way's own, and it is the pin R.61 asked for:
// in findShuffleSlots, put back `db.ListChildNodes(groupID)` in place of
// `db.ListChildNodesUnlocked(groupID, asker)`. Dig A then plans, step 2's refusal
// assertion fires, and the scenario reverts to the one this test used to be. Both
// lanes are UNGATED in twoDigsOneGroup, deliberately: nothing here can pass by
// testing the lane gate instead of the rule.
func TestCrossFlow_TwoDigsOneLane_ANeverStarts(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
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
	if legsB[0].VendorOrderID == "" {
		t.Fatalf("dig B's first leg was never dispatched (queue_cause %q) — the group was quiet", legsB[0].QueueCause)
	}
	// IT IS INSIDE LANE B, HOLDING ITS BLOCKER AND ITS ROW. Under the outbound
	// dwell the leg lifts and STAYS, so this is a stronger statement than it used
	// to be: before, the row was dropped at the lift and the corridor read empty
	// while the robot was still standing in it.
	if occ, _ := reservations.OccupantsOf(db.DB, laneB.ID); len(occ) != 1 || occ[0] != legsB[0].ID {
		t.Fatalf("lane B occupants = %v, want exactly dig B's first leg (%d), which is dwelling in it",
			occ, legsB[0].ID)
	}
	// And it takes the only parking when it is RELEASED. The aim is read here
	// rather than off the plan because that is when the destination is chosen now;
	// the premise it establishes — B has the group's one direct-child spot, leaving
	// A nothing but lane B — is unchanged, and so is what it costs A.
	releasedB0 := releaseDwell(t, d, db, legsB[0])
	if releasedB0.DeliveryNode != park.Name {
		t.Fatalf("dig B's unbury was released onto %s, want the group's parking %s — the fixture's "+
			"premise is that B takes the only direct-child spot, leaving A nothing but lane B",
			releasedB0.DeliveryNode, park.Name)
	}
	legsB = legsOf(t, db, demandB.ID)

	// ── 2. DIG A IS REFUSED, AND THE REFUSAL NAMES WHOSE LANE IT IS ───────────
	//
	// The re-ask loop is kept from the old shape and matters as much: a rule that
	// refuses once and lets the second attempt through is not a rule. Every pass
	// must reach the same answer while lane B is B's.
	var pe *planningError
	for range 3 {
		_, pe = d.planner.planBuriedReshuffle(demandA, &BuriedError{Bin: targetA, Slot: slotsA[1], LaneID: laneA.ID})
		if pe == nil {
			t.Fatalf("dig A PLANNED while dig B holds lane %s — the only parking left in the group is "+
				"inside that lane, and a dig that plans into a lane another dig holds is the wedge this "+
				"rule exists to make unconstructable", laneB.Name)
		}
		inv.holds("while dig A is refused against dig B's lane")
	}
	var held *DigParkingHeldError
	if !errors.As(pe.Err, &held) {
		t.Fatalf("dig A was refused as %q (%s) — right of way must refuse with the typed error, because "+
			"a wait that cannot name the order it is waiting on has no releaser a reader can check "+
			"(law 8)", pe.Code, pe.Detail)
	}
	if held.Lane != laneB.Name {
		t.Errorf("the refusal names lane %q, want %q — the lane an operator has to look at is the one "+
			"that must free, not the one being dug", held.Lane, laneB.Name)
	}
	if held.HolderID != demandB.ID {
		t.Errorf("the refusal names dig %d, want demand B (%d) — planBuriedReshuffle re-parents the "+
			"demand, so the demand IS the dig that holds lane B", held.HolderID, demandB.ID)
	}

	// ── 3. AND DIG A HOLDS NOTHING ────────────────────────────────────────────
	//
	// THIS IS THE CONSTRUCTION, and it is the assertion the old shape could not
	// make. There, A had a robot standing in lane A with a blocker in the air and
	// an occupancy row under it, waiting for B — hold-and-wait, both halves. Here
	// the refusal happens before the lock, before any child, before any claim, so
	// A is not a party to a wait-for graph at all.
	if legs := legsOf(t, db, demandA.ID); len(legs) != 0 {
		t.Fatalf("dig A wrote %d leg(s) after being refused — a refused dig must hold nothing, and a "+
			"child order is a claim on a bin", len(legs))
	}
	if d.laneLock.IsLocked(laneA.ID) {
		t.Fatal("dig A took lane A after being refused — the plan precedes the lock precisely so this " +
			"refusal is free")
	}
	blkA, err := db.GetBin(blockerA.ID)
	testutil.MustNoErr(t, err, "reload dig A's blocker")
	if blkA.ClaimedBy != nil {
		t.Errorf("dig A's blocker is claimed by order %d after the dig was refused — an unclaimed bin "+
			"is what lets some other flow use it while A waits", *blkA.ClaimedBy)
	}
	if occ, _ := reservations.OccupantsOf(db.DB, laneA.ID); len(occ) != 0 {
		t.Errorf("lane A has occupants %v after dig A was refused — nothing was ever dispatched", occ)
	}
	waitingA, err := db.GetOrder(demandA.ID)
	testutil.MustNoErr(t, err, "reload demand A")
	if protocol.IsTerminal(waitingA.Status) {
		t.Fatalf("demand A is %q — right of way is congestion and congestion never terminates anything",
			waitingA.Status)
	}
	if waitingA.QueueCause != string(CauseDigHoldsParking) {
		t.Errorf("demand A is parked under cause %q, want %q — %q would send a reader to look at a full "+
			"group, and the group is not full",
			waitingA.QueueCause, CauseDigHoldsParking, CauseNoShuffleSlot)
	}

	// ── 4. DIG B RUNS OUT ─────────────────────────────────────────────────────
	landLeg(t, d, db, legsB[0]) // the blocker reaches parking
	inv.holds("after dig B's unbury landed")
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandB.ID), "re-drive dig B onto its retrieve")
	inv.holds("after dig B's retrieve was admitted")

	legsB = legsOf(t, db, demandB.ID)
	if legsB[1].VendorOrderID == "" {
		t.Fatalf("dig B's retrieve never went out (queue_cause %q) — its own lane is clear and the bin "+
			"in front of it has gone", legsB[1].QueueCause)
	}
	landLeg(t, d, db, legsB[1])
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandB.ID), "close dig B")
	inv.holds("after dig B finished")

	doneB, err := db.GetOrder(demandB.ID)
	testutil.MustNoErr(t, err, "reload demand B")
	if doneB.Status != protocol.StatusConfirmed {
		t.Fatalf("demand B is %q, want confirmed — it is the flow that WON, and the winner completing "+
			"is the least this scenario owes", doneB.Status)
	}
	if d.laneLock.IsLocked(laneB.ID) {
		t.Fatal("lane B is still locked after its dig finished — dig A is waiting on exactly that " +
			"release, and nothing else would ever give it one")
	}
	atLine, err := db.GetBin(targetB.ID)
	testutil.MustNoErr(t, err, "reload demand B's bin")
	if atLine.NodeID == nil || *atLine.NodeID != lineB.ID {
		t.Errorf("demand B's bin is at node %v, want the line %d — the retrieve is what the dig was FOR",
			atLine.NodeID, lineB.ID)
	}

	// ── 5. AND NOW DIG A PLANS ────────────────────────────────────────────────
	//
	// THE RELEASER IS REAL, which is the other half of law 8 and the half a refusal
	// can quietly fail. A rule that refuses correctly and never lets the refused
	// party through is a wedge with better manners.
	if _, pe := d.planner.planBuriedReshuffle(demandA, &BuriedError{Bin: targetA, Slot: slotsA[1], LaneID: laneA.ID}); pe != nil {
		t.Fatalf("dig A is STILL refused (%s: %s) after dig B released lane %s — the wait named that "+
			"release as its releaser", pe.Code, pe.Detail, laneB.Name)
	}
	inv.holds("after dig A planned into the freed group")

	legsA := legsOf(t, db, demandA.ID)
	if len(legsA) != 2 {
		t.Fatalf("dig A planned %d leg(s), want an unbury and a retrieve", len(legsA))
	}
	releaseDwell(t, d, db, legsA[0])
	if legsA[0].VendorOrderID == "" {
		t.Fatalf("dig A's leg is STILL holding (queue_cause %q) after lane %s cleared — the wait had no "+
			"releaser, which makes it a stall wearing a queue reason", legsA[0].QueueCause, laneB.Name)
	}
	landLeg(t, d, db, legsA[0])
	inv.holds("after dig A's blocker landed in lane B")
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandA.ID), "re-drive dig A onto its retrieve")
	inv.holds("after dig A's retrieve was admitted")

	legsA = legsOf(t, db, demandA.ID)
	if legsA[1].VendorOrderID == "" {
		t.Fatalf("dig A's retrieve never went out (queue_cause %q)", legsA[1].QueueCause)
	}
	landLeg(t, d, db, legsA[1])
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demandA.ID), "close dig A")
	inv.holds("after both flows finished")

	doneA, err := db.GetOrder(demandA.ID)
	testutil.MustNoErr(t, err, "reload demand A")
	if doneA.Status != protocol.StatusConfirmed {
		t.Fatalf("demand A is %q, want confirmed — it lost the race, which costs it time and must cost "+
			"it nothing else", doneA.Status)
	}

	// THE BLOCKER IS WHERE CORE SENT IT, AND IT IS IN LANE B. Blockers lie where
	// they fall, and this one fell into the lane the other flow had just finished
	// with — which is the whole reason the two guards had to hold while both were
	// live.
	//
	// WHICH slot in lane B is deliberately not pinned any more. It used to be the
	// MOUTH slot, because that was the only free one at the moment dig A planned;
	// the choice is made after dig B finished now, when the lane it emptied is
	// wholly free, and findShuffleSlots packs DEEPEST-FIRST to keep the lane's FIFO
	// invariant. Pinning the mouth would be pinning the old moment's information,
	// not a property. What is asserted instead is the pair that matters: the lane
	// is the one the flows converged on, and the bin is at the node the leg's own
	// row named — the plan and the outcome agreeing is what a stale binding breaks.
	landed, err := db.GetBin(blockerA.ID)
	testutil.MustNoErr(t, err, "reload dig A's blocker")
	if landed.NodeID == nil {
		t.Fatalf("dig A's blocker is at no node at all after its leg landed")
	}
	landedLane, err := db.LaneForNode(*landed.NodeID)
	testutil.MustNoErr(t, err, "resolve the lane dig A's blocker landed in")
	if landedLane == nil || landedLane.ID != laneB.ID {
		t.Errorf("dig A's blocker ended in %s, want lane B — the convergence is the whole scenario",
			nodeName(landedLane))
	}
	aimed, err := db.GetNodeByDotName(legsA[0].DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the slot dig A's leg was released onto")
	if aimed == nil || *landed.NodeID != aimed.ID {
		t.Errorf("dig A's blocker is at node %d but its leg was released onto %q — the row and the bin "+
			"disagree, which is the stale-binding failure the late choice exists to remove",
			*landed.NodeID, legsA[0].DeliveryNode)
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
