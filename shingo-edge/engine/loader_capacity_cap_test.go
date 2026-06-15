package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// PR-0 capacity-cap regression suite — the SLN_002 incident class.
//
// The incident: a home-location loader configured as ONE core node carrying
// ~20 payloads. A single kanban demand signal swept every payload at minStock=2
// → ~40 retrieve_empty orders aimed at a one-bin window, all parked
// "destination occupied" and cancelled. The fix is two-fold and both parts are
// pinned here: (1) the per-node capacity cap in tryCreateL1 bounds total
// in-flight empties at a core node to its physical window slots
// (manualSwapWindowSlots); (2) fallback-no-sweep stops an unrelated signal,
// resolved by payload-first-match, from fanning across the resolved loader's
// whole catalog. The unloader (consume) analog is covered too.

func capTestPayloads(n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("PART-%02d", i)
	}
	return out
}

// seedCapManualSwap seeds an active manual_swap claim at coreNode whose single
// claim lists every payload in payloads (the SLN_002 shape: many payloads on
// one physical node). Returns the process_node id.
func seedCapManualSwap(t *testing.T, db *store.DB, proc, coreNode string, role protocol.ClaimRole, payloads []string, reorderPoint int, autoPush bool) int64 {
	t.Helper()
	procID, err := db.CreateProcess(proc, "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process %s: %v", proc, err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: coreNode, Code: coreNode, Name: coreNode, Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node %s: %v", coreNode, err)
	}
	styleID, err := db.CreateStyle(proc+"-STYLE", "", procID)
	if err != nil {
		t.Fatalf("create style for %s: %v", proc, err)
	}
	if err := db.SetActiveStyle(procID, &styleID); err != nil {
		t.Fatalf("set active style for %s: %v", proc, err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        coreNode,
		Role:                role,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         payloads[0],
		AllowedPayloadCodes: payloads,
		ReorderPoint:        reorderPoint,
		AutoPush:            autoPush,
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FG-MARKET",
		UOPCapacity:         100,
	}); err != nil {
		t.Fatalf("upsert claim for %s: %v", coreNode, err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	return nodeID
}

// capActiveOrders returns the active orders at a process node split by
// direction: empties (retrieve_empty, the loader L1 shape) and fulls
// (!retrieve_empty, the unloader U1 shape).
func capActiveOrders(t *testing.T, db *store.DB, nodeID int64, wantEmpty bool) []orders.Order {
	t.Helper()
	ords, err := db.ListActiveOrdersByProcessNode(nodeID)
	if err != nil {
		t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
	}
	var out []orders.Order
	for _, o := range ords {
		if o.RetrieveEmpty == wantEmpty {
			out = append(out, o)
		}
	}
	return out
}

// seedActiveManualSwapLoader creates a process with one active style + a produce
// manual_swap claim + matching process_node, all targeting coreNode for payload.
// The process_node is what loader resolution needs (node identity); the
// style/claim remain for the bin-op tests that assert on them. Loader CONFIG now
// comes from the Core-loader cache — a test that exercises resolution must also
// call eng.SetCoreLoaders with the matching loader (which warms the cache gate).
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

// seedCoreLoader writes one loader into the Edge Core-loader cache and warms the
// cache gate (SetCoreLoaders), so the aggregate resolvers resolve it and a
// threshold signal doesn't park as "not synced yet". Full-state replace — pass
// every loader a test needs in ONE call.
func seedCoreLoader(t *testing.T, eng *Engine, infos ...protocol.LoaderInfo) {
	t.Helper()
	eng.SetCoreLoaders(infos)
}

// sharedLoaderInfo builds a single-payload shared_window LoaderInfo — the common
// test loader shape. repl is "auto" (automatic replenishment) or "operator"
// (transitional / operator-driven). For a consume unloader pass role "consume".
func sharedLoaderInfo(coreNode, role, repl, payload string, minStock, uopThreshold int) protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          role,
		Layout:        "shared_window",
		Replenishment: repl,
		InboundSource: "EMPTY-SUPER",
		OutboundDest:  "FG-MARKET",
		ConfigGen:     1,
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode, Kind: "window"}},
		Payloads:      []protocol.LoaderPayloadInfo{{PayloadCode: payload, MinStock: minStock, UOPThreshold: uopThreshold}},
	}
}

// multiPayloadLoaderInfo builds a shared_window LoaderInfo carrying several
// payloads (each at the same minStock — the produce L1 deficit driver) — the
// many-payload loader the capacity-cap tests exercise.
func multiPayloadLoaderInfo(coreNode, role, repl string, payloads []string, minStock int) protocol.LoaderInfo {
	ps := make([]protocol.LoaderPayloadInfo, len(payloads))
	for i, p := range payloads {
		ps[i] = protocol.LoaderPayloadInfo{PayloadCode: p, MinStock: minStock}
	}
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          role,
		Layout:        "shared_window",
		Replenishment: repl,
		InboundSource: "EMPTY-SUPER",
		OutboundDest:  "FG-MARKET",
		ConfigGen:     1,
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode, Kind: "window"}},
		Payloads:      ps,
	}
}

