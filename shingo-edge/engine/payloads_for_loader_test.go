package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestPayloadsForLoader_UnionsAcrossProcessesActiveVsAll pins the multi-process
// union that powers the shared-loader board: active = payloads from every
// active style across all processes sharing the core node; all = those plus
// inactive-style payloads. This is the read BuildView feeds the HMI so an
// operator at a loader shared by two cells sees both cells' payloads.
func TestPayloadsForLoader_UnionsAcrossProcessesActiveVsAll(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	// Two active processes share SHARED-LOADER with disjoint active payloads.
	seedActiveManualSwapLoader(t, db, "SNF2", "SHARED-LOADER", "PART-A")
	seedActiveManualSwapLoader(t, db, "SNF3", "SHARED-LOADER", "PART-B")

	// A third process whose ACTIVE style does not claim the loader, but an
	// INACTIVE style does (PART-C) — contributes to `all` only.
	procID, err := db.CreateProcess("SNF4", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "SHARED-LOADER", Code: "s4", Name: "s4", Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	activeStyle, err := db.CreateStyle("ACT", "", procID)
	if err != nil {
		t.Fatalf("create active style: %v", err)
	}
	inactiveStyle, err := db.CreateStyle("INACT", "", procID)
	if err != nil {
		t.Fatalf("create inactive style: %v", err)
	}
	if err := db.SetActiveStyle(procID, &activeStyle); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: inactiveStyle, CoreNodeName: "SHARED-LOADER",
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeManualSwap,
		PayloadCode: "PART-C", AllowedPayloadCodes: []string{"PART-C"},
		InboundSource: "EMPTY-SUPER", OutboundDestination: "FG-MARKET", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("upsert inactive claim: %v", err)
	}

	active, all, _, err := processes.PayloadsForLoader(db.DB, "SHARED-LOADER", protocol.ClaimRoleProduce)
	if err != nil {
		t.Fatalf("PayloadsForLoader: %v", err)
	}
	if want := []string{"PART-A", "PART-B"}; !slices.Equal(active, want) {
		t.Errorf("active = %v, want %v (active styles only, not the inactive PART-C)", active, want)
	}
	if want := []string{"PART-A", "PART-B", "PART-C"}; !slices.Equal(all, want) {
		t.Errorf("all = %v, want %v (every style)", all, want)
	}
}

// TestFindLoaderForDemand_RoutesToSignaledCoreNode pins the legacy-path twin
// of the threshold routing fix: a DemandSignal naming a specific loader
// resolves to that loader (not the alphabetically-first one) when the same
// payload is loaded at two loaders, and falls back to first-match when no
// node is named.
func TestFindLoaderForDemand_RoutesToSignaledCoreNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	seedActiveManualSwapLoader(t, db, "AAA-PROC", "LOADER-A", "SHARED")
	seedActiveManualSwapLoader(t, db, "BBB-PROC", "LOADER-B", "SHARED")

	if got := eng.findLoaderForDemand("LOADER-B", "SHARED"); got == nil || got.node.CoreNodeName != "LOADER-B" {
		t.Errorf("signaled LOADER-B but resolved %v, want LOADER-B", got)
	}
	if got := eng.findLoaderForDemand("", "SHARED"); got == nil || got.node.CoreNodeName != "LOADER-A" {
		t.Errorf("no node named: fallback resolved %v, want first-match LOADER-A", got)
	}
}
