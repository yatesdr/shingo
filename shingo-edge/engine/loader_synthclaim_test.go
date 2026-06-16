package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestCoreLoaderNode_NoEdgeClaim_ResolvesAsManualSwap pins the Core-owned loader
// refactor: a node that is a window of a Core loader but has NO per-style
// style_node_claim still resolves as a manual_swap loader node — loadActiveNode
// synthesizes the claim from the aggregate and CanAcceptOrders gives it the
// multi-order queue. So an operator never has to author a per-style claim to run
// a loader; the Core loader config + operator-station membership are enough.
func TestCoreLoaderNode_NoEdgeClaim_ResolvesAsManualSwap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// A process node for the unloader window — deliberately with NO style_node_claim.
	procID, err := db.CreateProcess("UNLOADER-PROC", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "SMN-U1", Code: "SMN-U1", Name: "SMN-U1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	// Core owns the unloader (consume, shared_window) with SMN-U1 as a window.
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "Supermarket Unloader", LoaderKey: "loader:UNLD", Role: "consume", Layout: "shared_window",
		Replenishment: "operator", OutboundDest: "Empty Totes", InboundSource: "FG Area", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "SMN-U1", Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}, {PayloadCode: "PART-B"}},
	})

	// loadActiveNode synthesizes a manual_swap consume claim despite no edge claim.
	_, _, claim, err := eng.loadActiveNode(nodeID)
	if err != nil {
		t.Fatalf("loadActiveNode: %v", err)
	}
	if claim == nil {
		t.Fatal("no claim synthesized for a Core-loader node with no edge claim")
	}
	if claim.ID != 0 {
		t.Errorf("synth claim ID = %d, want 0 (non-persisted)", claim.ID)
	}
	if claim.SwapMode != protocol.SwapModeManualSwap {
		t.Errorf("SwapMode = %q, want manual_swap", claim.SwapMode)
	}
	if claim.Role != protocol.ClaimRoleConsume {
		t.Errorf("Role = %q, want consume", claim.Role)
	}
	if !claim.AutoConfirm {
		t.Error("AutoConfirm = false, want true")
	}

	// And the node takes the multi-order queue, not the serial single-order constraint.
	if ok, reason := eng.CanAcceptOrders(nodeID); !ok {
		t.Errorf("CanAcceptOrders = false (%s), want true for a Core-loader node", reason)
	}

	// A non-loader node still synthesizes nothing (clean miss → plain node).
	other, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "PLAIN-1", Code: "PLAIN-1", Name: "PLAIN-1", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create plain node: %v", err)
	}
	if _, _, c, _ := eng.loadActiveNode(other); c != nil {
		t.Errorf("loadActiveNode for a non-loader node = %+v, want nil claim", c)
	}
}
