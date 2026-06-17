package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
)

// TestLoadablePayloads_PrefersAggregateOverStaleClaim pins the post-cutover
// invariant that the load/request gate reads the loader's Core-owned payload set
// (the aggregate) — the SAME source the operator board shows — and NOT the
// per-style edge style_node_claims the cutover left behind.
//
// Reproduces Springfield 2026-06-17: SMN_001's per-style claims held narrow
// allowed lists that omitted a payload since added to the loader on the Core
// board. The board offered it; the gate (reading the stale claim union) rejected
// it with "payload ... not in allowed list for node". With the aggregate as the
// source of truth, the gate accepts every payload the board offers.
func TestLoadablePayloads_PrefersAggregateOverStaleClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// Stale per-style edge claim: this loader node's allowed list is just PART-OLD
	// (what the per-style picker held before it was retired).
	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "PROC", "LDR", "PART-OLD")

	// Core-owned aggregate: the loader now carries a broader set — PART-NEW was
	// added on the board after the per-style edge config was frozen.
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "LDR", LoaderKey: "loader:LDR", Role: "produce",
		Layout: "shared_window", Replenishment: "operator",
		InboundSource: "EMPTY-SUPER", OutboundDest: "FG-MARKET", ConfigGen: 2,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "LDR", Kind: "window"}},
		Payloads: []protocol.LoaderPayloadInfo{
			{PayloadCode: "PART-OLD"}, {PayloadCode: "PART-NEW"},
		},
	})

	node, _, claim, err := eng.loadActiveNode(nodeID)
	if err != nil {
		t.Fatalf("loadActiveNode: %v", err)
	}
	// Precondition: the resolved claim is the real, stale one — its own list omits
	// PART-NEW, so a claim-derived gate would (wrongly) reject it.
	if got := claim.AllowedPayloads(); !slices.Equal(got, []string{"PART-OLD"}) {
		t.Fatalf("precondition: claim.AllowedPayloads() = %v, want the stale [PART-OLD]", got)
	}

	got := eng.loadablePayloads(node, claim)
	if !slices.Contains(got, "PART-NEW") {
		t.Errorf("loadablePayloads = %v; want PART-NEW from the Core aggregate "+
			"(the board offers it, so the gate must accept it)", got)
	}
	if !slices.Contains(got, "PART-OLD") {
		t.Errorf("loadablePayloads = %v; want it to still include PART-OLD", got)
	}
}

// TestLoadablePayloads_FallsBackToClaimWhenNotInAggregate pins the preserved
// pre-cutover behaviour: a manual_swap node absent from the Core aggregate
// (legacy / not yet migrated) falls back to the per-style edge claim union, so an
// unmigrated loader still resolves payloads rather than stranding the operator.
func TestLoadablePayloads_FallsBackToClaimWhenNotInAggregate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	_, nodeID, _ := seedActiveManualSwapLoader(t, db, "PROC", "LDR", "PART-OLD")
	// No seedCoreLoader — the loader is absent from the aggregate.

	node, _, claim, err := eng.loadActiveNode(nodeID)
	if err != nil {
		t.Fatalf("loadActiveNode: %v", err)
	}
	got := eng.loadablePayloads(node, claim)
	if !slices.Equal(got, []string{"PART-OLD"}) {
		t.Errorf("loadablePayloads = %v; want fallback to the claim union [PART-OLD]", got)
	}
}
