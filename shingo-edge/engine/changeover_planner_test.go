package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// fullSwapClaim returns a NodeClaim with all the fields the planner needs to
// take the standard swap/evacuate path (i.e. not fall back).
func fullSwapClaim(node, payload string, role protocol.ClaimRole) processes.NodeClaim {
	return processes.NodeClaim{
		CoreNodeName:        node,
		PayloadCode:         payload,
		Role:                role,
		InboundSource:       "SRC_" + node,
		InboundStaging:      "ISTG_" + node,
		OutboundStaging:     "OSTG_" + node,
		OutboundDestination: "ODST_" + node,
	}
}

func TestPlanNodeAction_Swap(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	to := fullSwapClaim("N1", "PART-B", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.OrderA == nil || action.OrderA.Complex == nil {
		t.Fatal("expected OrderA complex spec")
	}
	if action.OrderA.Complex.DeliveryNode != "ISTG_N1" {
		t.Errorf("OrderA delivery = %q, want ISTG_N1", action.OrderA.Complex.DeliveryNode)
	}
	if action.OrderA.Complex.AutoConfirm {
		t.Error("OrderA must not auto-confirm (operator stages)")
	}
	if action.OrderB == nil || action.OrderB.Complex == nil {
		t.Fatal("expected OrderB complex spec")
	}
	if !action.OrderB.Complex.AutoConfirm {
		t.Error("OrderB must auto-confirm (robot completes)")
	}
	if action.NextState != "staging_requested" {
		t.Errorf("NextState = %q, want staging_requested", action.NextState)
	}
	if action.LogTag != "swap" {
		t.Errorf("LogTag = %q, want swap", action.LogTag)
	}
}

func TestPlanNodeAction_Drop(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationDrop,
		FromClaim:    &from,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.OrderA != nil {
		t.Error("Drop should not produce OrderA")
	}
	if action.OrderB == nil || action.OrderB.Complex == nil {
		t.Fatal("Drop should produce OrderB (release)")
	}
	// AutoConfirm=false: the drop order uses the staged-release pattern
	// (wait-at-line → operator releases with partial count → pickup →
	// dropoff). The operator's release click at the lineside is the gate.
	if action.OrderB.Complex.AutoConfirm {
		t.Error("Drop OrderB must NOT auto-confirm — operator releases with partial count at lineside")
	}
	// Default drop (no EvacuateOnChangeover marker): terminal from plan
	// time so cutover doesn't wait. Bin retrieval still runs but isn't on
	// the critical path.
	if action.NextState != "line_cleared" {
		t.Errorf("NextState = %q, want line_cleared (terminal from plan for non-evac drop)", action.NextState)
	}
}

// Drops with EvacuateOnChangeover=true on the from-claim represent
// tool-change-style evacuations — cutover should wait until the bin
// physically leaves the line. Task stays at empty_requested at plan
// time and advances to line_cleared on the pickup event (handled in
// handler_bin_picked_up).
func TestPlanNodeAction_Drop_WithEvacuateMarker(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.EvacuateOnChangeover = true
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationDrop,
		FromClaim:    &from,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.NextState != "empty_requested" {
		t.Errorf("NextState = %q, want empty_requested (evac-marked drop blocks cutover until pickup)", action.NextState)
	}
}

// Drop without OutboundDestination must fail loudly so the apply
// layer refuses the plan. The previous gate keyed on OutboundStaging
// and silently skipped — root cause of Bug 2 (ALN_002) where operators
// saw nothing happen and had no diagnostic to point at.
func TestPlanNodeAction_DropWithoutOutbound_FailsLoudly(t *testing.T) {
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"} // no OutboundDestination
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationDrop,
		FromClaim:    &from,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err == nil {
		t.Fatal("expected NodeAction.Err for Drop with no outbound destination, got nil")
	}
	if msg := action.Err.Error(); !strings.Contains(msg, "no outbound destination configured") {
		t.Errorf("err message = %q, want substring %q", msg, "no outbound destination configured")
	}
	if action.OrderA != nil || action.OrderB != nil {
		t.Error("Drop with config error must dispatch no orders")
	}
}

// Pin the apply-side contract — any NodeAction.Err in a multi-node
// plan is enough to refuse dispatch. The planner emits all actions; the
// apply layer's job is to fail the whole plan on any Err.
func TestBuildChangeoverPlan_AnyNodeWithErrFailsThePlan(t *testing.T) {
	// Valid Swap on N1.
	swapFrom := fullSwapClaim("N1", "PART-A", "consume")
	swapTo := fullSwapClaim("N1", "PART-B", "consume")
	// Misconfigured Drop on N2 (no OutboundDestination).
	dropFrom := processes.NodeClaim{CoreNodeName: "N2", PayloadCode: "PART-C", Role: "consume"}

	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &swapFrom, ToClaim: &swapTo},
		{CoreNodeName: "N2", Situation: SituationDrop, FromClaim: &dropFrom},
	}
	nodes := []processes.Node{
		{ID: 42, Name: "N1", CoreNodeName: "N1"},
		{ID: 43, Name: "N2", CoreNodeName: "N2"},
	}
	plan := BuildChangeoverPlan(diffs, nodes, false, nil)

	if len(plan.Actions) != 2 {
		t.Fatalf("expected 2 actions in plan, got %d", len(plan.Actions))
	}

	var sawErr bool
	for _, a := range plan.Actions {
		if a.Err != nil {
			sawErr = true
			if a.NodeName != "N2" {
				t.Errorf("expected Err on N2, found on %s", a.NodeName)
			}
		}
	}
	if !sawErr {
		t.Error("expected at least one NodeAction.Err in the plan")
	}
}


