package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
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
	t.Parallel()
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
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil {
		t.Fatal("expected supply order complex spec")
	}
	if action.SupplyOrder.Complex.DeliveryNode != "ISTG_N1" {
		t.Errorf("supply order delivery = %q, want ISTG_N1", action.SupplyOrder.Complex.DeliveryNode)
	}
	if action.SupplyOrder.Complex.AutoConfirm {
		t.Error("supply order must not auto-confirm (operator stages)")
	}
	if action.EvacOrder == nil || action.EvacOrder.Complex == nil {
		t.Fatal("expected evac order complex spec")
	}
	if !action.EvacOrder.Complex.AutoConfirm {
		t.Error("evac order must auto-confirm (robot completes)")
	}
	// Regression (plant 2026-06-01 ALN_001): the evac/removal leg must carry the
	// FROM-style payload (the outgoing bin), not the new/target style. Left empty
	// it gets backfilled to the target style by lookupPayloadMeta during
	// changeover, so the removal filters for the new payload and fails to claim
	// the old bin still at the line ("no bin present at the node").
	if action.EvacOrder.Complex.PayloadCode != "PART-A" {
		t.Errorf("evac payload = %q, want PART-A (from-style); a non-from payload reopens the ALN_001 removal failure", action.EvacOrder.Complex.PayloadCode)
	}
	if action.NextState != domain.NodeTaskStagingRequested {
		t.Errorf("NextState = %q, want staging_requested", action.NextState)
	}
	if action.LogTag != "swap" {
		t.Errorf("LogTag = %q, want swap", action.LogTag)
	}
}

func TestPlanNodeAction_Drop(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder != nil {
		t.Error("Drop should not produce supply order")
	}
	if action.EvacOrder == nil || action.EvacOrder.Complex == nil {
		t.Fatal("Drop should produce evac order (release)")
	}
	// AutoConfirm=false: the drop order uses the staged-release pattern
	// (wait-at-line → operator releases with partial count → pickup →
	// dropoff). The operator's release click at the lineside is the gate.
	if action.EvacOrder.Complex.AutoConfirm {
		t.Error("Drop evac order must NOT auto-confirm — operator releases with partial count at lineside")
	}
	// Default drop (no EvacuateOnChangeover marker): terminal from plan
	// time so cutover doesn't wait. Bin retrieval still runs but isn't on
	// the critical path.
	if action.NextState != domain.NodeTaskLineCleared {
		t.Errorf("NextState = %q, want line_cleared (terminal from plan for non-evac drop)", action.NextState)
	}
}

// Drops with EvacuateOnChangeover=true on the from-claim represent
// tool-change-style evacuations — cutover should wait until the bin
// physically leaves the line. Task stays at empty_requested at plan
// time and advances to line_cleared on the pickup event (handled in
// handler_bin_picked_up).
func TestPlanNodeAction_Drop_WithEvacuateMarker(t *testing.T) {
	t.Parallel()
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
	if action.NextState != domain.NodeTaskEmptyRequested {
		t.Errorf("NextState = %q, want empty_requested (evac-marked drop blocks cutover until pickup)", action.NextState)
	}
}

// Drop without OutboundDestination must fail loudly so the apply
// layer refuses the plan. The previous gate keyed on OutboundStaging
// and silently skipped — root cause of Bug 2 (ALN_002) where operators
// saw nothing happen and had no diagnostic to point at.
func TestPlanNodeAction_DropWithoutOutbound_FailsLoudly(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder != nil || action.EvacOrder != nil {
		t.Error("Drop with config error must dispatch no orders")
	}
}

// Pin the apply-side contract — any NodeAction.Err in a multi-node
// plan is enough to refuse dispatch. The planner emits all actions; the
// apply layer's job is to fail the whole plan on any Err.
func TestBuildChangeoverPlan_AnyNodeWithErrFailsThePlan(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil {
		t.Fatal("expected sequential complex order; got no supply order")
	}
}

func TestPlanNodeAction_Swap_Sequential(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder == nil {
		t.Fatal("expected single-order supply order at plan time")
	}
	if action.EvacOrder != nil {
		t.Error("evac order must be nil — sequential SWAP is a single-order shape")
	}
	if action.LogTag != "swap_sequential" {
		t.Errorf("LogTag = %q, want swap_sequential", action.LogTag)
	}
	// Tie-break: empty active-pull snapshot → CoreNodeName=inactive.
	// First pickup in supply order's steps should target N1 (inactive); the
	// cutover wait should target N1B (active).
	steps := action.SupplyOrder.Complex.Steps
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
	t.Parallel()
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
	steps := plan.Actions[0].SupplyOrder.Complex.Steps
	if steps[0].Node != "N1" {
		t.Errorf("active=N1B → swap N1 (inactive) first; got first pickup at %q", steps[0].Node)
	}
	if steps[4].Node != "N1B" {
		t.Errorf("cutover wait should be at active N1B, got %q", steps[4].Node)
	}
}

