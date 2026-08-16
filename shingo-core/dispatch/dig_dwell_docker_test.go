//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
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

// dig_dwell_docker_test.go — the outbound dwell, end to end.
//
// THE CLAIM UNDER TEST: a dig leg dispatches UNSEALED with no destination, comes
// to the shallowest slot of the lane it is digging (or does not move at all if it
// is already there), and Core chooses where the blocker goes at release and
// appends the tail.
//
// Every fixture here drives PRODUCTION calls. The release is always through
// EvaluateWaitLaneForStagedOrder or EvaluateLaneReleases — the two triggers the
// plant has — never through the resolver directly, because a test that can reach
// a state the plant cannot is testing something else.

// setupDwellGroup builds an UNMARKED group, which is what both plants run today:
//
//	DW-GRP
//	├── DW-DUG   depth 3: S1 · S2 · S3        <- the dig happens here
//	├── DW-SIB   depth 2: S1 · S2             <- sibling lane, the shuffle pool
//	└── DW-PARK  (a direct child of the group)
//
// No lane carries a gate point. That is the whole point of always-shallowest: the
// dwell needs no mark, so it works at an unmarked plant on day one, and the
// waiting position is a real Core node rather than a map-point property.
func setupDwellGroup(t *testing.T, db *store.DB, prefix string, sibDepth int, withPark bool) (grp, dug, sib, park *nodes.Node, dugSlots, sibSlots []*nodes.Node, bp *payloads.Payload) {
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
		for i := 1; i <= depth; i++ {
			at := i
			s := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), ParentID: &lane.ID, Enabled: true, Depth: &at}
			testutil.MustNoErr(t, db.CreateNode(s), "create slot")
			slots = append(slots, s)
		}
		reloaded, _ := db.GetNode(lane.ID)
		return reloaded, slots
	}
	dug, dugSlots = mkLane(prefix+"-DUG", 3)
	if sibDepth > 0 {
		sib, sibSlots = mkLane(prefix+"-SIB", sibDepth)
	}
	if withPark {
		park = &nodes.Node{Name: prefix + "-PARK", ParentID: &grp.ID, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(park), "create parking")
	}
	grp, _ = db.GetNode(grp.ID)
	return grp, dug, sib, park, dugSlots, sibSlots, bp
}

// liftBin models THE LIFT the way the plant does it: the bin leaves its slot for
// the transit node, and the lane-gate handler runs on that event.
//
// Both halves matter and a fixture that fires only the second is testing nothing.
// The handler's whole job is to decide what a bin entering transit means for the
// corridor — is this robot leaving, or is it a dweller that lifted and stayed? —
// and both the occupancy hold and flip 2 read the bin's POSITION to answer it. A
// bin still sitting in its slot while the event claims it is in transit is a
// state the plant cannot produce and the code should not be asked about.
func liftBin(t *testing.T, db *store.DB, d *Dispatcher, leg *orders.Order, from, transit *nodes.Node) {
	t.Helper()
	if leg.BinID == nil {
		t.Fatalf("leg %d carries no bin to lift", leg.ID)
	}
	testutil.MustNoErr(t, db.MoveBinToTransit(*leg.BinID, transit.ID), "lift the blocker into transit")
	d.HandleTransitForLaneGate(leg.ID, from.ID)
}

// transitNode makes the synthetic node a lifted bin lives at while a robot is
// carrying it.
func transitNode(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create the transit node")
	return n
}

// planDigFor buries a target behind blockers and plans the dig, returning the
// demand and its legs in sequence order.
func planDigFor(t *testing.T, db *store.DB, d *Dispatcher, bp *payloads.Payload, lane *nodes.Node,
	targetSlot *nodes.Node, target *orders.Order) []*orders.Order {
	t.Helper()
	_, pe := d.planner.planBuriedReshuffle(target, &BuriedError{
		Bin: mustBinAt(t, db, targetSlot), Slot: targetSlot, LaneID: lane.ID})
	if pe != nil {
		t.Fatalf("the dig must plan: %s: %s", pe.Code, pe.Detail)
	}
	_ = bp
	return legsOf(t, db, target.ID)
}

func mustBinAt(t *testing.T, db *store.DB, slot *nodes.Node) *bins.Bin {
	t.Helper()
	list, err := db.ListBinsByNode(slot.ID)
	testutil.MustNoErr(t, err, "list bins at "+slot.Name)
	if len(list) == 0 {
		t.Fatalf("no bin at %s — the fixture is wrong", slot.Name)
	}
	return list[0]
}

// planOf returns a leg's steps_json parsed.
func planOf(t *testing.T, db *store.DB, legID int64) []resolvedStep {
	t.Helper()
	leg, err := db.GetOrder(legID)
	testutil.MustNoErr(t, err, "reload the leg")
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(leg.StepsJSON), &steps), "parse the leg's plan")
	return steps
}

// TestDwell_LiftsAndWaitsAtTheShallowestSlot is the shape itself.
//
// A dig at depth 3 emits blockers at S1 and S2. The FIRST leg lifts the shallowest
// blocker, which is already at the shallowest slot, and must not move at all; the
// SECOND lifts from S2 and comes forward to S1. Neither leg names a destination,
// and each one's wait names the DUG LANE — a real lane, with an evaluator, a
// floor, a cause vocabulary and a tripwire, which is exactly what a wait naming no
// lane lacks.
//
// MUTATION: make dwellSlotFor return the leg's own pickup slot instead of the
// lane's shallowest (drop the ListLaneSlots walk and use the source). Leg 2 then
// waits at S2 with S1 empty in front of it, and the depth assertion fires — the
// robot is parked deeper than it needs to be, holding the corridor for longer,
// which is the cost the always-shallowest rule exists to avoid.
func TestDwell_LiftsAndWaitsAtTheShallowestSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, "DWSHAL", 2, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWSHAL-BLK1")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWSHAL-BLK2")
	createTestBinAtNode(t, db, bp.Code, dugSlots[2].ID, "DWSHAL-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwshal"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWSHAL-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[2], demand)
	if len(legs) != 3 {
		t.Fatalf("the dig has %d leg(s), want two unburies and a retrieve", len(legs))
	}

	// ── LEG 1: LIFTS AT DEPTH 1 AND DOES NOT MOVE ────────────────────────────
	steps := planOf(t, db, legs[0].ID)
	if len(steps) != 2 {
		t.Fatalf("leg 1's plan is %d step(s) (%+v), want [pickup, wait] — an unsealed dwell", len(steps), steps)
	}
	if steps[0].Action != protocol.ActionPickup || steps[0].Node != dugSlots[0].Name {
		t.Fatalf("leg 1 picks %s at %q, want a pickup at the shallowest blocker %s",
			steps[0].Action, steps[0].Node, dugSlots[0].Name)
	}
	if steps[1].Action != protocol.ActionWait || steps[1].WaitKind != WaitKindLane {
		t.Fatalf("leg 1's second step is %s/%q, want a LANE wait", steps[1].Action, steps[1].WaitKind)
	}
	if steps[1].WaitLane != dug.ID {
		t.Fatalf("leg 1's wait names lane %d, want the DUG lane %d. A wait naming no lane — or the wrong "+
			"one — is invisible to every evaluator and to the 60-second floor while being exempt from the "+
			"abandon sweep: unbounded, unreleasable and silent, with a bin in the gripper",
			steps[1].WaitLane, dug.ID)
	}
	if steps[1].Node != dugSlots[0].Name {
		t.Errorf("leg 1 waits at %q, want %s — it lifted the shallowest blocker, so it is ALREADY at the "+
			"shallowest slot and must not move at all", steps[1].Node, dugSlots[0].Name)
	}
	if legs[0].DeliveryNode != "" {
		t.Errorf("leg 1 was born aimed at %q — a dwelling leg names no destination, which is the "+
			"commitment the dwell exists to remove", legs[0].DeliveryNode)
	}

	// ── LEG 2: LIFTS AT DEPTH 2 AND COMES FORWARD TO DEPTH 1 ─────────────────
	// Run leg 1 out first so leg 2 is the one being dispatched.
	landLeg(t, d, db, legs[0])
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(demand.ID), "advance onto leg 2")

	steps = planOf(t, db, legs[1].ID)
	if len(steps) != 2 || steps[1].Action != protocol.ActionWait {
		t.Fatalf("leg 2's plan is %+v, want [pickup, wait]", steps)
	}
	if steps[0].Node != dugSlots[1].Name {
		t.Fatalf("leg 2 picks at %q, want the second blocker %s", steps[0].Node, dugSlots[1].Name)
	}
	if steps[1].Node != dugSlots[0].Name {
		t.Errorf("leg 2 waits at %q, want the lane's shallowest slot %s. Waiting there costs no new "+
			"motion — a robot that picked at depth 2 has to reverse out through depth 1 to leave the lane "+
			"at all, so it is a pause at a point it was already going to drive",
			steps[1].Node, dugSlots[0].Name)
	}
	if steps[1].WaitLane != dug.ID {
		t.Errorf("leg 2's wait names lane %d, want the dug lane %d", steps[1].WaitLane, dug.ID)
	}
}

