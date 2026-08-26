package dispatch

import (
	"encoding/json"
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
	entry, _, isRetrieve, ok := laneEntryAfterWait(store, 0)
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
	entry, _, isRetrieve, ok = laneEntryAfterWait(retrieve, 0)
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
	entry, _, isRetrieve, ok = laneEntryAfterWait(mixed, 1)
	if !ok || isRetrieve || entry.Node != "SLOT-2" {
		t.Errorf("mixed: entry=%+v retrieve=%v ok=%v, want the SLOT-2 dropoff after the SECOND wait",
			entry, isRetrieve, ok)
	}

	// Nothing actionable after the wait is not a direction, it is a broken plan.
	if _, _, _, ok := laneEntryAfterWait([]resolvedStep{{Action: protocol.ActionWait}}, 0); ok {
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

// TestEveryWaitDeclaresAnOwner is W1's drift test on the Core side, and it is
// armed on the dispatch path rather than living only here — spliceLaneWait calls
// it before returning a plan Core is about to persist.
//
// Three cases, and the middle one is the whole design:
//
//	lane / station  → owned, accepted
//	""              → the DRAIN WINDOW. Plans authored before the stamp existed
//	                  are still in flight and cannot be failed for a field they
//	                  could not have had. Loud, allowed, and read as the
//	                  station's — the historical default.
//	anything else   → refused. An unrecognised owner can only be a new author
//	                  disagreeing with the vocabulary, and a wait no fence claims
//	                  is one no floor sweeps and no board can render.
//
// When the drain window closes, the "" arm becomes an error and
// IsStationWait's `== ""` arm goes with it — in the same commit, or an untagged
// wait passes here while being unowned at the fence.
func TestEveryWaitDeclaresAnOwner(t *testing.T) {
	t.Parallel()

	owned := []resolvedStep{
		{Action: protocol.ActionWait, Node: "MARK", WaitKind: WaitKindLane, WaitLane: 7},
		{Action: protocol.ActionPickup, Node: "SLOT"},
		{Action: protocol.ActionWait, Node: "STAGING", WaitKind: WaitKindStation},
	}
	if err := assertEveryWaitDeclaresAnOwner(owned); err != nil {
		t.Errorf("a plan whose waits are all owned was refused: %v", err)
	}

	// The drain window: allowed, and IsStationWait is what reads it.
	untagged := []resolvedStep{{Action: protocol.ActionWait, Node: "OLD-PLAN"}}
	if err := assertEveryWaitDeclaresAnOwner(untagged); err != nil {
		t.Errorf("an untagged wait was refused during the drain window: %v — orders authored before "+
			"the field exists cannot be failed for not having it", err)
	}
	if !IsStationWait("") {
		t.Error("IsStationWait(\"\") = false: the drain window's default must be station-owned, which " +
			"is the meaning every pre-ruling plan already had")
	}

	// An owner nobody implements is worse than none: it reads as unowned at
	// every fence while looking deliberate.
	bogus := []resolvedStep{{Action: protocol.ActionWait, Node: "X", WaitKind: "supervisor"}}
	if err := assertEveryWaitDeclaresAnOwner(bogus); err == nil {
		t.Error("a wait declaring an unrecognised owner was accepted. No fence claims it, no floor " +
			"sweeps it, and the board cannot say whether to offer Release — the shape that held three " +
			"robots for a soak")
	}

	// And the two real kinds must not be the same string, or the fence cannot
	// tell them apart at all.
	if WaitKindLane == WaitKindStation {
		t.Fatal("WaitKindLane == WaitKindStation — the whole distinction collapses")
	}
}

// TestHardReleaseIsScopedToCoreOwnedWaits pins W3's scope, which is the half
// that keeps the escape hatch from becoming a foot-gun.
//
// A STATION-owned wait is released from the station's board, by the person who
// can see whether the cell is clear. A Core-side override for one would let an
// engineer advance a robot into an occupied cell from a screen that cannot show
// them the cell — and would do it in the one case where the ordinary path is not
// broken at all.
//
// So CoreOwnsWaitAt is the gate, and both the button (via can_hard_release) and
// the handler read it. An UNTAGGED wait is the station's for the drain window
// and therefore refused: the conservative direction, decided in exactly one
// place (IsStationWait) so the two readers cannot disagree.
func TestHardReleaseIsScopedToCoreOwnedWaits(t *testing.T) {
	t.Parallel()

	plan := func(kind string) string {
		steps := []resolvedStep{
			{Action: protocol.ActionWait, Node: "MARK", WaitKind: kind},
			{Action: protocol.ActionPickup, Node: "SLOT"},
		}
		b, err := json.Marshal(steps)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}

	if !CoreOwnsWaitAt(plan(WaitKindLane), 0) {
		t.Error("a lane wait is CORE's — the hatch exists for exactly this, a wait whose evaluator " +
			"can wedge with nobody else able to clear it")
	}
	if CoreOwnsWaitAt(plan(WaitKindStation), 0) {
		t.Error("a STATION wait was reported as Core-owned. The button would render and the handler " +
			"would accept, letting an engineer advance a robot into a cell somebody is working in " +
			"— from a screen that cannot show them the cell")
	}
	if CoreOwnsWaitAt(plan(""), 0) {
		t.Error("an UNTAGGED wait was reported as Core-owned. During the drain window it reads as " +
			"the station's, and the conservative direction is to withhold the override")
	}
	// Unreadable or absent plans must not offer an override either.
	if CoreOwnsWaitAt("", 0) {
		t.Error("an order with no plan was reported as Core-owned")
	}
	if CoreOwnsWaitAt("{not json", 0) {
		t.Error("an unparseable plan was reported as Core-owned")
	}
	if CoreOwnsWaitAt(plan(WaitKindLane), 7) {
		t.Error("a wait_index past the end was reported as Core-owned — there is no wait there to " +
			"advance")
	}
}
