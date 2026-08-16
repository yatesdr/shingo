package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet"
)

// lane_gate_plan_pure_test.go — the pure half of the gated plan: how waits are
// counted, and what a wait gates.
//
// -- THIS FILE'S ORIGINAL PREMISE WAS OVERTURNED TWICE, AND BOTH ARE WORTH
//    STATING BECAUSE BOTH WERE CITED FORWARD ---------------------------------
//
// It first argued that no order carries both a GATE wait and a STATION wait,
// "disjoint by order class", and concluded that the fence could key on
// IsGateStaged rather than on a per-step marker — calling such a marker "the part
// that lies". The disjointness was ARRANGED, not structural: it held only because
// upstream exclusions kept coordinated orders out of the valve. The marker it
// argued against is now what the fence rests on (resolvedStep.WaitKind).
//
// It then pinned the one-wait shape of the two AUTHORED plan builders. Those
// builders are gone: the valve no longer writes plans, it splices a wait into
// whatever plan an order already has (spliceLaneWait). The one-gated-lane rule
// survives as a splice-time refusal, tested against the splice itself in
// lane_gate_splice_docker_test.go, because the splice needs a database to resolve
// which lane a step enters.
//
// What is left here is genuinely pure and genuinely load-bearing: the two
// functions that enumerate waits, and the agreement between them.

// TestWaitAt_CountsLikeSplitSegment is the agreement the fence depends on and
// the reason waitAt lives beside splitSegment.
//
// waitAt names the wait an order is PARKED AT; splitSegment releases what follows
// that same wait. If the two ever enumerated waits differently, the predicate
// would describe a different wait than the release acted on — and the two shapes
// most likely to break it are a BARE wait (no node, emits no block) and an
// operator wait sitting before the lane wait, which is precisely the plan the
// splice creates.
//
// MUTATION (verified): make waitAt skip bare waits (`s.Node == ""` continue).
// The mixed-plan case fires — waitAt names the gate wait where splitSegment is
// still releasing past the operator's.
func TestWaitAt_CountsLikeSplitSegment(t *testing.T) {
	t.Parallel()
	// An operator wait FIRST (bare — the "tooling done" gate Edge emits with no
	// node), then a lane wait. wait_index 0 is the operator's, 1 is the lane's.
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "LINE"},
		{Action: protocol.ActionWait}, // bare operator wait
		{Action: protocol.ActionWait, Node: "GATE", WaitKind: WaitKindLane, WaitLane: 9},
		{Action: protocol.ActionDropoff, Node: "SLOT"},
	}

	w0, ok := waitAt(plan, 0)
	if !ok || w0.WaitKind != "" {
		t.Errorf("wait_index 0: got %+v (ok=%v), want the bare OPERATOR wait — a bare wait emits no "+
			"RDS block but is still a split point, and splitSegment counts it", w0, ok)
	}
	w1, ok := waitAt(plan, 1)
	if !ok || w1.WaitKind != WaitKindLane {
		t.Errorf("wait_index 1: got %+v (ok=%v), want the LANE wait", w1, ok)
	}
	if _, ok := waitAt(plan, 2); ok {
		t.Error("wait_index 2 named a wait; the plan holds two")
	}

	// The consumer's side of the same enumeration: the segment released at
	// wait_index 0 must end at the wait waitAt names at index 1.
	seg, moreWaits, _ := splitSegment(plan, 0)
	if !moreWaits {
		t.Fatal("splitSegment says no wait follows index 0, but waitAt names one at index 1 — the " +
			"predicate and the release disagree about how many waits this plan has")
	}
	if len(seg) == 0 || seg[len(seg)-1].WaitKind != WaitKindLane {
		t.Errorf("segment after wait 0 = %+v; it should run up to and include the lane wait that "+
			"waitAt names at index 1", seg)
	}
}