// TestDwell_OpenDestinationReleasesOnArrival is the cheap case, and it is the one
// that decides whether the dwell is felt on the floor at all.
//
// With somewhere to put the blocker, the release happens the moment the robot
// reports staged — one round trip, not one 60-second floor interval — and the leg
// leaves with a sealed tail aimed at the slot Core picked against fresh state.
//
// MUTATION: make releaseDwellingDigLeg return a refusal unconditionally. This
// fires — the leg stands at its wait with the group's parking empty.
//
// THE WIRING HALF IS MUTATED SOMEWHERE ELSE, and it has to be, which is worth
// stating rather than leaving as a gap. This test calls the dispatcher's trigger
// directly, so dropping the EvaluateWaitLaneForStagedOrder call from the staged
// arm of wiring_vendor_status.go does NOT fire it. What that mutation fires is
// the engine's compound suite (TestBuriedBin_ReshuffleViaEngine and its
// siblings), which drives the fleet simulator through WAITING and lands the bins:
// verified 2026-08-13, both red with the call removed. The two together are the
// claim — the resolver releases on the trigger, and the plant pulls the trigger.
func TestDwell_OpenDestinationReleasesOnArrival(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, park, dugSlots, _, bp := setupDwellGroup(t, db, "DWOPEN", 2, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWOPEN-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWOPEN-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwopen"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWOPEN-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	// It is dwelling: dispatched, unsealed, no destination.
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig leg was never dispatched (queue_cause %q)", legs[0].QueueCause)
	}
	if legs[0].DeliveryNode != "" {
		t.Fatalf("the leg is already aimed at %q before the robot has arrived anywhere", legs[0].DeliveryNode)
	}

	// THE ARRIVAL. This is the production trigger, and the release is its effect.
	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)

	released, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the leg after arrival")
	if released.DeliveryNode == "" {
		t.Fatalf("the leg is still dwelling (queue_cause %q) with the group's parking standing empty — "+
			"an open destination must release on arrival, not on the floor's next tick", released.QueueCause)
	}
	if released.DeliveryNode != park.Name {
		t.Errorf("the leg was released onto %q, want the free parking %s", released.DeliveryNode, park.Name)
	}
	if IsGateStaged(released) {
		t.Errorf("the leg is still gate-staged after its tail was appended (wait_index=%d) — the wait it "+
			"is parked at should be behind it now", released.WaitIndex)
	}
	// AND THE TAIL IS ON THE PLAN, not only on the column: the robot is driven from
	// steps_json, and a column moving without its step is what sent a robot to one
	// node while the plan said another (§R.5).
	steps := planOf(t, db, legs[0].ID)
	if len(steps) != 3 || steps[2].Action != protocol.ActionDropoff || steps[2].Node != park.Name {
		t.Errorf("the leg's plan is %+v, want a dropoff at %s appended after the wait", steps, park.Name)
	}
}

// TestDwell_ClosedDestinationWaitsWithACauseAndWakesOnAGroupSlot is the expensive
// case, and it is where law 8 is either satisfied or it is not.
//
// The group is full, so the robot stands in the lane it is digging holding a
// blocker with nowhere to put it. That wait must NAME ITS OWNER, ITS CAUSE AND ITS
// RELEASER: the owner is the dug lane (a real lane the floor sweeps), the cause is
// no-shuffle-slot on the row, and the releaser is a slot freeing ANYWHERE IN THE
// GROUP — which is not the dweller's own lane, because the dweller is standing in
// that one and it will not clear.
//
// MUTATION 1: make DwellerLanesSharingGroupWith return nil. The wake half fires —
// the sibling lane frees a slot, nothing maps that to the dweller's lane, and the
// leg stands there until the floor's next pass.
//
// MUTATION 2: skip the dwell arm in gateStagedForLane so the candidate is never
// added. The cause assertion fires immediately: the dweller becomes invisible to
// the evaluator and carries a blank row, which is the exact shape three robots sat
// in for a whole soak (§12.49).
//
// A COVERAGE GAP, NAMED RATHER THAN PAPERED OVER. This test calls the dispatcher's
// mapping directly, so removing the evaluateGroup CALL from the BinEnteredTransit
// subscriber (wiring_lane_gate.go) does not fire it, and no other test does either
// — verified 2026-08-13, that mutation leaves the suite green. The subscriber is
// one line with no branch, and the 60-second floor bounds what its loss costs
// (slow, not wrong), which is why this is recorded as a known thinness rather than
// treated as a hole. The honest way to close it is an engine-level test that
// publishes the event and watches a dweller in a sibling lane release.
func TestDwell_ClosedDestinationWaitsWithACauseAndWakesOnAGroupSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWFULL", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWFULL-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWFULL-TGT")
	// The sibling lane is FULL, and there is no group parking, so the pool is dry.
	evictable := createTestBinAtNode(t, db, bp.Code, sibSlots[0].ID, "DWFULL-SIB1")
	createTestBinAtNode(t, db, bp.Code, sibSlots[1].ID, "DWFULL-SIB2")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwfull"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWFULL-LINE").Name
		o.Status = protocol.StatusPending
	})
	// The dig plans against a dry pool only because its own lane is what it is
	// digging; the count check refuses first, so this fixture frees one slot to let
	// the dig start and re-fills it before the release. That is the honest shape of
	// "the group closed while the robot was working".
	testutil.MustNoErr(t, db.MoveBinClearingStaging(evictable.ID, dugSlots[0].ID, false), "park a bin out of the way")
	// dugSlots[0] now holds two bins conceptually; instead move it to the line.
	line := lineNode(t, db, "DWFULL-PARKING")
	testutil.MustNoErr(t, db.MoveBinClearingStaging(evictable.ID, line.ID, false), "clear the sibling's mouth")

	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig leg was never dispatched (queue_cause %q)", legs[0].QueueCause)
	}

	// THE GROUP CLOSES while the robot is working: the freed sibling slot fills.
	refill := createTestBinAtNode(t, db, bp.Code, sibSlots[0].ID, "DWFULL-REFILL")

	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	held, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the dwelling leg")
	if held.DeliveryNode != "" {
		t.Fatalf("the leg was released onto %q with every slot in the group occupied — a blocker cannot "+
			"be put down where a bin already is", held.DeliveryNode)
	}

	// (a) THE CAUSE IS ON THE ROW.
	if QueueCause(held.QueueCause) != CauseNoShuffleSlot {
		t.Errorf("the dwelling leg carries cause %q, want %q. A robot parked in a lane holding a bin with "+
			"a blank row is indistinguishable from one nobody has evaluated — the shape that held three "+
			"robots for a whole soak", held.QueueCause, CauseNoShuffleSlot)
	}

	// (b) THE OWNER IS A REAL LANE, AND THE FLOOR CAN SEE IT.
	if lane := laneOfGateWait(held); lane != dug.ID {
		t.Fatalf("the dweller's wait names lane %d, want the dug lane %d — a zero here is the silent trap: "+
			"exempt from the abandon sweep, invisible to every evaluator and dropped by the floor",
			lane, dug.ID)
	}
	waiters, err := d.laneWaiters()
	testutil.MustNoErr(t, err, "read the floor's waiting set")
	found := false
	for _, w := range waiters {
		if w.orderID == held.ID {
			found = true
			if w.pop != PopGateStaged {
				t.Errorf("the floor files the dweller under %q, want %q", w.pop, PopGateStaged)
			}
			if w.laneID != dug.ID {
				t.Errorf("the floor thinks the dweller is waiting on lane %d, want %d", w.laneID, dug.ID)
			}
		}
	}
	if !found {
		t.Fatalf("the 60-second floor cannot see the dwelling leg %d at all. Its event releaser may be "+
			"incomplete — that is survivable — but a population with no floor is F-22, and this one is a "+
			"committed robot rather than a row", held.ID)
	}

	// (c) THE RELEASER: A SLOT FREES SOMEWHERE ELSE IN THE GROUP.
	//
	// Not in the dweller's own lane — that one will not clear, because the dweller
	// is standing in it. This is the trigger widening: the evaluator keys on
	// w.WaitLane, so without it this event reaches nobody.
	testutil.MustNoErr(t, db.MoveBinClearingStaging(refill.ID, line.ID, false), "free a sibling slot")
	for _, laneID := range d.DwellerLanesSharingGroupWith(sibSlots[0].ID) {
		d.EvaluateLaneReleases(laneID)
	}

	freed, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload after the group freed a slot")
	if freed.DeliveryNode == "" {
		t.Fatalf("a slot freed in %s and the dweller in %s was never re-asked (cause %q) — its lane is "+
			"the one lane that cannot clear, so an event set keyed on the dweller's own lane never "+
			"releases it", sib.Name, dug.Name, freed.QueueCause)
	}
	if freed.DeliveryNode != sibSlots[0].Name {
		t.Errorf("the leg was released onto %q, want the slot that just freed %s",
			freed.DeliveryNode, sibSlots[0].Name)
	}
}