func TestBuildChangeoverPlan_Sequential_ActiveOnA_SwapsBFirst(t *testing.T) {
	t.Parallel()
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
	steps := plan.Actions[0].SupplyOrder.Complex.Steps
	if steps[0].Node != "N1B" {
		t.Errorf("active=N1 → swap N1B (inactive) first; got first pickup at %q", steps[0].Node)
	}
	if steps[4].Node != "N1" {
		t.Errorf("cutover wait should be at active N1, got %q", steps[4].Node)
	}
}

// Sequential Evacuate emits both supply order and evac order at plan time
// (each robot evacs its position then fetches new + waits + delivers).
// LogTag = "evacuate_sequential".
func TestPlanNodeAction_Evacuate_Sequential(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder == nil || action.EvacOrder == nil {
		t.Fatal("sequential evacuate emits both supply order and evac order at plan time")
	}
	if action.LogTag != "evacuate_sequential" {
		t.Errorf("LogTag = %q, want evacuate_sequential", action.LogTag)
	}
	// Each robot's order has 5 steps with a bare wait at index 3.
	if len(action.SupplyOrder.Complex.Steps) != 5 {
		t.Errorf("supply order: expected 5 steps (backfill shape), got %d", len(action.SupplyOrder.Complex.Steps))
	}
	if len(action.EvacOrder.Complex.Steps) != 5 {
		t.Errorf("evac order: expected 5 steps (backfill shape), got %d", len(action.EvacOrder.Complex.Steps))
	}
}

// Sequential without PairedCoreNode → loud NodeAction.Err.
func TestPlanNodeAction_Sequential_RequiresPairedCoreNode(t *testing.T) {
	t.Parallel()
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
		if action.SupplyOrder != nil || action.EvacOrder != nil {
			t.Errorf("situation=%s: misconfigured plan must dispatch no orders", situation)
		}
	}
}

func TestPlanNodeAction_Add_FallsBackToStaging(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder == nil {
		t.Fatal("Add should produce a fallback supply order")
	}
	if action.NextState != domain.NodeTaskStagingRequested {
		t.Errorf("NextState = %q, want staging_requested", action.NextState)
	}
}

func TestPlanNodeAction_AddNoStaging_RetrieveFallback(t *testing.T) {
	t.Parallel()
	to := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-B", Role: "produce"} // no InboundStaging
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationAdd,
		ToClaim:      &to,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, true, nil)

	if action.SupplyOrder == nil || action.SupplyOrder.Retrieve == nil {
		t.Fatal("expected retrieve fallback")
	}
	if !action.SupplyOrder.Retrieve.RetrieveEmpty {
		t.Error("produce role should retrieve empty bins")
	}
	if !action.SupplyOrder.Retrieve.AutoConfirm {
		t.Error("retrieve fallback should honour fallbackAutoConfirm=true")
	}
}

func TestPlanNodeAction_KeepStagedSplit(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder == nil || action.EvacOrder == nil {
		t.Fatal("keep-staged split needs both orders")
	}
	if action.EvacOrder.Complex.PayloadCode != "PART-A" {
		t.Errorf("keep-staged split evac payload = %q, want PART-A (from-style)", action.EvacOrder.Complex.PayloadCode)
	}
}

func TestPlanNodeAction_KeepStagedCombined(t *testing.T) {
	t.Parallel()
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
	if action.EvacOrder == nil || action.EvacOrder.Complex == nil {
		t.Fatal("keep-staged combined needs an evac order")
	}
	if action.EvacOrder.Complex.PayloadCode != "PART-A" {
		t.Errorf("keep-staged combined evac payload = %q, want PART-A (from-style)", action.EvacOrder.Complex.PayloadCode)
	}
}

func TestBuildChangeoverPlan_SkipsUnchangedAndUnknownNodes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	if action.SupplyOrder == nil || action.EvacOrder == nil {
		t.Fatal("expected R1+R2 orders for same-bin-type press-index Swap")
	}
}