// TestLoaderCapacityCap_SweepBoundedByWindowSlots is the config-fuzz property:
// for arbitrary (payloadCount, minStock, preInFlight) a single demand signal
// must never leave more in-flight empties at a one-window loader's core node
// than the window can physically stage. Pre-PR-0 a 20-payload loader at
// minStock 2 created ~40; the cap bounds it to manualSwapWindowSlots.
func TestLoaderCapacityCap_SweepBoundedByWindowSlots(t *testing.T) {
	t.Parallel()
	cases := []struct{ payloads, minStock, preInFlight int }{
		{1, 2, 0},  // baseline
		{3, 2, 0},  // small multi-payload
		{20, 2, 0}, // the SLN_002 shape: 20 payloads × minStock 2
		{20, 5, 0}, // higher minStock — still bounded by physical slots
		{5, 1, 0},  // minStock 1
		{20, 0, 0}, // ReorderPoint unset + no threshold → no policy → fires nothing
		{20, 2, 1}, // one empty already in flight → no additional fires
		{3, 3, 1},  // partial pre-fill
		{1, 2, 1},  // single payload already staged
	}
	for i, tc := range cases {
		name := fmt.Sprintf("payloads=%d_minStock=%d_pre=%d", tc.payloads, tc.minStock, tc.preInFlight)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			payloads := capTestPayloads(tc.payloads)
			nodeID := seedCapManualSwap(t, db, fmt.Sprintf("PROP-%d", i), "LOADER-1", protocol.ClaimRoleProduce, payloads, tc.minStock, false)
			seedCoreLoader(t, eng, multiPayloadLoaderInfo("LOADER-1", "produce", "auto", payloads, tc.minStock))

			for range tc.preInFlight {
				if _, err := eng.orderMgr.CreateRetrieveOrder(
					&nodeID, true, 1, "LOADER-1", "EMPTY-SUPER", "", "standard", payloads[0], false, true,
				); err != nil {
					t.Fatalf("seed pre-in-flight empty: %v", err)
				}
			}

			// Exact-node signal → non-fallback → sweeps every allowed payload.
			eng.MaybeCreateLoaderEmptyIn("LOADER-1", payloads[0])

			if got := len(capActiveOrders(t, db, nodeID, true)); got > manualSwapWindowSlots {
				t.Errorf("%s: %d in-flight empties at the window exceeds physical cap %d", name, got, manualSwapWindowSlots)
			}
		})
	}
}

// TestLoaderCapacityCap_FallbackRefillsOnlySignaledPayload pins the Factor-C
// fix: when a demand signal names a node with no matching loader, resolution
// falls back to payload-first-match; the resolved (many-payload) loader must
// refill ONLY the signaled payload, never sweep its whole catalog. The signaled
// payload is deliberately not first in the list so a stray full-catalog sweep
// (which the cap would still bound to one order, but for the WRONG payload)
// would be caught by the payload-code assertion.
func TestLoaderCapacityCap_FallbackRefillsOnlySignaledPayload(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	payloads := capTestPayloads(20)
	nodeID := seedCapManualSwap(t, db, "FB-PROC", "LOADER-2", protocol.ClaimRoleProduce, payloads, 2, false)
	seedCoreLoader(t, eng, multiPayloadLoaderInfo("LOADER-2", "produce", "auto", payloads, 2))

	eng.MaybeCreateLoaderEmptyIn("SOME-OTHER-NODE", payloads[7])

	empties := capActiveOrders(t, db, nodeID, true)
	if len(empties) != 1 {
		t.Fatalf("fallback must refill only the signaled payload (capped to 1), got %d empties", len(empties))
	}
	if empties[0].PayloadCode != payloads[7] {
		t.Errorf("fallback refilled %s, want only the signaled payload %s (no full-catalog sweep)", empties[0].PayloadCode, payloads[7])
	}
}

// TestUnloaderCapacityCap_AutoPushBoundedByWindowSlots is the consume-side
// analog: a 20-payload AutoPush unloader sweep (MaybePushUnloader) must not
// stage more full-in (U1) orders at the window than it can physically hold.
func TestUnloaderCapacityCap_AutoPushBoundedByWindowSlots(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	payloads := capTestPayloads(20)
	nodeID := seedCapManualSwap(t, db, "UNL-PROC", "UNLOADER-1", protocol.ClaimRoleConsume, payloads, 0, true)
	seedCoreLoader(t, eng, multiPayloadLoaderInfo("UNLOADER-1", "consume", "auto", payloads, 0))

	eng.MaybePushUnloader(0) // 0 = sweep every matching unloader

	if got := len(capActiveOrders(t, db, nodeID, false)); got > manualSwapWindowSlots {
		t.Errorf("AutoPush unloader staged %d full-in orders, exceeds window cap %d", got, manualSwapWindowSlots)
	}
}

// TestUnloaderCapacityCap_SweepPushBoundedByWindowSlots covers the startup
// sweep path (SweepPushUnloaders), which loops the same per-payload create as
// MaybePushUnloader and must hit the same node cap.
func TestUnloaderCapacityCap_SweepPushBoundedByWindowSlots(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	payloads := capTestPayloads(20)
	nodeID := seedCapManualSwap(t, db, "UNLSWEEP-PROC", "UNLOADER-2", protocol.ClaimRoleConsume, payloads, 0, true)
	seedCoreLoader(t, eng, multiPayloadLoaderInfo("UNLOADER-2", "consume", "auto", payloads, 0))

	eng.SweepPushUnloaders()

	if got := len(capActiveOrders(t, db, nodeID, false)); got > manualSwapWindowSlots {
		t.Errorf("AutoPush unloader startup sweep staged %d full-in orders, exceeds window cap %d", got, manualSwapWindowSlots)
	}
}