// Sequential SWAP emits a single complex order (StepsB nil at plan
// time) with a mid-sequence cutover wait. The planner reads ActivePull
// at plan time; with empty snapshot the tie-break convention applies
// (CoreNodeName=inactive, paired=active). LogTag = "swap_sequential"
// so logs distinguish from regular swap.

// Sequential is direct-trip and does not need InboundStaging /
// OutboundStaging populated. The planner must NOT divert sequential
// claims with empty staging fields into the staging-fallback path —
// doing so would silently emit a single-delivery retrieve instead of
// the A/B paired swap the operator expects.
func TestPlanNodeAction_Sequential_EmptyStagingStillDispatchesSequential(t *testing.T) {
	from := processes.NodeClaim{
		CoreNodeName:        "N1",
		PayloadCode:         "PART-A",
		Role:                "consume",
		SwapMode:            "sequential",
		PairedCoreNode:      "N1B",
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
		// InboundStaging and OutboundStaging deliberately empty.
	}
	to := processes.NodeClaim{
		CoreNodeName:  "N1",
		PayloadCode:   "PART-B",
		Role:          "consume",
		SwapMode:      "sequential",
		InboundSource: "MARKET",
	}
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("expected sequential dispatch with empty staging, got Err = %v", action.Err)
	}
	if action.LogTag != "swap_sequential" {
		t.Errorf("LogTag = %q, want swap_sequential (must NOT route through fallback_staging or fallback_retrieve)", action.LogTag)
	}
	if action.OrderA == nil || action.OrderA.Complex == nil {
		t.Fatal("expected sequential complex order; got no OrderA")
	}
}

func TestPlanNodeAction_Swap_Sequential(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "sequential"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-B", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.OrderA == nil {
		t.Fatal("expected single-order OrderA at plan time")
	}
	if action.OrderB != nil {
		t.Error("OrderB must be nil — sequential SWAP is a single-order shape")
	}
	if action.LogTag != "swap_sequential" {
		t.Errorf("LogTag = %q, want swap_sequential", action.LogTag)
	}
	// Tie-break: empty active-pull snapshot → CoreNodeName=inactive.
	// First pickup in OrderA's steps should target N1 (inactive); the
	// cutover wait should target N1B (active).
	steps := action.OrderA.Complex.Steps
	if len(steps) < 5 {
		t.Fatalf("expected at least 5 steps, got %d", len(steps))
	}
	if steps[0].Action != "pickup" || steps[0].Node != "N1" {
		t.Errorf("step 0: expected pickup at inactive N1, got %+v", steps[0])
	}
	if steps[4].Action != "wait" || steps[4].Node != "N1B" {
		t.Errorf("step 4: expected cutover wait at active N1B, got %+v", steps[4])
	}
}