// TestDwell_HoldsItsLaneUntilItDrivesOut is §7.3, and it is the correction of a
// claim that was FALSE rather than merely unproven.
//
// The occupancy row used to drop at the LIFT, on the reasoning that a robot that
// has raised a bin is driving out. Under the dwell it is not: it stays, standing
// in the lane. The earlier draft of this shape assumed `ModeDig` contained the
// consequence — that nobody else could enter anyway — and that is wrong in the one
// direction that matters: a sibling leg is exempt from its OWN parent's dig lock
// by design (ownsDig routes the leg's question to its parent), so the lock
// excludes everyone EXCEPT the exact population that queues behind a dwelling
// robot.
//
// MUTATION: delete the holdsOccupancyThroughDwell early return in
// releaseOccupancyOnExit. The row drops at the lift, the next leg is admitted, and
// two robots are nose to tail in one single-file corridor with the deeper one's
// exit behind the shallower one's — in-lane stacking, which is an unproven
// property of the fleet and Eric's question rather than the code's.
func TestDwell_HoldsItsLaneUntilItDrivesOut(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, park, dugSlots, _, bp := setupDwellGroup(t, db, "DWOCC", 2, true)

	blocker := createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWOCC-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWOCC-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwocc"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWOCC-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	// The robot is inside the lane, and the row says so.
	occ, err := reservations.OccupantsOf(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "occupants at dispatch")
	if len(occ) != 1 || occ[0] != legs[0].ID {
		t.Fatalf("lane occupants at dispatch = %v, want exactly the dig leg %d", occ, legs[0].ID)
	}

	// THE LIFT. The bin enters transit — and under a sealed plan that was the exit.
	liftBin(t, db, d, legs[0], dugSlots[0], transitNode(t, db, "DWOCC-TRANSIT"))
	occ, err = reservations.OccupantsOf(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "occupants after the lift")
	if len(occ) != 1 || occ[0] != legs[0].ID {
		t.Fatalf("lane occupants after the lift = %v, want the dwelling leg %d still in place. It lifted "+
			"and STAYED — releasing here declares an occupied corridor empty and admits a sibling in "+
			"behind a robot that has not moved", occ, legs[0].ID)
	}
	_ = blocker

	// THE RELEASE. Now it has a destination and is driving out, so the lane frees.
	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	released, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload after release")
	if released.DeliveryNode != park.Name {
		t.Fatalf("the leg was released onto %q, want %s", released.DeliveryNode, park.Name)
	}
	// THE DWELLER'S ROW IS GONE. Not "the lane is empty" — the next leg of this
	// same dig is admitted during the drive-out, which is precisely the pipelining
	// the sibling-in-flight guard was retired to get, and asserting an empty lane
	// here would pin the serialization instead of the release.
	occ, err = reservations.OccupantsOf(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "occupants after the release")
	for _, o := range occ {
		if o == legs[0].ID {
			t.Errorf("the dwelling leg %d still occupies the lane after its tail was appended "+
				"(occupants %v). It has its waybill and is driving out — holding past this point "+
				"reinstates the serialization the sibling-in-flight guard was retired to remove",
				legs[0].ID, occ)
		}
	}
}

// TestDwell_ClaimsItsSlotAtTheMomentItChoosesIt is §R.71's rider 1, pinned on the
// only thing a single-threaded fixture can observe directly: the claim itself.
//
// TWO DWELLERS RELEASED TOGETHER MUST NOT PICK ONE SLOT. Between findShuffleSlots
// returning a slot and delivery_node being written there is a window in which
// that slot still reads free to every other chooser, and two dwellers stand in
// different lanes — so they are released under different mutexes and nothing else
// serializes them. The 2026-07-13 specimen is what that window costs: two digs
// picked SMN_008/SMN_009 three seconds apart, a blocker landed on a blocker,
// EvictStaleGhostsTx threw a bin to _TRANSIT, two bins orphaned.
//
// The DIVERSION half — two dwellers, two distinct slots — is pinned end to end by
// TestFindShuffleSlots_TwoDigsDivertOntoDifferentSlots. What that test cannot
// show is WHY it holds under contention, because a sequential fixture never
// enters the window. This one asserts the mechanism instead: the exclusive claim
// exists on the chosen node the moment the choice is made.
//
// MUTATION: delete the claimStoreSlot call from dwellDestination. This fires —
// the leg is released with a destination nothing holds, which is exactly the
// pre-D83a world the reservation was added to end, one moment later in the
// lifecycle.
func TestDwell_ClaimsItsSlotAtTheMomentItChoosesIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, park, dugSlots, _, bp := setupDwellGroup(t, db, "DWCLAIM", 2, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWCLAIM-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWCLAIM-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwclaim"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWCLAIM-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
	released := releaseDwell(t, d, db, legs[0])
	if released.DeliveryNode != park.Name {
		t.Fatalf("the leg was released onto %q, want %s", released.DeliveryNode, park.Name)
	}

	rows, err := db.ListReservationsByOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "list the released leg's reservations")
	held := false
	for _, r := range rows {
		if r.Kind == reservations.KindSlot && r.NodeID == park.ID {
			held = true
		}
	}
	if !held {
		t.Errorf("leg %d was released onto %s holding no slot reservation on it (%d reservation(s)). "+
			"The choice and the claim have to be one act: a slot chosen and not claimed still reads "+
			"free to the next dweller released in the same breath, and the second blocker lands on "+
			"the first", legs[0].ID, park.Name, len(rows))
	}
}