// TestPlanNodeAction_PressIndex_BlankStaging_BuildsSwap is the flip of the
// commit-A characterization test of the same input: press-index is now in
// directTripChangeoverMode, so a blank-staging Swap no longer diverts to the
// retrieve fallback — it builds the R1/R2 choreography (LogTag "swap"). This is
// the change that un-stalls HK P400. Both legs carry the from-style payload so
// each can claim the OLD tote it picks up (R1 at the front, R2 at the back);
// without it the removal filters for the new payload and holds at sourcing
// (ALN_001). The refill pickup is NOT payload-stamped (consume here; produce is
// covered separately) — for consume it fetches a new full bin normally.
func TestPlanNodeAction_PressIndex_BlankStaging_BuildsSwap(t *testing.T) {
	t.Parallel()
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "two_robot_press_index"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-B", "consume")
	to.SwapMode = "two_robot_press_index"
	to.InboundStaging = "" // HK: no staging — but press-index is now direct-trip, so no fallback
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
	if action.LogTag != "swap" {
		t.Fatalf("LogTag = %q, want swap (press-index is direct-trip; must not divert to fallback)", action.LogTag)
	}
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil ||
		action.EvacOrder == nil || action.EvacOrder.Complex == nil {
		t.Fatal("expected both R1 (evac) and R2 (supply) complex orders")
	}
	// Both legs pick up an OLD (from-style) tote, so both must carry PART-A.
	if p := action.EvacOrder.Complex.PayloadCode; p != "PART-A" {
		t.Errorf("evac (R1) payload = %q, want PART-A (front tote is from-style)", p)
	}
	if p := action.SupplyOrder.Complex.PayloadCode; p != "PART-A" {
		t.Errorf("supply (R2) payload = %q, want PART-A (back tote is from-style; blank reopens ALN_001)", p)
	}
	// consume refill is a normal full retrieve — the InboundSource pickup on R1
	// is NOT flagged Empty (that flag is produce-only; see the produce test).
	if s := pickupAt(action.EvacOrder.Complex.Steps, to.InboundSource); s == nil || s.Empty {
		t.Errorf("consume R1 refill at %q: want present and Empty=false, got %+v", to.InboundSource, s)
	}
}

// TestPlanNodeAction_PressIndex_Produce_RefillIsEmpty pins markInboundEmpty on
// the changeover path (P400 is produce). R1's refill pickup at InboundSource
// must be Empty so it fetches a fresh empty carrier rather than hunting a full
// payload-matched bin in the empty pool ("no bin of requested payload"). The old
// front-tote pickup is NOT flagged — it must keep the from-style payload.
func TestPlanNodeAction_PressIndex_Produce_RefillIsEmpty(t *testing.T) {
	t.Parallel()
	from := fullSwapClaim("N1", "PART-A", "produce")
	from.SwapMode = "two_robot_press_index"
	from.PairedCoreNode = "N1B"
	to := fullSwapClaim("N1", "PART-B", "produce")
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
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	r1 := action.EvacOrder.Complex.Steps
	if s := pickupAt(r1, to.InboundSource); s == nil || !s.Empty {
		t.Errorf("produce R1 refill at %q: want Empty=true, got %+v", to.InboundSource, s)
	}
	if s := pickupAt(r1, "N1"); s == nil || s.Empty {
		t.Errorf("produce R1 front pickup at N1: want Empty=false (keeps from-payload), got %+v", s)
	}
}

// pickupAt returns the first pickup step at node, or nil.
func pickupAt(steps []protocol.ComplexOrderStep, node string) *protocol.ComplexOrderStep {
	for i := range steps {
		if steps[i].Action == "pickup" && steps[i].Node == node {
			return &steps[i]
		}
	}
	return nil
}

// TestPlanNodeAction_TwoRobot_SlotAssignment is the neutrality anchor for the
// role-declared-legs change (commit B). two_robot is the mode where the
// positional StepsA=supply / StepsB=evac mapping happens to equal the
// steps-based role, so commit B must leave this assignment BYTE-identical. If
// this test changes, role-naming perturbed the one mode it must not.
func TestPlanNodeAction_TwoRobot_SlotAssignment(t *testing.T) {
	t.Parallel()
	from := fullSwapClaim("N1", "PART-A", "consume")
	from.SwapMode = "two_robot"
	to := fullSwapClaim("N1", "PART-B", "consume")
	to.SwapMode = "two_robot"
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
	if action.LogTag != "swap" {
		t.Errorf("LogTag = %q, want swap", action.LogTag)
	}
	// Supply leg = the resupply robot: fetch new from source, stage, deliver to line.
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil {
		t.Fatal("expected supply complex order")
	}
	sup := action.SupplyOrder.Complex
	if last := sup.Steps[len(sup.Steps)-1]; last.Action != "dropoff" || last.Node != "N1" {
		t.Errorf("supply leg final step = %+v, want dropoff at N1 (delivers new bin to the line)", last)
	}
	if sup.DeliveryNode != "N1" {
		t.Errorf("supply DeliveryNode = %q, want N1 (the line node it delivers to)", sup.DeliveryNode)
	}
	if sup.PayloadCode != "" {
		t.Errorf("supply PayloadCode = %q, want empty (fresh bin, backfilled to target style)", sup.PayloadCode)
	}
	// Evac leg = the removal robot: wait at line, lift old bin, carry to outbound.
	if action.EvacOrder == nil || action.EvacOrder.Complex == nil {
		t.Fatal("expected evac complex order")
	}
	evac := action.EvacOrder.Complex
	if len(evac.Steps) < 2 || evac.Steps[1].Action != "pickup" || evac.Steps[1].Node != "N1" {
		t.Errorf("evac leg steps = %+v, want pickup at N1 as second step (removal leg)", evac.Steps)
	}
	if evac.PayloadCode != "PART-A" {
		t.Errorf("evac PayloadCode = %q, want PART-A (from-style; ALN_001 removal-claim fix)", evac.PayloadCode)
	}
}