// TestLaneEntryAfterWait_ReadsDirectionOffThePlan pins the third reader of that
// same enumeration.
//
// Candidate discovery and the valve both ask "what is this robot going in to
// do", and both answer it from the plan rather than from which query found the
// order or which end the lane is on: a PICKUP after the wait is a retrieve (it
// takes something out), a DROPOFF is a store. That step is also the lane-relevant
// node the classifier reads and the depth sort orders on.
//
// MUTATION (verified): return the first actionable step from index 0 instead of
// from after the wait. The store case reports the LINE pickup and its direction
// inverts.
func TestLaneEntryAfterWait_ReadsDirectionOffThePlan(t *testing.T) {
	t.Parallel()

	// A store: pickup at a line, wait, then the drop INTO the lane.
	store := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "LINE"},
		{Action: protocol.ActionWait, Node: "GATE", WaitKind: WaitKindLane, WaitLane: 7},
		{Action: protocol.ActionDropoff, Node: "SLOT-0"},
	}
	entry, isRetrieve, ok := laneEntryAfterWait(store, 0)
	if !ok || isRetrieve || entry.Node != "SLOT-0" {
		t.Errorf("store: entry=%+v retrieve=%v ok=%v, want the SLOT-0 dropoff and store direction",
			entry, isRetrieve, ok)
	}

	// A retrieve: nothing legal to do before the lane opens, so the wait is first
	// and the pickup follows it.
	retrieve := []resolvedStep{
		{Action: protocol.ActionWait, Node: "GATE", WaitKind: WaitKindLane, WaitLane: 7},
		{Action: protocol.ActionPickup, Node: "SLOT-1"},
		{Action: protocol.ActionDropoff, Node: "LINE"},
	}
	entry, isRetrieve, ok = laneEntryAfterWait(retrieve, 0)
	if !ok || !isRetrieve || entry.Node != "SLOT-1" {
		t.Errorf("retrieve: entry=%+v retrieve=%v ok=%v, want the SLOT-1 pickup and retrieve direction",
			entry, isRetrieve, ok)
	}

	// A plan whose LANE wait is the second one: the operator's wait must not be
	// mistaken for it, which is the same counting agreement as above.
	mixed := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "LINE"},
		{Action: protocol.ActionWait},
		{Action: protocol.ActionWait, Node: "GATE", WaitKind: WaitKindLane, WaitLane: 7},
		{Action: protocol.ActionDropoff, Node: "SLOT-2"},
	}
	entry, isRetrieve, ok = laneEntryAfterWait(mixed, 1)
	if !ok || isRetrieve || entry.Node != "SLOT-2" {
		t.Errorf("mixed: entry=%+v retrieve=%v ok=%v, want the SLOT-2 dropoff after the SECOND wait",
			entry, isRetrieve, ok)
	}

	// Nothing actionable after the wait is not a direction, it is a broken plan.
	if _, _, ok := laneEntryAfterWait([]resolvedStep{{Action: protocol.ActionWait}}, 0); ok {
		t.Error("a wait with no actionable step after it reported an entry")
	}
}

// TestSplicedPlan_BlockOffsetsContinue is the traced example two reviewers asked
// for: a multi-step spliced plan walked through splitSegment's offset arithmetic,
// proving appended block ids continue the create's numbering rather than
// colliding with it.
//
// SEER's one contract on block ids is uniqueness, so a collision is the failure
// mode that matters and it is silent until the fleet rejects the append.
//
// The plan, as the splice leaves it (5 steps, lane entry interior):
//
//	0 pickup  @LINE-A     -> block 1   |
//	1 wait    @GATE       -> block 2   |  the unsealed CREATE (splitAtWait)
//	2 dropoff @SLOT-0     -> block 3   |
//	3 pickup  @SLOT-1     -> block 4   |  the TAIL (splitSegment at wait 0)
//	4 dropoff @LINE-B     -> block 5   |
//
// blockOffset for the tail must therefore be 2 — the number of block-producing
// steps before it — so the tail starts at 3 and nothing repeats.
func TestSplicedPlan_BlockOffsetsContinue(t *testing.T) {
	t.Parallel()
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "LINE-A"},
		{Action: protocol.ActionWait, Node: "GATE", WaitKind: WaitKindLane, WaitLane: 3},
		{Action: protocol.ActionDropoff, Node: "SLOT-0"},
		{Action: protocol.ActionPickup, Node: "SLOT-1"},
		{Action: protocol.ActionDropoff, Node: "LINE-B"},
	}

	preWait, hasWait := splitAtWait(plan)
	if !hasWait || len(preWait) != 2 {
		t.Fatalf("preWait = %+v (hasWait=%v), want the pickup and the wait", preWait, hasWait)
	}
	createBlocks := stepsToBlocks("V1", preWait, 0, nil)
	if len(createBlocks) != 2 {
		t.Fatalf("create blocks = %d, want 2 — a wait WITH a node is a real block", len(createBlocks))
	}

	segment, moreWaits, blockOffset := splitSegment(plan, 0)
	if moreWaits {
		t.Error("moreWaits = true; this plan holds exactly one wait, so the tail seals the order")
	}
	if blockOffset != 2 {
		t.Fatalf("blockOffset = %d, want 2 (the pickup and the wait both produce blocks)", blockOffset)
	}
	tailBlocks := stepsToBlocks("V1", segment, blockOffset, nil)
	if len(tailBlocks) != 3 {
		t.Fatalf("tail blocks = %d, want 3", len(tailBlocks))
	}

	seen := map[string]bool{}
	var ids []string
	all := append(append([]fleet.OrderBlock{}, createBlocks...), tailBlocks...)
	for _, b := range all {
		if seen[b.BlockID] {
			t.Fatalf("duplicate block id %q across create and tail (%v) — SEER rejects the append "+
				"outright and the robot strands at the gate", b.BlockID, ids)
		}
		seen[b.BlockID] = true
		ids = append(ids, b.BlockID)
	}
	want := []string{"V1-b1", "V1-b2", "V1-b3", "V1-b4", "V1-b5"}
	for i, w := range want {
		if ids[i] != w {
			t.Errorf("block %d = %q, want %q (ids: %v)", i, ids[i], w, ids)
		}
	}
}