// TestDwell_WalksPastASlotAdmissionRefuses is the rig's find, pinned.
//
// THE FAILURE, MEASURED 2026-08-13 on the lane-stress rig. findShuffleSlots is
// deterministic and does NOT ask the dig question, so it hands back the same first
// candidate every time — and when that candidate is in a lane another dig holds,
// admission refuses it every time. A resolver that takes one answer and waits for
// the next event therefore re-asks its way into a livelock: seven digs stood
// loaded under `lane-dig-active` for a whole 17-minute window with NINE legal
// slots empty elsewhere in their groups, and 28 orders confirmed against a 113
// baseline. It does not self-clear, because the dig holding the refused lane is
// stuck the same way.
//
// So the resolver walks its candidates. The fixture is the shape in miniature: the
// only slot findShuffleSlots offers first is in a lane a FOREIGN dig holds, and a
// legal one stands behind it.
//
// WHAT THIS IS NOT is the parked right-of-way rule. That is a plan-time
// construction about which digs may START; this is a release-time chooser keeping
// the contract it always had — choose a slot admission admits. A leg whose every
// candidate is refused still waits, which the sibling assertion below pins.
//
// MUTATION: break out of the candidate loop after the first refusal (return the
// verdict instead of excluding and re-asking). This fires — the leg dwells under
// lane-dig-active with the free slot standing empty, which is the rig's state
// reproduced in a fixture.
func TestDwell_WalksPastASlotAdmissionRefuses(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, dug, sib, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWWALK", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWWALK-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWWALK-TGT")

	// A THIRD LANE, ungated and empty: the legal answer standing behind the
	// refused one. Pass 2 fills deepest-first and walks lanes in name order, so
	// DWWALK-SIB is offered before this one.
	lanType, _ := db.GetNodeTypeByCode("LANE")
	open := &nodes.Node{Name: "DWWALK-ZOPEN", NodeTypeID: &lanType.ID, ParentID: &grp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(open), "create the open lane")
	openDepth := 1
	openSlot := &nodes.Node{Name: "DWWALK-ZOPEN-S1", ParentID: &open.ID, Enabled: true, Depth: &openDepth}
	testutil.MustNoErr(t, db.CreateNode(openSlot), "create the open slot")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwwalk"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWWALK-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	// A FOREIGN DIG TAKES THE SIBLING LANE — the lane whose slots the resolver is
	// about to be offered first.
	foreign := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwwalk-foreign" })
	if !d.laneLock.TryLock(sib.ID, foreign.ID) {
		t.Fatal("the foreign dig could not take the sibling lane")
	}

	released := releaseDwell(t, d, db, legs[0])
	if released.DeliveryNode == "" {
		t.Fatalf("the leg is still dwelling (cause %q) with %s standing free and unheld. "+
			"findShuffleSlots offers %s first and admission refuses it, so a resolver that gives up on "+
			"the first refusal re-asks its way into a livelock — seven digs stood loaded for a whole "+
			"window that way", released.QueueCause, openSlot.Name, sib.Name)
	}
	if got := released.DeliveryNode; got != openSlot.Name {
		t.Errorf("the leg was released onto %q, want the one slot no dig holds (%s)", got, openSlot.Name)
	}
	// AND IT DID NOT SIMPLY IGNORE THE DIG. The refused lane's slots are still
	// refused, not merely deprioritised: nothing landed in the foreign dig's lane.
	for _, s := range sibSlots {
		var n int
		testutil.MustNoErr(t, db.DB.QueryRow(
			`SELECT COUNT(*) FROM orders WHERE delivery_node = $1`, s.Name).Scan(&n), "count inbound")
		if n != 0 {
			t.Errorf("%d order(s) are aimed at %s, which a foreign dig holds — walking past a refusal "+
				"must not become ignoring it", n, s.Name)
		}
	}
}

// TestDwell_WaitsWhenEveryCandidateIsRefused is the other half: a leg whose every
// candidate is in a dig-held lane has nowhere it may legally go, so it WAITS —
// holding its blocker, under a cause that names what has to free — and does not
// force its way in or report a group that is not full as full.
//
// ── THE MOMENT MOVED WHEN RIGHT OF WAY LANDED, AND SO DID THE CAUSE ───────
//
// This asserted CauseLaneDigActive, which is ADMISSION refusing a candidate the
// pool had already offered. Right of way (§R.61) removes the candidate one layer
// earlier: findShuffleSlots reads the group through ListChildNodesUnlocked, so a
// foreign dig's lane is not in the pool at all and admission is never asked about
// it. The refusal now comes from the POOL, and its cause is CauseDigHoldsParking.
//
// The releaser is unchanged — that dig releasing that lane — which is why this is
// a re-point and not a behaviour change to argue about. What is genuinely better
// is that the cause now carries WHICH lane in its Where, taken from the typed
// error rather than from the lane the robot happens to be standing in.
//
// COMPOSITION, RECORDED: the walk still runs, and this test no longer reaches its
// exhaustion arm — with dig-held lanes gone from the pool there is nothing left
// for admission to refuse here. TestDwell_WaitsWhenTheWalkIsExhausted below keeps
// that arm pinned, on an occupancy refusal, which is a reason right of way does
// not pre-empt.
//
// MUTATION (verified): delete the errors.As(&DigParkingHeldError) arm in
// dwellDestination. Both assertions fire. Worth recording what the mutant
// actually does, because it is worse than a mislabel: DigParkingHeldError is not
// an ErrNoShuffleSlot, so with that arm gone the shortfall leaves the resolver as
// a hard error and the leg parks under gate-release-failed — a FAULT cause, on a
// robot whose situation is ordinary congestion. Wait-not-fail, lost at the label.
func TestDwell_WaitsWhenEveryCandidateIsRefused(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, _, bp := setupDwellGroup(t, db, "DWALLHELD", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWALLHELD-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWALLHELD-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwallheld"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWALLHELD-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	foreign := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwallheld-foreign" })
	if !d.laneLock.TryLock(sib.ID, foreign.ID) {
		t.Fatal("the foreign dig could not take the sibling lane")
	}

	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	held, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the dwelling leg")
	if held.DeliveryNode != "" {
		t.Fatalf("the leg was released onto %q — every candidate is in a lane a foreign dig holds, and "+
			"walking past a refusal must not turn into overriding it", held.DeliveryNode)
	}
	if QueueCause(held.QueueCause) != CauseDigHoldsParking {
		t.Errorf("the dwelling leg carries cause %q, want %q. The group is NOT full — every slot it "+
			"could reach is in somebody else's excavation, and those two clear on different events",
			held.QueueCause, CauseDigHoldsParking)
	}
	if !strings.Contains(held.QueueReason, sib.Name) {
		t.Errorf("the operator-facing reason is %q and does not name %s — the lane that has to free is "+
			"the sibling, not the one the robot is standing in, and a reason that points at the robot's "+
			"own lane sends an operator to look at the one thing behaving correctly",
			held.QueueReason, sib.Name)
	}
}