// Active-pull awareness: when the runtime says the line is currently
// pulling from PairedCoreNode (N1B active), the planner should swap N1
// (the inactive side) FIRST. Mirror test asserts the inverse direction.
func TestBuildChangeoverPlan_Sequential_ActiveOnB_SwapsAFirst(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "sequential"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-B", "consume")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	nodes := []processes.Node{{ID: 42, Name: "N1", CoreNodeName: "N1"}}
	activePull := map[string]bool{"N1": false, "N1B": true}

	plan := BuildChangeoverPlan(diffs, nodes, false, activePull)
	if len(plan.Actions) != 1 || plan.Actions[0].Err != nil {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	steps := plan.Actions[0].OrderA.Complex.Steps
	if steps[0].Node != "N1" {
		t.Errorf("active=N1B → swap N1 (inactive) first; got first pickup at %q", steps[0].Node)
	}
	if steps[4].Node != "N1B" {
		t.Errorf("cutover wait should be at active N1B, got %q", steps[4].Node)
	}
}

func TestBuildChangeoverPlan_Sequential_ActiveOnA_SwapsBFirst(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "sequential"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-B", "consume")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	nodes := []processes.Node{{ID: 42, Name: "N1", CoreNodeName: "N1"}}
	activePull := map[string]bool{"N1": true, "N1B": false}

	plan := BuildChangeoverPlan(diffs, nodes, false, activePull)
	if len(plan.Actions) != 1 || plan.Actions[0].Err != nil {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	steps := plan.Actions[0].OrderA.Complex.Steps
	if steps[0].Node != "N1B" {
		t.Errorf("active=N1 → swap N1B (inactive) first; got first pickup at %q", steps[0].Node)
	}
	if steps[4].Node != "N1" {
		t.Errorf("cutover wait should be at active N1, got %q", steps[4].Node)
	}
}

// Sequential Evacuate emits both OrderA and OrderB at plan time
// (each robot evacs its position then fetches new + waits + delivers).
// LogTag = "evacuate_sequential".
func TestPlanNodeAction_Evacuate_Sequential(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "sequential"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-A", "consume")
	to.EvacuateOnChangeover = true
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationEvacuate,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.OrderA == nil || action.OrderB == nil {
		t.Fatal("sequential evacuate emits both OrderA and OrderB at plan time")
	}
	if action.LogTag != "evacuate_sequential" {
		t.Errorf("LogTag = %q, want evacuate_sequential", action.LogTag)
	}
	// Each robot's order has 5 steps with a bare wait at index 3.
	if len(action.OrderA.Complex.Steps) != 5 {
		t.Errorf("OrderA: expected 5 steps (backfill shape), got %d", len(action.OrderA.Complex.Steps))
	}
	if len(action.OrderB.Complex.Steps) != 5 {
		t.Errorf("OrderB: expected 5 steps (backfill shape), got %d", len(action.OrderB.Complex.Steps))
	}
}

// Sequential without PairedCoreNode → loud NodeAction.Err.
func TestPlanNodeAction_Sequential_RequiresPairedCoreNode(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "sequential"
	// PairedCoreNode intentionally empty
	to := fullSwapClaim("N1", "PART-B", "consume")

	for _, situation := range []ChangeoverSituation{SituationSwap, SituationEvacuate} {
		diff := ChangeoverNodeDiff{
			CoreNodeName: "N1",
			Situation:    situation,
			FromClaim:    &from,
			ToClaim:      &to,
		}
		node := &processes.Node{ID: 42, Name: "N1"}
		action := planNodeAction(diff, node, false, nil)
		if action.Err == nil {
			t.Errorf("situation=%s: expected NodeAction.Err for unpaired sequential, got none", situation)
		}
		// Per-mode validation reports the user-facing field name.
		if action.Err != nil && !strings.Contains(action.Err.Error(), "Paired Core Node") {
			t.Errorf("situation=%s: err message = %q, want substring %q", situation, action.Err.Error(), "Paired Core Node")
		}
		if action.OrderA != nil || action.OrderB != nil {
			t.Errorf("situation=%s: misconfigured plan must dispatch no orders", situation)
		}
	}
}

func TestPlanNodeAction_Add_FallsBackToStaging(t *testing.T) {
	to := fullSwapClaim("N1", "PART-B", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationAdd,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, true, nil)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.OrderA == nil {
		t.Fatal("Add should produce a fallback OrderA")
	}
	if action.NextState != "staging_requested" {
		t.Errorf("NextState = %q, want staging_requested", action.NextState)
	}
}

func TestPlanNodeAction_AddNoStaging_RetrieveFallback(t *testing.T) {
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-B", Role: "produce"} // no InboundStaging
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationAdd,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, true, nil)

	if action.OrderA == nil || action.OrderA.Retrieve == nil {
		t.Fatal("expected retrieve fallback")
	}
	if !action.OrderA.Retrieve.RetrieveEmpty {
		t.Error("produce role should retrieve empty bins")
	}
	if !action.OrderA.Retrieve.AutoConfirm {
		t.Error("retrieve fallback should honour fallbackAutoConfirm=true")
	}
}

func TestPlanNodeAction_KeepStagedSplit(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.KeepStaged = true
	from.SwapMode = "two_robot"
	to := fullSwapClaim("N1", "PART-B", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.LogTag != "keep_staged_split" {
		t.Errorf("LogTag = %q, want keep_staged_split", action.LogTag)
	}
	if action.OrderA == nil || action.OrderB == nil {
		t.Fatal("keep-staged split needs both orders")
	}
}

func TestPlanNodeAction_KeepStagedCombined(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.KeepStaged = true
	from.SwapMode = "single_robot"
	to := fullSwapClaim("N1", "PART-B", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.LogTag != "keep_staged_combined" {
		t.Errorf("LogTag = %q, want keep_staged_combined", action.LogTag)
	}
}

func TestBuildChangeoverPlan_SkipsUnchangedAndUnknownNodes(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	to := fullSwapClaim("N1", "PART-B", "consume")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
		{CoreNodeName: "N2", Situation: SituationUnchanged}, // skipped
		{CoreNodeName: "Nmissing", Situation: SituationSwap, FromClaim: &from, ToClaim: &to},
	}
	nodes := []processes.Node{{ID: 42, Name: "N1", CoreNodeName: "N1"}}
	plan := BuildChangeoverPlan(diffs, nodes, false, nil)

	if len(plan.Actions) != 1 {
		t.Fatalf("plan actions = %d, want 1 (only N1 should produce an action)", len(plan.Actions))
	}
	if plan.Actions[0].NodeName != "N1" {
		t.Errorf("action node = %q, want N1", plan.Actions[0].NodeName)
	}
}

// Same-bin-type press-index Swap routes through the standard press-
// index dispatch with both R1 and R2 orders emitted (no fan-out, no
// NodeAction.Err).
func TestPlanNodeAction_PressIndex_SameBinType_RoutesToStandardDispatch(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "two_robot_press_index"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-B", "consume")
	to.SwapMode = "two_robot_press_index"
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("expected no error in default inert mode, got %v", action.Err)
	}
	if action.OrderA == nil || action.OrderB == nil {
		t.Fatal("expected R1+R2 orders for same-bin-type press-index Swap")
	}
}

// The press-index different-bin-type case is handled by the per-
// position fan-out post-processor in changeover.go before the planner
// sees the diff list. Tests for that fan-out live in
// changeover_diff_test.go; the synthesized per-position diffs that the
// fan-out produces route through the planner via the
// TestPlanNodeAction_PressPosition tests below.

// A synthesized per-position diff (SwapMode = pressPositionSwapMode)
// routes through BuildSwapChangeoverSteps' per-position case: single-
// order shape, 4 steps, no operator gate.
func TestPlanNodeAction_PressPosition_SwapRoutesToPerPositionBuilder(t *testing.T) {
	from := processes.NodeClaim{
		CoreNodeName:        "POS-A",
		PayloadCode:         "PART-A",
		Role:                "consume",
		SwapMode:            pressPositionSwapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	to := processes.NodeClaim{
		CoreNodeName:        "POS-A",
		PayloadCode:         "PART-B",
		Role:                "consume",
		SwapMode:            pressPositionSwapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	diff := ChangeoverNodeDiff{
		CoreNodeName: "POS-A",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "POS-A"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected NodeAction.Err: %v", action.Err)
	}
	if action.OrderA == nil || action.OrderA.Complex == nil {
		t.Fatal("expected OrderA with the per-position 4-step list")
	}
	if action.OrderB != nil {
		t.Error("OrderB must be nil — per-position is single-order shape")
	}
	steps := action.OrderA.Complex.Steps
	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(steps))
	}
}

// Per-position Evacuate routes the same way (parent SituationEvacuate
// is unusual when bin types differ, but the dispatch must handle it).
func TestPlanNodeAction_PressPosition_EvacuateRoutesToPerPositionBuilder(t *testing.T) {
	from := processes.NodeClaim{
		CoreNodeName:        "POS-A",
		PayloadCode:         "PART-A",
		Role:                "consume",
		SwapMode:            pressPositionSwapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	to := processes.NodeClaim{
		CoreNodeName:        "POS-A",
		PayloadCode:         "PART-A",
		Role:                "consume",
		SwapMode:            pressPositionSwapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	diff := ChangeoverNodeDiff{
		CoreNodeName: "POS-A",
		Situation:    SituationEvacuate,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "POS-A"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err != nil {
		t.Fatalf("unexpected NodeAction.Err: %v", action.Err)
	}
	if action.OrderA == nil || action.OrderA.Complex == nil {
		t.Fatal("expected OrderA with the per-position 4-step list")
	}
	if len(action.OrderA.Complex.Steps) != 4 {
		t.Errorf("expected 4 steps, got %d", len(action.OrderA.Complex.Steps))
	}
}

// Per-mode changeover field validation. Each mode has a registry
// entry naming the fields its builder consumes; the planner emits a
// clear NodeAction.Err naming the missing field(s).
func TestRequiredChangeoverFields_PerMode(t *testing.T) {
	cases := []struct {
		name        string
		fromMode protocol.SwapMode
		from        processes.NodeClaim
		to          processes.NodeClaim
		wantSubstr  []string // substrings that must appear in formatMissingFields
		wantOK      bool     // when true, missing list should be empty
	}{
		{
			name:     "single_robot_complete",
			fromMode: "single_robot",
			from:     processes.NodeClaim{SwapMode: "single_robot", OutboundStaging: "OS", OutboundDestination: "OD"},
			to:       processes.NodeClaim{InboundStaging: "IS"},
			wantOK:   true,
		},
		{
			name:       "single_robot_missing_staging",
			fromMode:   "single_robot",
			from:       processes.NodeClaim{SwapMode: "single_robot", OutboundStaging: "OS", OutboundDestination: "OD"},
			to:         processes.NodeClaim{}, // no InboundStaging
			wantSubstr: []string{"to-claim", "Inbound Staging"},
		},
		{
			name:       "two_robot_missing_destination",
			fromMode:   "two_robot",
			from:       processes.NodeClaim{SwapMode: "two_robot"}, // no OutboundDestination
			to:         processes.NodeClaim{InboundStaging: "IS"},
			wantSubstr: []string{"from-claim", "Outbound Destination"},
		},
		{
			name:     "two_robot_complete",
			fromMode: "two_robot",
			from:     processes.NodeClaim{SwapMode: "two_robot", OutboundDestination: "OD"},
			to:       processes.NodeClaim{InboundStaging: "IS"},
			wantOK:   true,
		},
		{
			name:       "press_index_missing_paired",
			fromMode:   "two_robot_press_index",
			from:       processes.NodeClaim{SwapMode: "two_robot_press_index", OutboundDestination: "OD"},
			to:         processes.NodeClaim{InboundSource: "MARKET"},
			wantSubstr: []string{"Paired Core Node"},
		},
		{
			name:     "press_index_complete",
			fromMode: "two_robot_press_index",
			from:     processes.NodeClaim{SwapMode: "two_robot_press_index", PairedCoreNode: "B", OutboundDestination: "OD"},
			to:       processes.NodeClaim{InboundSource: "MARKET"},
			wantOK:   true,
		},
		{
			name:       "sequential_missing_inbound_source",
			fromMode:   "sequential",
			from:       processes.NodeClaim{SwapMode: "sequential", PairedCoreNode: "B", OutboundDestination: "OD"},
			to:         processes.NodeClaim{}, // no InboundSource
			wantSubstr: []string{"to-claim", "Inbound Source"},
		},
		{
			name:     "sequential_complete",
			fromMode: "sequential",
			from:     processes.NodeClaim{SwapMode: "sequential", PairedCoreNode: "B", OutboundDestination: "OD"},
			to:       processes.NodeClaim{InboundSource: "MARKET"},
			wantOK:   true,
		},
		{
			name:       "sequential_missing_paired",
			fromMode:   "sequential",
			from:       processes.NodeClaim{SwapMode: "sequential", OutboundDestination: "OD"},
			to:         processes.NodeClaim{InboundSource: "MARKET"},
			wantSubstr: []string{"Paired Core Node"},
		},
		{
			// Synthesized per-position claim from the press-index different-
			// bin-type fan-out. Missing OutboundDestination should surface
			// the same diagnostic shape as other modes.
			name:       "press_position_missing_outbound_destination",
			fromMode:   pressPositionSwapMode,
			from:       processes.NodeClaim{SwapMode: pressPositionSwapMode},
			to:         processes.NodeClaim{InboundSource: "MARKET"},
			wantSubstr: []string{"from-claim", "Outbound Destination"},
		},
		{
			name:     "press_position_complete_passes",
			fromMode: pressPositionSwapMode,
			from:     processes.NodeClaim{SwapMode: pressPositionSwapMode, OutboundDestination: "DEST"},
			to:       processes.NodeClaim{InboundSource: "MARKET"},
			wantOK:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing := requiredChangeoverFields(&tc.from, &tc.to)
			msg := formatMissingFields(missing)
			if tc.wantOK {
				if len(missing) != 0 {
					t.Errorf("expected no missing fields, got %v (%q)", missing, msg)
				}
				return
			}
			if len(missing) == 0 {
				t.Fatalf("expected missing fields, got none")
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(msg, want) {
					t.Errorf("missing-fields message %q does not contain %q", msg, want)
				}
			}
		})
	}
}

// Per-mode field validation emits an operator-readable diagnostic
// naming the missing field, distinct from the generic "couldn't build
// steps" message that the builder emits for unanticipated rejections.
func TestPlanNodeAction_MissingFieldDiagnostic(t *testing.T) {
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "two_robot"
	from.OutboundDestination = "" // missing
	to := fullSwapClaim("N1", "PART-B", "consume")
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationSwap,
		FromClaim:    &from,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false, nil)

	if action.Err == nil {
		t.Fatal("expected NodeAction.Err for missing OutboundDestination")
	}
	msg := action.Err.Error()
	if !strings.Contains(msg, "two_robot changeover requires") {
		t.Errorf("err must name the mode and changeover context: %q", msg)
	}
	if !strings.Contains(msg, "Outbound Destination") {
		t.Errorf("err must name the missing field: %q", msg)
	}
}
