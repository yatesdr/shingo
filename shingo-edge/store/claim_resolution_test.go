package store

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// Two hand-rolled resolvers held opposite precedence with no shared name, so
// the disagreement was invisible: each read like the general answer to "which
// claim governs this node". These pin both answers under the one name, so a
// future edit cannot quietly flip either.
func TestResolveNodeClaim_Precedence(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	processID, err := db.CreateProcess("PREC", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PREC-NODE", Code: "P1",
		Name: "Prec Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}

	activeStyle, err := db.CreateStyle("PREC-ACTIVE", "outgoing", processID)
	if err != nil {
		t.Fatalf("create active style: %v", err)
	}
	targetStyle, err := db.CreateStyle("PREC-TARGET", "incoming", processID)
	if err != nil {
		t.Fatalf("create target style: %v", err)
	}
	mkClaim := func(styleID int64, payload string) {
		if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: "PREC-NODE", Role: "consume",
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: payload, UOPCapacity: 100,
		}); err != nil {
			t.Fatalf("upsert claim %s: %v", payload, err)
		}
	}
	mkClaim(activeStyle, "PART-RESIDENT")
	mkClaim(targetStyle, "PART-REQUESTED")

	// Mid-changeover: both styles claim the node.
	proc := &processes.Process{ID: processID, ActiveStyleID: &activeStyle, TargetStyleID: &targetStyle}

	if got := db.ResolveNodeClaim(proc, node, ActiveStyleFirst); got == nil || got.PayloadCode != "PART-RESIDENT" {
		t.Errorf("ActiveStyleFirst = %v, want the ACTIVE style's claim. This is the question asked "+
			"about material already standing on the node.", got)
	}
	if got := db.ResolveNodeClaim(proc, node, TargetStyleFirst); got == nil || got.PayloadCode != "PART-REQUESTED" {
		t.Errorf("TargetStyleFirst = %v, want the TARGET style's claim. This is the question asked "+
			"about an order that does not exist yet.", got)
	}
}

// The fallback must not preempt: when the active style claims the node,
// ActiveStyleFirst answers with it even though a target style exists. This is the
// rule the engine resolver's own test has always stated; it now lives with the
// implementation.
func TestResolveNodeClaim_FallbackOnlyWhenTheFirstChoiceIsSilent(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	processID, err := db.CreateProcess("PREC2", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PREC2-NODE", Code: "P2",
		Name: "Prec Node 2", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	node, _ := db.GetProcessNode(nodeID)

	activeStyle, _ := db.CreateStyle("PREC2-ACTIVE", "outgoing", processID)
	targetStyle, _ := db.CreateStyle("PREC2-TARGET", "incoming", processID)
	// ONLY the target style claims this node — the freshly-added-node case.
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: targetStyle, CoreNodeName: "PREC2-NODE", Role: "consume",
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-ONLY-TARGET", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("upsert target claim: %v", err)
	}
	proc := &processes.Process{ID: processID, ActiveStyleID: &activeStyle, TargetStyleID: &targetStyle}

	if got := db.ResolveNodeClaim(proc, node, ActiveStyleFirst); got == nil || got.PayloadCode != "PART-ONLY-TARGET" {
		t.Errorf("ActiveStyleFirst = %v, want the target claim as a FALLBACK — the active style does "+
			"not claim this node, so there is nothing to preempt.", got)
	}
}

// nil for every reason a lookup can fail. Callers read nil as "no opinion".
func TestResolveNodeClaim_NilInputs(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	if got := db.ResolveNodeClaim(nil, nil, ActiveStyleFirst); got != nil {
		t.Errorf("nil process/node = %v, want nil", got)
	}
	if got := db.ResolveNodeClaim(&processes.Process{}, &processes.Node{}, TargetStyleFirst); got != nil {
		t.Errorf("process with no styles = %v, want nil", got)
	}
}