// TestDwell_FullGroupIsNotBlamedOnADig is the other half of the cause split, and
// it is what keeps right of way from taking credit for congestion it did not cause.
//
// A dig is running on the sibling lane AND the sibling lane is packed solid. Both
// facts are true; only one of them is the reason. Removing right of way would not
// have found this leg a slot, so the honest cause is CauseNoShuffleSlot — "the
// group is full", which clears when anything anywhere places — and NOT
// CauseDigHoldsParking, which would send an engineer to look at a dig that is
// behaving perfectly and is not in anybody's way.
//
// This is why digHeldParking re-runs the walk over the unfiltered group instead of
// reading the diff and calling any dig-held lane the reason. The first cut did the
// latter, and on a full group with one dig running it would have blamed the dig
// every time.
//
// MUTATION (verified): in digHeldParking, delete the shuffleSlotsFrom re-walk and
// return the DigParkingHeldError as soon as the diff is non-empty. This test's
// cause assertion fires; TestDwell_WaitsWhenEveryCandidateIsRefused stays green,
// which is the point of having both.
func TestDwell_FullGroupIsNotBlamedOnADig(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWFULL", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWFULL-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWFULL-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwfull"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWFULL-LINE").Name
		o.Status = protocol.StatusPending
	})
	// THE DIG PLANS FIRST, against a group that had room — which is the only way
	// to reach the release-time question at all. The plan-time count check would
	// otherwise refuse the dig outright and this would be a test about that.
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	// AND NOW THE POOL CLOSES UNDER IT: the sibling packs solid, and a dig takes
	// it. This is the residual named in findShuffleSlots' header — a pool eaten out
	// from under a dig that had already planned — and the cause has to say which of
	// the two facts is the reason.
	for i, s := range sibSlots {
		createTestBinAtNode(t, db, bp.Code, s.ID, fmt.Sprintf("DWFULL-SIB%d", i))
	}

	foreign := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwfull-foreign" })
	if !d.laneLock.TryLock(sib.ID, foreign.ID) {
		t.Fatal("the foreign dig could not take the sibling lane")
	}

	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	held, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the dwelling leg")
	if held.DeliveryNode != "" {
		t.Fatalf("the leg was released onto %q — the group has no empty slot in it", held.DeliveryNode)
	}
	if QueueCause(held.QueueCause) != CauseNoShuffleSlot {
		t.Errorf("the dwelling leg carries cause %q, want %q. A dig IS holding the sibling lane, and it "+
			"is not the reason — the lane is full, so lifting right of way would find this leg nothing. "+
			"Blaming the dig sends an engineer to the one thing behaving correctly",
			held.QueueCause, CauseNoShuffleSlot)
	}
}

// TestDwell_WaitsWhenTheWalkIsExhausted keeps the candidate walk's own exhaustion
// arm pinned after right of way took the dig case away from it.
//
// The walk exists because admission asks SEVERAL questions and a resolver that
// gives up on the first refusal re-asks its way into a livelock. Right of way
// answers one of those questions earlier, at the pool — but not the others, and
// occupancy is the plainest of them: a robot physically inside the sibling lane
// refuses a candidate that is genuinely in the pool, for a reason no exclusion
// pre-empts.
//
// So the leg walks its whole (short) candidate list, is refused everywhere, and
// must settle under the LAST REFUSAL's cause rather than under "no shuffle slot".
//
// MUTATION (verified): in dwellDestination, drop the `if refused.Cause() != ""`
// arm so exhaustion always reports CauseNoShuffleSlot. The cause assertion fires.
func TestDwell_WaitsWhenTheWalkIsExhausted(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, _, bp := setupDwellGroup(t, db, "DWEXH", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWEXH-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWEXH-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwexh"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWEXH-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	// A ROBOT INSIDE THE SIBLING LANE, not a dig holding it. The lane stays in the
	// pool — this is exactly the case right of way does not remove.
	inside := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwexh-inside" })
	testutil.MustNoErr(t, reservations.AcquireOccupancy(db.DB, inside.ID, sib.ID), "occupy the sibling lane")

	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	held, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the dwelling leg")
	if held.DeliveryNode != "" {
		t.Fatalf("the leg was released onto %q while a robot is inside the only lane with room — "+
			"walking past a refusal must not turn into overriding it", held.DeliveryNode)
	}
	if QueueCause(held.QueueCause) != CauseLaneOccupied {
		t.Errorf("the exhausted walk settled under cause %q, want %q — the last refusal is the true one, "+
			"and it clears when that robot places, not when the group frees a slot",
			held.QueueCause, CauseLaneOccupied)
	}
}

// TestDwell_ComposesWithTheGatedEntry is the composition check §7.3 asks for by
// name, and it is here because "should compose" is not "composes".
//
// TWO FIXES CHANGE WHO HOLDS A CORRIDOR ROW, and they meet on a MARKED dug lane:
//
//	§R.54/R.56 — a compound leg's dispatch takes NO occupancy row on a gated lane,
//	             because the create stops at the mark and the robot is standing
//	             next to the corridor rather than in it. That phantom row refused
//	             the one order that could break a four-order cycle: 997 seconds.
//	§7.3       — a dwelling leg HOLDS its row across the dwell, because it lifts
//	             and stays, and dropping it admits a sibling in behind a robot that
//	             has not moved.
//
// One says "do not take it yet" and the other says "do not drop it yet", and on a
// marked lane the same leg is subject to both in sequence: no row at the create,
// a row at the inbound release (the tail append is when it actually enters), the
// row HELD through the lift and the dwell, and the row dropped when its own tail
// is appended and it drives out. This walks that sequence.
//
// MUTATION: delete the holdsOccupancyThroughDwell early return in
// releaseOccupancyOnExit. The lift assertion fires HERE as well as in
// TestDwell_HoldsItsLaneUntilItDrivesOut — and the two are not redundant, because
// the row's WRITER differs between them. On an unmarked lane the leg's own
// dispatch takes it; on this one the dispatch takes nothing (R.54) and the ENTRY
// APPEND takes it. A fix that held the row only where the dispatch had written it
// would pass the unmarked test and fail this one, which is the composition being
// checked rather than assumed.
func TestDwell_ComposesWithTheGatedEntry(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, park, dugSlots, _, bp := setupDwellGroup(t, db, "DWGATE", 2, true)
	testutil.MustNoErr(t, db.SetNodeProperty(dug.ID, PropLaneGatePoint, "DWGATE-MARK"), "mark the dug lane")
	dug, _ = db.GetNode(dug.ID)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWGATE-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWGATE-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwgate"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWGATE-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig leg was never dispatched (queue_cause %q)", legs[0].QueueCause)
	}

	// ── THE PLAN CARRIES BOTH WAITS ──────────────────────────────────────────
	// The inbound one is spliced before the pickup because the lane is marked; the
	// outbound one is the dwell. Two waits, one plan, released independently —
	// which is the multi-gate shape rule 2 already builds.
	steps := planOf(t, db, legs[0].ID)
	if len(steps) != 3 {
		t.Fatalf("the leg's plan is %+v, want [wait@mark, pickup, wait@dwell]", steps)
	}
	if steps[0].Action != protocol.ActionWait || steps[0].Node != "DWGATE-MARK" {
		t.Fatalf("step 0 is %s@%q, want the inbound wait at the lane's mark", steps[0].Action, steps[0].Node)
	}
	if steps[2].Action != protocol.ActionWait || steps[2].Node != dugSlots[0].Name {
		t.Fatalf("step 2 is %s@%q, want the outbound dwell at the shallowest slot %s",
			steps[2].Action, steps[2].Node, dugSlots[0].Name)
	}

	// ── THE ROW IS THE APPEND'S, NOT THE CREATE'S ────────────────────────────
	//
	// The lane was open, so the valve appended the entry tail back to back with the
	// create — which is what makes an open gate invisible — and the row exists by
	// the time this reads it. That is R.54's fix working, not bypassing it: the
	// row's WRITER is appendGateTail, and the leg's own dispatch took nothing
	// (enteredAtDispatch skips a gated lane). The state where the robot stands at
	// the mark with no row is the CONTENDED case, and it is pinned separately by
	// TestGatedLeg_TakesNoOccupancyOnTheLaneItStandsOutsideOf; what this test is
	// for is what happens to that row once the dwell begins.
	occ, err := reservations.OccupantsOf(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "occupants after the entry append")
	if len(occ) != 1 || occ[0] != legs[0].ID {
		t.Fatalf("lane %s occupants after the gate opened = %v, want the leg %d — the tail append is "+
			"the moment it actually enters, and the row is that moment's", dug.Name, occ, legs[0].ID)
	}

	// ── §7.3: THE LIFT DOES NOT RELEASE IT ───────────────────────────────────
	liftBin(t, db, d, legs[0], dugSlots[0], transitNode(t, db, "DWGATE-TRANSIT"))
	occ, err = reservations.OccupantsOf(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "occupants after the lift")
	if len(occ) != 1 || occ[0] != legs[0].ID {
		t.Fatalf("lane %s occupants after the lift = %v, want the dwelling leg %d still holding. The two "+
			"fixes meet here: the row this leg holds was taken by the APPEND rather than the create, and "+
			"it must survive the lift because the robot does not leave at the lift any more",
			dug.Name, occ, legs[0].ID)
	}

	// ── AND THE DWELL'S OWN RELEASE DROPS IT ─────────────────────────────────
	released := releaseDwell(t, d, db, legs[0])
	if released.DeliveryNode != park.Name {
		t.Fatalf("the leg was released onto %q, want %s", released.DeliveryNode, park.Name)
	}
	occ, err = reservations.OccupantsOf(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "occupants after the dwell released")
	for _, o := range occ {
		if o == legs[0].ID {
			t.Errorf("lane %s still records the dwelling leg %d as inside it after its tail was "+
				"appended (occupants %v) — the row taken by the entry append must be dropped by the "+
				"dwell's own release, or the two fixes together hold a corridor for the whole leg",
				dug.Name, legs[0].ID, occ)
		}
	}
}

