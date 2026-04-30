package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// fullSwapClaim returns a NodeClaim with all the fields the planner needs to
// take the standard swap/evacuate path (i.e. not fall back).
func fullSwapClaim(node, payload string, role protocol.ClaimRole) processes.NodeClaim {
	return processes.NodeClaim{
		CoreNodeName:    node,
		PayloadCode:     payload,
		Role:            role,
		InboundSource:   "SRC_" + node,
		InboundStaging:  "ISTG_" + node,
		OutboundStaging: "OSTG_" + node,
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
	action := planNodeAction(diff, node, false)

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
	action := planNodeAction(diff, node, false)

	if action.Err != nil {
		t.Fatalf("unexpected planning error: %v", action.Err)
	}
	if action.OrderA != nil {
		t.Error("Drop should not produce OrderA")
	}
	if action.OrderB == nil || action.OrderB.Complex == nil {
		t.Fatal("Drop should produce OrderB (release)")
	}
	if !action.OrderB.Complex.AutoConfirm {
		t.Error("Drop OrderB must auto-confirm")
	}
	if action.NextState != "empty_requested" {
		t.Errorf("NextState = %q, want empty_requested", action.NextState)
	}
}

func TestPlanNodeAction_DropWithoutOutbound_Skips(t *testing.T) {
	from := processes.NodeClaim{CoreNodeName: "N1", PayloadCode: "PART-A", Role: "consume"} // no OutboundStaging
	diff := ChangeoverNodeDiff{
		CoreNodeName: "N1",
		Situation:    SituationDrop,
		FromClaim:    &from,
	}
	node := &processes.Node{ID: 42, Name: "N1"}
	action := planNodeAction(diff, node, false)

	if action.Err != nil {
		t.Fatalf("expected silent skip, got err: %v", action.Err)
	}
	if action.OrderA != nil || action.OrderB != nil {
		t.Error("Drop without OutboundStaging must produce no orders")
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
	action := planNodeAction(diff, node, true)

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
	action := planNodeAction(diff, node, true)

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
	action := planNodeAction(diff, node, false)

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
	action := planNodeAction(diff, node, false)

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
	plan := BuildChangeoverPlan(diffs, nodes, false)

	if len(plan.Actions) != 1 {
		t.Fatalf("plan actions = %d, want 1 (only N1 should produce an action)", len(plan.Actions))
	}
	if plan.Actions[0].NodeName != "N1" {
		t.Errorf("action node = %q, want N1", plan.Actions[0].NodeName)
	}
}
