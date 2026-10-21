package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestLoadablePayloads_PrefersAggregateOverStaleClaim pins that the load/request
// gate reads the loader's Core-owned payload set at the node — the SAME source
// the operator board shows — and not a claim's own narrower list.
//
// Reproduces Springfield 2026-06-17: SMN_001's per-style claims held narrow
// allowed lists that omitted a payload since added to the loader on the Core
// board. The board offered it; the gate (reading the stale claim union) rejected
// it with "payload ... not in allowed list for node". With the loader as the
// source of truth, the gate accepts every payload the board offers, whatever
// claim it is handed, and the claim the engine resolves at the node is the
// loader's own.
func TestLoadablePayloads_PrefersAggregateOverStaleClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// The loader carries the broader set: PART-NEW was added on the Core board.
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "LDR", LoaderKey: "loader:LDR", Role: "produce",
		Layout: "shared_window", Replenishment: "operator",
		InboundSource: "EMPTY-SUPER", OutboundDest: "FG-MARKET",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "LDR"}},
		Payloads: []protocol.LoaderPayloadInfo{
			{PayloadCode: "PART-OLD"}, {PayloadCode: "PART-NEW"},
		},
	})
	_, nodeID, _ := seedLoaderNode(t, db, "PROC", "LDR")

	node, _, claim, err := eng.loadActiveNode(nodeID)
	if err != nil {
		t.Fatalf("loadActiveNode: %v", err)
	}
	if claim == nil || claim.ID != 0 || !slices.Equal(claim.AllowedPayloads(), []string{"PART-NEW", "PART-OLD"}) {
		t.Fatalf("claim at LDR = %+v, want the loader's synthesized claim allowing [PART-NEW PART-OLD]", claim)
	}

	// A claim whose own list omits PART-NEW, as the stale per-style claim did.
	stale := &processes.NodeClaim{
		CoreNodeName: "LDR", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeManualSwap,
		PayloadCode: "PART-OLD", AllowedPayloadCodes: []string{"PART-OLD"},
	}
	for name, c := range map[string]*processes.NodeClaim{"resolved": claim, "stale": stale} {
		got := eng.loadablePayloads(node, c)
		if !slices.Contains(got, "PART-NEW") || !slices.Contains(got, "PART-OLD") {
			t.Errorf("loadablePayloads with the %s claim = %v; want the loader's set, PART-NEW and PART-OLD "+
				"(the board offers both, so the gate must accept both)", name, got)
		}
	}
}

// TestLoadablePayloads_FallsBackToClaimWhenNotInAggregate pins the other arm:
// at a node no Core loader names, the gate reads the claim's own list.
func TestLoadablePayloads_FallsBackToClaimWhenNotInAggregate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	_, nodeID, _ := seedLoaderNode(t, db, "PROC", "LDR")
	// No seedCoreLoader — no loader names LDR.
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	claim := &processes.NodeClaim{
		CoreNodeName: "LDR", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeManualSwap,
		PayloadCode: "PART-OLD", AllowedPayloadCodes: []string{"PART-OLD"},
	}
	if got := eng.loadablePayloads(node, claim); !slices.Equal(got, []string{"PART-OLD"}) {
		t.Errorf("loadablePayloads = %v; want the claim's own list [PART-OLD]", got)
	}
}