// TestFlip2_TheDigKeepsItsLaneWhileALegDwellsInIt is §7.4, fused into the dwell
// because the dwell is the case that breaks the predicate it was proposed with.
//
// FLIP 2 releases the dug lane's claim when the last blocker LEAVES THE LANE
// rather than when the compound terminates — the compound's last act is usually a
// drive to a line, and holding a corridor shut for the length of a journey that
// has already left it is the cost being removed.
//
// THE PREDICATE IT WAS PROPOSED WITH — "any non-terminal leg still picks from this
// lane" — is false at exactly the wrong moment under the dwell: the dwelling leg
// HAS picked, so the event that would fire the release is the dwell's own lift,
// with a loaded robot standing in the corridor. So this test drives the two
// moments separately and asserts opposite things about them.
//
// EXTENDED TO THE HANDOFF. The test walks a SERVICE dig's corridor through its
// whole life — lift, drive-out, land — because the excavation ending is not the
// corridor ending: the bin the dig uncovered is standing at an open mouth, and
// what protects it is that the lane CHANGES HANDS to the order collecting it, as
// that order's own outbound hold, rather than being held by a dig that never
// terminates.
//
// The four moments, and what each one is proving:
//
//	LIFT        the dig claim survives a dweller's own pick (§7.4, unchanged)
//	DRIVE-OUT   the corridor passes to the collector, still shut to drops
//	LAND        the dig parent TERMINATES — it owes nothing and holds nothing
//	COLLECTED   the collector's own hold is the last thing, and it ends with it
//
// MUTATION: delete the holdsOccupancyThroughDwell arm from legStillNeedsLane. The
// first assertion fires — the claim drops at the LIFT, while the robot is still
// standing in the lane, and the dig's own remaining legs (exempt from their
// parent's lock by design) are free to queue in behind it.
//
// MUTATION: make handOffDugLane return false unconditionally. The DRIVE-OUT
// assertion fires — the lane goes fully free with the uncovered bin standing in
// it, which is the re-burial window.
func TestFlip2_TheDigKeepsItsLaneWhileALegDwellsInIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp, dug, _, park, dugSlots, _, bp := setupDwellGroup(t, db, "DWFLIP", 2, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWFLIP-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWFLIP-TGT")

	// A SERVICE DIG: one blocker, no retrieve. The claim's fate then depends only
	// on that leg, which is what makes the two moments below separable — a plain
	// buried retrieve would keep the lane for its own tail's target and hide the
	// arm this test is about.
	//
	// IT CARRIES AN ORIGIN, and that is what makes it the collector rather than
	// fixture decoration: createServiceDigParent inherits the requester's origin,
	// and the episode is the only tie a dig has to the demand it was raised for.
	// A buried demand records nothing else — no claim on the bin, no reservation,
	// no source_node naming the slot — because it cannot claim what it cannot
	// reach.
	requester := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwflip-req"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWFLIP-LINE").Name
		o.OriginID = "44444444-4444-4444-4444-444444444444"
		o.OriginClass = "demand"
		o.Status = protocol.StatusPending
	})
	plan, err := PlanLaneMouthClear(db, dugSlots[1], dug, grp.ID, reservations.Anyone)
	testutil.MustNoErr(t, err, "plan the lane clear")
	parent, err := d.createServiceDigParent(dug, dugSlots[1], requester, plan)
	testutil.MustNoErr(t, err, "create the service dig parent")
	if !d.laneLock.TryLock(dug.ID, parent.ID) {
		t.Fatal("the dig could not take the lane it is about to excavate")
	}
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "create the dig")

	legs := legsOf(t, db, parent.ID)
	if len(legs) != 1 {
		t.Fatalf("the service dig has %d leg(s), want exactly the blocker's unbury", len(legs))
	}
	if legs[0].VendorOrderID == "" {
		t.Fatalf("the dig's leg never went out (queue_cause %q)", legs[0].QueueCause)
	}

	// ── THE LIFT: the bin is off the floor, the robot is not out of the lane ──
	liftBin(t, db, d, legs[0], dugSlots[0], transitNode(t, db, "DWFLIP-TRANSIT"))
	if !d.laneLock.IsLocked(dug.ID) {
		t.Fatalf("the dig released lane %s at the LIFT, while its leg is standing in the lane holding "+
			"the blocker. 'Any non-terminal leg still picks from this lane' reads false the moment a "+
			"dweller lifts — and a sibling leg is exempt from its own parent's dig lock by design, so "+
			"the population this claim excludes is precisely the one that would queue in behind it",
			dug.Name)
	}

	// ── THE DRIVE-OUT: Core chooses a destination and the robot leaves ────────
	released := releaseDwell(t, d, db, legs[0])
	if released.DeliveryNode != park.Name {
		t.Fatalf("the leg was released onto %q, want the free parking %s", released.DeliveryNode, park.Name)
	}

	// ── AND THE CORRIDOR CHANGES HANDS ────────────────────────
	//
	// The sentence that stood here was: "lane %s is still dig-claimed after its
	// last blocker left it. What remains of this compound is transport — the robot
	// is driving to %s — and holding the corridor for that is the cost flip 2
	// exists to remove."
	//
	// It is quoted rather than deleted because it was right about transport and
	// incomplete about this shape. This is a SERVICE dig: it owns no retrieve, so
	// when its blocker leaves, DWFLIP-TGT — the bin the excavation was raised to
	// uncover — is standing at an open lane mouth. Releasing outright here is the
	// re-burial window; holding it under the dig is a finished order that never
	// terminates. So the lane is HANDED to the order collecting the bin, and it is
	// that order's hold from this moment on.
	if owner := digHoldOwner(t, db, dug.ID); owner != 0 {
		t.Fatalf("lane %s is still held as a DIG (by order %d) after the excavation finished. The "+
			"excavation is over — what remains is somebody else's pickup — so the corridor belongs to "+
			"the collector now, not to a dig with nothing left to do in it", dug.Name, owner)
	}
	holders, err := reservations.ActiveMouthRows(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth holds after the handoff")
	if len(holders) != 1 || holders[0].OrderID != requester.ID || holders[0].Mode != reservations.ModeOutbound {
		t.Fatalf("lane %s holds %+v after its dig finished, want exactly one OUTBOUND hold for the "+
			"collector %d. Outbound is the whole mechanism: it excludes a drop into the lane — which "+
			"is the only way the uncovered bin can be re-buried — while admitting the collector's own "+
			"dispatch idempotently", dug.Name, holders, requester.ID)
	}

	// ── LAND: AND THE DIG TERMINATES ON THE ORDINARY PATH ───────────────
	//
	// It owes nothing and holds nothing, so there is no arm to refuse it and no
	// re-drive to un-stick it later. A dig that held past this point was a third
	// non-terminal state — invisible to every stall checker, holding a corridor on
	// behalf of a demand it could not ask about — and that is what the handoff
	// above exists to make unnecessary.
	//
	// AdvanceCompoundOrder IS CALLED EXPLICITLY, and that is not fixture noise.
	// landLeg terminalizes the leg through the store, which is not the path that
	// re-drives the parent — in production the leg's terminal event is what calls
	// this.
	landLeg(t, d, db, legs[0])
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "re-drive the parent after its last leg landed")

	done, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the dig parent")
	if !protocol.IsTerminal(done.Status) {
		t.Fatalf("the dig parent is %q with every child terminal and its corridor already handed to "+
			"order %d. A finished dig that will not finish is a permanent row every census and every "+
			"stall checker has to learn to ignore", done.Status, requester.ID)
	}
	// AND THE PARENT'S OWN TEARDOWN MUST NOT TAKE THE HANDED-OVER ROW WITH IT.
	// unlockLaneForCompound releases by OWNER and the row's owner changed, so this
	// is structural rather than lucky — asserted because if it ever stops being
	// true the corridor opens at the exact moment the dig ends.
	holders, err = reservations.ActiveMouthRows(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth holds after the dig terminated")
	if len(holders) != 1 || holders[0].OrderID != requester.ID {
		t.Fatalf("lane %s holds %+v after the dig terminated, want the collector %d's hold to survive "+
			"the dig's teardown", dug.Name, holders, requester.ID)
	}

	// ── COLLECTED: the collector's hold is the last thing, and it ends with it ─
	//
	// The corridor is now inside an ordinary order's lifetime, which is the point:
	// however that order ends — collected, cancelled, failed — its rows go with it,
	// so no hold can outlive the work it was taken for. That is the property the
	// dig-held version could not have, because a dig holding for a bin has no
	// lifetime left of its own.
	testutil.MustNoErr(t, db.FailOrderAtomic(requester.ID, "collector went away"),
		"terminalize the collector")
	holders, err = reservations.ActiveMouthRows(db.DB, dug.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth holds after the collector terminated")
	if len(holders) != 0 {
		t.Errorf("lane %s still holds %+v after its collector %d reached a terminal status. A hold "+
			"that survives its owner is the stranded corridor the handoff exists to make impossible",
			dug.Name, holders, requester.ID)
	}
}

// digHoldOwner reads the dig-mode mouth hold on a lane, 0 when there is none.
func digHoldOwner(t *testing.T, db *store.DB, laneID int64) int64 {
	t.Helper()
	owner, err := reservations.DigHoldOwner(db.DB, laneID)
	testutil.MustNoErr(t, err, "read the dig hold owner")
	return owner
}

// TestDwell_TheChosenSlotCannotBeBuriedBeforeTheRobotArrives is the regression
// test for the specimen the shape was written against.
//
// THE OLD FAILURE: the leg was dispatched carrying delivery_node = a slot in
// another lane, and nothing held it — shuffleSlotFree says the non-reservation is
// deliberate, and the burial test that would have protected it is a plan-time
// snapshot by its own admission ("the set is computed once above"). Two ordinary
// stores into shallower slots in that lane while the robot was still working the
// first one, and the leg arrived at a slot it could not reach: admission refuses
// with lane-target-buried and handleStaleDigLeg has two dispositions and no good
// one — hold indefinitely, or dissolve and re-plan the entire parent dig.
//
// UNDER THE DWELL IT CANNOT ARISE, and that is a structural claim rather than a
// probabilistic one: there is no interval between choosing the slot and driving
// to it, because the choice is made when the robot is standing ready to drive. So
// the fixture buries what WOULD have been chosen, at the moment it would have been
// chosen, and the leg simply picks somewhere else.
//
// MUTATION: bind the destination at plan time again (have planUnbury write
// shuffleSlots[i] onto each step's ToNode). The leg is then aimed at the slot this
// fixture buries, and it arrives to find it walled — the dissolve path.
func TestDwell_TheChosenSlotCannotBeBuriedBeforeTheRobotArrives(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	// A sibling lane AND a parking node: the sibling's deepest slot is what a
	// plan-time pick takes (pass 2 fills deepest-first), and the parking is where
	// there is still to go once that slot is walled. Without the second option the
	// test would only be proving that a full group waits.
	_, dug, sib, park, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWBURY", 2, true)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWBURY-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWBURY-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwbury"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWBURY-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
	if legs[0].DeliveryNode != "" {
		t.Fatalf("the leg is aimed at %q at plan time — the window this test is about is back",
			legs[0].DeliveryNode)
	}

	// THE BURIAL. Pass 2 fills deepest-first, so the slot a plan-time pick would
	// have taken is the sibling lane's DEEPEST — and an ordinary store into the
	// slot in FRONT of it is what used to wall the leg out.
	deepest := sibSlots[len(sibSlots)-1]
	createTestBinAtNode(t, db, bp.Code, sibSlots[0].ID, "DWBURY-WALL")

	// The robot arrives and Core chooses NOW, against the lane as it stands.
	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	released, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload after the release")
	if released.DeliveryNode == "" {
		t.Fatalf("the leg is still dwelling (cause %q) — %s's deepest slot is walled, but the group's "+
			"parking %s is standing empty, so there IS somewhere to go", released.QueueCause, sib.Name, park.Name)
	}
	if released.DeliveryNode == deepest.Name {
		t.Fatalf("Core chose %s, which is behind the bin that just landed at %s. A leg cannot reach a "+
			"slot with a bin in front of it: admission refuses with lane-target-buried and the leg goes "+
			"to handleStaleDigLeg, whose two dispositions are hold forever or dissolve the whole parent "+
			"dig. Choosing at release is supposed to remove the window this arrives through",
			released.DeliveryNode, sibSlots[0].Name)
	}

	// AND IT IS REACHABLE, which is the positive form of the same claim.
	dest, err := db.GetNodeByDotName(released.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the chosen destination")
	acc, err := db.IsSlotAccessible(dest.ID)
	testutil.MustNoErr(t, err, "ask whether the chosen slot is reachable")
	if !acc {
		t.Errorf("Core chose %s, which is not reachable — the choice is made against fresh state, so "+
			"an unreachable answer means it read something stale", dest.Name)
	}
	_ = dug
}

// ── LAW 10'S DUAL FOR §R.76: NO BIN EVER RETURNS TO A DUG LANE ─────────────
//
// These two tests are a pair and neither is worth anything alone. The first
// proves the assertion FIRES on the one destination that must never be chosen;
// the second proves it is not simply refusing everything, which is how a guard
// passes review while protecting nothing.
//
// THEY BREAK THIS FILE'S OWN RULE, ON PURPOSE, AND HERE IS WHY. The header says
// every fixture drives production calls because "a test that can reach a state
// the plant cannot is testing something else". These call bindChosenDestination
// directly, because the state under test is exactly one the plant cannot reach:
// shuffleSlotsFrom's Pass 2 skips the dug lane, so no production path offers this
// destination. That is the point. The assertion exists for a FUTURE writer — one
// who deletes that skip, or adds a second chooser — and a dual that can only fire
// through today's callers would go green on the day the guard stops being
// reachable and tell nobody. The rule stands for every other test here.
//
// WHAT IS BEING DEFENDED is the retreat: when three digs are each other's only
// source of space, putting one blocker back down at its own lane's mouth looks
// like the symmetric fix, needs no new mode, and is legal by construction. It is
// ruled out — a blocker is what stands between the mouth and the target bin, so
// putting one back re-buries the bin the dig was raised to uncover.

// TestNoReturnToADugLane_TheAssertionFires is the red half.
//
// The fixture makes the retreat maximally attractive: the blocker is LIFTED
// first, so the mouth slot it came from is genuinely empty, unclaimed and
// reachable — every ordinary reason to refuse it is gone, and the only thing
// standing between the leg and its old slot is the invariant.
//
// MUTATION: delete the guard at the top of bindChosenDestination. The leg then
// claims the slot it just emptied and binds a tail driving the blocker back into
// the lane the dig is digging, which is the dig undone.
func TestNoReturnToADugLane_TheAssertionFires(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWNORET", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWNORET-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWNORET-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwnoret"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWNORET-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	// LIFT IT, so the slot the retreat wants is empty and legal.
	liftBin(t, db, d, legs[0], dugSlots[0], transitNode(t, db, "DWNORET-TRANSIT"))

	_, _, err := d.bindChosenDestination(legs[0], dug, dugSlots[0])
	if err == nil {
		t.Fatalf("bindChosenDestination accepted %s — a slot INSIDE %s, the lane leg %d is digging. "+
			"The blocker would go back where it came from, in front of the target the dig exists to "+
			"uncover, and the excavation would be undone at its last step",
			dugSlots[0].Name, dug.Name, legs[0].ID)
	}
	if !strings.Contains(err.Error(), dug.Name) {
		t.Errorf("the refusal is %q, which does not name %s. A construction fault has to say which "+
			"lane it was about or the next reader cannot act on it", err.Error(), dug.Name)
	}

	// AND NOTHING WAS WRITTEN. A guard that refuses after claiming the slot has
	// still taken the slot out of the pool for everybody else.
	reloaded, gErr := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, gErr, "reload the refused leg")
	if reloaded.DeliveryNode != "" {
		t.Errorf("the refused leg carries delivery_node %q — the guard fired after the write instead "+
			"of before it", reloaded.DeliveryNode)
	}
	rows, rErr := db.ListReservationsByOrder(legs[0].ID)
	testutil.MustNoErr(t, rErr, "list the refused leg's reservations")
	for _, r := range rows {
		if r.Kind == reservations.KindSlot && r.NodeID == dugSlots[0].ID {
			t.Errorf("the refused leg holds a slot reservation on %s — it claimed the slot it was "+
				"about to be refused for, and nobody else can use it now", dugSlots[0].Name)
		}
	}
	_ = sibSlots
}

// TestNoReturnToADugLane_ALegalSlotStillBinds is the green half, and it is what
// stops the guard above from being satisfied by a function that refuses
// everything. Same fixture, same call, one field different: a slot in the SIBLING
// lane, which is where a blocker is supposed to go.
//
// MUTATION: widen the guard to any lane in the group (drop the `== lane.ID`
// comparison and refuse whenever dest has a lane parent). This fires — the dig
// can no longer park anywhere at all, which is the famine made permanent.
func TestNoReturnToADugLane_ALegalSlotStillBinds(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWNORETOK", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWNORETOK-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWNORETOK-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwnoretok"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWNORETOK-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)
	liftBin(t, db, d, legs[0], dugSlots[0], transitNode(t, db, "DWNORETOK-TRANSIT"))

	dest, verdict, err := d.bindChosenDestination(legs[0], dug, sibSlots[0])
	testutil.MustNoErr(t, err, "bind a legal destination in "+sib.Name)
	if !verdict.Admitted() {
		t.Fatalf("a slot in %s was refused with %q — the guard is refusing legal parking, not the "+
			"dug lane", sib.Name, verdict.Cause())
	}
	if dest == nil || dest.ID != sibSlots[0].ID {
		t.Fatalf("bound %v, want %s", dest, sibSlots[0].Name)
	}
	reloaded, gErr := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, gErr, "reload the bound leg")
	if reloaded.DeliveryNode != sibSlots[0].Name {
		t.Errorf("the leg's delivery_node is %q, want %s — the choice was not written down",
			reloaded.DeliveryNode, sibSlots[0].Name)
	}
}