// TestPlanNodeAction_PressIndex_WithStaging_SlotContent pins that press-index
// legs are assigned by ROLE, not by StepsA/StepsB position. R1 clears the press
// (steps-role evac) and R2 indexes the fresh bin on (steps-role supply) — the
// inverse of the positional order (swap_leg_role.go:28-36 table). Before commit
// B this test pinned the inversion (R1 in the supply slot, R2 in the evac slot,
// plus the hardcoded DeliveryNodeA=CoreNodeName lie); the assertions below are
// the flipped, corrected version, and that diff is the stall fix. With the evac
// slot now holding R1 — whose pickup(A) is at CoreNodeName — the release defer's
// location gate passes and R2 is released (no release-path change).
func TestPlanNodeAction_PressIndex_WithStaging_SlotContent(t *testing.T) {
	t.Parallel()
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
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil ||
		action.EvacOrder == nil || action.EvacOrder.Complex == nil {
		t.Fatal("expected both R1 and R2 complex orders")
	}
	// EVAC slot = R1, the removal leg: it waits at and picks up from the front
	// (CoreNodeName), which is what lets the release defer's location gate fire.
	if s := action.EvacOrder.Complex.Steps; len(s) < 2 || s[1].Action != "pickup" || s[1].Node != "N1" {
		t.Errorf("evac slot steps = %+v, want R1 (pickup at N1 as 2nd step)", s)
	}
	// SUPPLY slot = R2, the index leg: it waits at and picks up from the back
	// (PairedCoreNode) and places the tote on the front.
	if s := action.SupplyOrder.Complex.Steps; len(s) < 1 || s[0].Action != "wait" || s[0].Node != "N1B" {
		t.Errorf("supply slot steps = %+v, want R2 (wait at N1B first)", s)
	}
	// The evac leg (R1) no longer carries a hardcoded delivery node — Core
	// derives its true destination (the back position) from the steps. This is
	// the HK 2026-07-14 misbind fix: the old code stored the front node here.
	if dn := action.EvacOrder.Complex.DeliveryNode; dn != "" {
		t.Errorf("evac DeliveryNode = %q, want empty (Core-derived from steps); a stored front node is the 07-14 lie", dn)
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
	t.Parallel()
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
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil {
		t.Fatal("expected supply order with the per-position 4-step list")
	}
	if action.EvacOrder != nil {
		t.Error("evac order must be nil — per-position is single-order shape")
	}
	steps := action.SupplyOrder.Complex.Steps
	if len(steps) != 4 {
		t.Fatalf("expected 4 steps, got %d", len(steps))
	}
}

// Per-position Evacuate routes the same way (parent SituationEvacuate
// is unusual when bin types differ, but the dispatch must handle it).
func TestPlanNodeAction_PressPosition_EvacuateRoutesToPerPositionBuilder(t *testing.T) {
	t.Parallel()
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
	if action.SupplyOrder == nil || action.SupplyOrder.Complex == nil {
		t.Fatal("expected supply order with the per-position 4-step list")
	}
	if len(action.SupplyOrder.Complex.Steps) != 4 {
		t.Errorf("expected 4 steps, got %d", len(action.SupplyOrder.Complex.Steps))
	}
}

// Per-mode changeover field validation. Each mode has a registry
// entry naming the fields its builder consumes; the planner emits a
// clear NodeAction.Err naming the missing field(s).
func TestRequiredChangeoverFields_PerMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		fromMode   protocol.SwapMode
		from       processes.NodeClaim
		to         processes.NodeClaim
		wantSubstr []string // substrings that must appear in formatMissingFields
		wantOK     bool     // when true, missing list should be empty
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
	t.Parallel()
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
