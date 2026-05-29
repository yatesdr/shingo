package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// These characterization tests pin the behaviour of the manual_swap claim
// finders across the WalkClaims extraction (build step 1): the
// multi-process union, the core-node filter, the first-match iteration
// order, and the active-only-vs-all-styles distinction. They lock the
// contract so the refactor is provably behaviour-preserving.

// seedActiveManualSwapLoader creates a process with one active style and a
// produce manual_swap claim + matching process_node, all targeting coreNode
// for payload.
func seedActiveManualSwapLoader(t *testing.T, db *store.DB, procName, coreNode, payload string) (procID, nodeID, styleID int64) {
	t.Helper()
	var err error
	procID, err = db.CreateProcess(procName, "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process %s: %v", procName, err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    procID,
		CoreNodeName: coreNode,
		Code:         coreNode,
		Name:         coreNode,
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node %s: %v", coreNode, err)
	}
	styleID, err = db.CreateStyle(procName+"-STYLE", "", procID)
	if err != nil {
		t.Fatalf("create style for %s: %v", procName, err)
	}
	if err := db.SetActiveStyle(procID, &styleID); err != nil {
		t.Fatalf("set active style for %s: %v", procName, err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        coreNode,
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         payload,
		AllowedPayloadCodes: []string{payload},
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FG-MARKET",
		UOPCapacity:         100,
	}); err != nil {
		t.Fatalf("upsert claim for %s: %v", coreNode, err)
	}
	return procID, nodeID, styleID
}

// TestFindManualSwapNodes_TwoProcessesOneLoader pins the union: two
// processes whose active styles each claim the SAME shared loader core
// node both contribute a (node, claim) pair. This is the SNF2+SNF3
// "one loader, two cells" case the transitional plan centres on.
func TestFindManualSwapNodes_TwoProcessesOneLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedActiveManualSwapLoader(t, db, "SNF2", "SHARED-LOADER", "PART-A")
	seedActiveManualSwapLoader(t, db, "SNF3", "SHARED-LOADER", "PART-B")

	got := eng.findManualSwapNodes("")
	if len(got) != 2 {
		t.Fatalf("expected 2 (node,claim) pairs across both processes, got %d", len(got))
	}
	for _, m := range got {
		if m.node.CoreNodeName != "SHARED-LOADER" {
			t.Errorf("unexpected node %q", m.node.CoreNodeName)
		}
	}
	if got[0].node.ID == got[1].node.ID {
		t.Errorf("expected two distinct process_nodes, both were node %d", got[0].node.ID)
	}
}

// TestFindManualSwapNodes_CoreNodeNameFilter pins the coreNodeName filter:
// only pairs at the named core node are returned.
func TestFindManualSwapNodes_CoreNodeNameFilter(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedActiveManualSwapLoader(t, db, "P1", "LOADER-1", "PART-A")
	seedActiveManualSwapLoader(t, db, "P2", "LOADER-2", "PART-B")

	got := eng.findManualSwapNodes("LOADER-2")
	if len(got) != 1 {
		t.Fatalf("expected 1 pair for the filtered core node, got %d", len(got))
	}
	if got[0].node.CoreNodeName != "LOADER-2" {
		t.Errorf("filter leaked: got %q", got[0].node.CoreNodeName)
	}
}

// TestFindAnyLoaderClaimForPayload_DuplicatePayloadAcrossProcessesIsOrderDependent
// pins the first-match invariant: when the SAME payload is claimed at two
// different loaders, the resolver returns the first in walk order
// (processes by name). WalkClaims must preserve this ordering — routing
// depends on it, and the v2 plan's HandleLoopBelowThreshold fix (resolve by
// CoreNodeName) exists precisely because this first-match-by-name is the
// wrong key for multi-loader-same-payload topologies.
func TestFindAnyLoaderClaimForPayload_DuplicatePayloadAcrossProcessesIsOrderDependent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedActiveManualSwapLoader(t, db, "AAA-PROC", "LOADER-A", "SHARED-PART")
	seedActiveManualSwapLoader(t, db, "BBB-PROC", "LOADER-B", "SHARED-PART")

	got := eng.FindAnyLoaderClaimForPayload("SHARED-PART")
	if got == nil {
		t.Fatal("expected a loader, got nil")
	}
	if got.node.CoreNodeName != "LOADER-A" {
		t.Errorf("expected first-match LOADER-A (alphabetically-first process), got %q", got.node.CoreNodeName)
	}
}

// TestFindLoaderForPayload_ActiveOnlyVsFindAny pins the ActiveOnly axis:
// a produce claim on an INACTIVE style is invisible to FindLoaderForPayload
// (active-style only) but visible to FindAnyLoaderClaimForPayload
// (all styles).
func TestFindLoaderForPayload_ActiveOnlyVsFindAny(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("ACT-PROC", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "CAL-LOADER", Code: "CL", Name: "Cal Loader", Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	activeStyle, err := db.CreateStyle("ACTIVE", "", procID)
	if err != nil {
		t.Fatalf("create active style: %v", err)
	}
	inactiveStyle, err := db.CreateStyle("INACTIVE", "", procID)
	if err != nil {
		t.Fatalf("create inactive style: %v", err)
	}
	if err := db.SetActiveStyle(procID, &activeStyle); err != nil {
		t.Fatalf("set active: %v", err)
	}
	// Claim for WIDGET lives on the INACTIVE style.
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             inactiveStyle,
		CoreNodeName:        "CAL-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "WIDGET",
		AllowedPayloadCodes: []string{"WIDGET"},
		OutboundDestination: "FG-MARKET",
		UOPCapacity:         100,
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	if got := eng.FindLoaderForPayload("WIDGET"); got != nil {
		t.Errorf("FindLoaderForPayload should skip the inactive-style claim, got node %q", got.node.CoreNodeName)
	}
	if got := eng.FindAnyLoaderClaimForPayload("WIDGET"); got == nil {
		t.Error("FindAnyLoaderClaimForPayload should find the inactive-style claim, got nil")
	}
}