// TestDwell_TheDigReleaseWakesTheDwellerItWasBlocking is §R.76 arm 5: the
// releaser a dweller is waiting on has to actually reach it.
//
// THE GAP THIS PINS. A leg parked under `dig-holds-parking` is standing in ONE
// lane and naming ANOTHER — the lane whose dig holds its parking. The dig-lock
// release evaluated only the lane it freed, so the cause whose entire definition
// is "another dig holds the parking" had a release that re-asked everybody except
// the robots waiting on it. The engine's group fan-out covered it by accident,
// because flip 2's release usually rides a bin entering transit and that event
// fans out separately — an accident that stops working the moment a dig releases
// for any other reason, and the teardown path is exactly that reason.
//
// The fixture removes the accident: nothing moves, no bin changes slot, no order
// terminates. The ONLY thing that happens is the foreign dig letting go of the
// sibling lane. If the dweller is still parked afterwards, it is asleep next to
// open shelves.
//
// MUTATION (verified): delete the EvaluateDwellersSharingGroupWith call from
// maybeReleaseDigOnLastBlockerOut. The leg stays parked with an empty
// delivery_node and both sibling slots standing free, released only by the
// 60-second floor.
//
// ON THE RIVAL DIG BEING A BARE LOCK WITH NO LEGS. It stands in for a real plant
// state and a narrow one: a dig whose excavation is DONE — blockers out, lane
// empty — still holding its lane until flip 2 drops the claim. That is exactly
// the moment under test, and it cannot be built any other way here, because a dig
// with legs still to run is a dig whose lane still has bins in it, and then there
// is no space for the release to reveal.
//
// It does NOT stand in for a dig that owes capacity. Owing is derived from a
// dig's open legs, so a legless one owes nothing, and
// any test about the usable-capacity claim built on this fixture would pass
// without exercising it. Those live in dig_admission_capacity_docker_test.go and
// use real compounds for that reason.
func TestDwell_TheDigReleaseWakesTheDwellerItWasBlocking(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, sib, _, dugSlots, sibSlots, bp := setupDwellGroup(t, db, "DWWAKE", 2, false)

	createTestBinAtNode(t, db, bp.Code, dugSlots[0].ID, "DWWAKE-BLK")
	createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, "DWWAKE-TGT")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dwwake"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.DeliveryNode = lineNode(t, db, "DWWAKE-LINE").Name
		o.Status = protocol.StatusPending
	})
	legs := planDigFor(t, db, d, bp, dug, dugSlots[1], demand)

	foreign := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "dwwake-foreign" })
	if !d.laneLock.TryLock(sib.ID, foreign.ID) {
		t.Fatal("the foreign dig could not take the sibling lane")
	}

	// PARK IT, and confirm the premise before testing the release.
	d.EvaluateWaitLaneForStagedOrder(legs[0].ID)
	parked, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the parked leg")
	if QueueCause(parked.QueueCause) != CauseDigHoldsParking {
		t.Fatalf("the fixture did not reach the state under test: cause is %q, want %q",
			parked.QueueCause, CauseDigHoldsParking)
	}

	// THE ONLY EVENT: the foreign dig lets go. No bin moves, nothing terminates.
	d.maybeReleaseDigOnLastBlockerOut(sib.ID)

	woken, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the leg after the dig released")
	if woken.DeliveryNode == "" {
		t.Fatalf("the leg is still parked (cause %q) after the dig holding its parking released %s. "+
			"That release IS this cause's releaser — a robot standing in a lane holding a blocker "+
			"with two free slots in front of it, waiting for the 60-second floor to notice",
			woken.QueueCause, sib.Name)
	}
	inSibling := false
	for _, s := range sibSlots {
		if woken.DeliveryNode == s.Name {
			inSibling = true
		}
	}
	if !inSibling {
		t.Errorf("the leg woke onto %q, which is not one of %s's slots — it was released, but not "+
			"into the space that actually freed", woken.DeliveryNode, sib.Name)
	}
	_ = dug
}
