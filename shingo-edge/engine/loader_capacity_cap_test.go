package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// PR-0 capacity-cap regression suite — the SLN_002 incident class.
//
// The incident: a home-location loader configured as ONE core node carrying
// ~20 payloads. A single kanban demand signal swept every payload at minStock=2
// → ~40 retrieve_empty orders aimed at a one-bin window, all parked
// "destination occupied" and cancelled. The fix was two-fold: (1) a per-node
// capacity cap bounds total in-flight carriers at a core node to its physical
// window slots (one per window) — Core's now, for every loader ask
// (plan.LoaderWindows); (2) fallback-no-sweep stops an unrelated signal,
// resolved by payload-first-match, from fanning across the resolved loader's
// whole catalog. The unloader (consume) analog is pinned here, with what it
// costs in asks.

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
	procID, err := db.CreateProcess(proc, "", "", "", false)
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
	if _, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
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

// seedLoaderNode creates a process with one active style and a process_node at
// coreNode: a loader window's node as a station holds it. It writes no claim:
// the window's claim is the one SynthClaim builds from the Core loader, so a test
// that exercises the window must also seed that loader (seedCoreLoader), which
// warms the cache gate.
func seedLoaderNode(t *testing.T, db *store.DB, procName, coreNode string) (procID, nodeID, styleID int64) {
	t.Helper()
	var err error
	procID, err = db.CreateProcess(procName, "", "", "", false)
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
// test loader shape. repl is "threshold" or "operator". For a consume unloader
// pass role "consume". Both trailing ints are vestigial: the bin-count min_stock
// is retired, and the UOP threshold left the wire (it is Core's alone).
func sharedLoaderInfo(coreNode, role, repl, payload string, _, _ int) protocol.LoaderInfo {
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          role,
		Layout:        "shared_window",
		Replenishment: repl,
		InboundSource: "EMPTY-SUPER",
		OutboundDest:  "FG-MARKET",
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode}},
		Payloads:      []protocol.LoaderPayloadInfo{{PayloadCode: payload}},
	}
}

// multiPayloadLoaderInfo builds a shared_window LoaderInfo carrying several
// payloads — the many-payload loader the capacity-cap tests exercise. (The
// trailing int param is vestigial — the bin-count min_stock it set is retired.)
func multiPayloadLoaderInfo(coreNode, role, repl string, payloads []string, _ int) protocol.LoaderInfo {
	ps := make([]protocol.LoaderPayloadInfo, len(payloads))
	for i, p := range payloads {
		ps[i] = protocol.LoaderPayloadInfo{PayloadCode: p}
	}
	return protocol.LoaderInfo{
		Name:          coreNode,
		LoaderKey:     "loader:" + coreNode,
		Role:          role,
		Layout:        "shared_window",
		Replenishment: repl,
		InboundSource: "EMPTY-SUPER",
		OutboundDest:  "FG-MARKET",
		Positions:     []protocol.LoaderPosition{{CoreNodeName: coreNode}},
		Payloads:      ps,
	}
}

// NOTE: the produce bin-count cap tests (SweepBoundedByWindowSlots /
// FallbackRefillsOnlySignaledPayload) were REMOVED with the bin-count floor: the
// legacy produce DemandSignal sweep (retired: MaybeCreateLoaderEmptyIn / refillLoaderForPayload)
// is retired. The never-2N cap they exercised is Core's for every loader ask
// (dispatch's ported budget pins, loader_budget_port_docker_test.go).

// TestUnloaderCapacityCap_AutoPushBoundedByWindowSlots is the consume-side
// analog: a 20-payload AutoPush unloader's re-pull (rePushOwnUnloader, what the
// pickup gate calls, and the CLEAR / PUSH EMPTY / U2-landed gates when not
// covered locally) stages one full-in (U1) at its one window. The cost: ONE
// ask naming the twenty parts in the unloader's order; Core places the first
// and answers the other nineteen held.
func TestUnloaderCapacityCap_AutoPushBoundedByWindowSlots(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	core := answerLoaderAsks(t, eng)
	payloads := capTestPayloads(20)
	nodeID := seedCapManualSwap(t, db, "UNL-PROC", "UNLOADER-1", protocol.ClaimRoleConsume, payloads, 0, true)
	seedCoreLoader(t, eng, multiPayloadLoaderInfo("UNLOADER-1", "consume", "operator", payloads, 0))

	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read unloader node")
	eng.rePushOwnUnloader(node)
	awaitAsks(t, eng)

	if got := inFlightFulls(t, db, []string{"UNLOADER-1"}); got != 1 {
		t.Errorf("AutoPush unloader staged %d full-in orders, want 1 for the one-bin window", got)
	}
	if n := core.asks.Load(); n != 1 {
		t.Errorf("asks = %d, want 1 for the unloader", n)
	}
	ask := core.lastAsk()
	if ask.Origin != protocol.LoaderAskSideCycle || ask.PayloadCode != "" || len(ask.Payloads) != 20 || ask.Payloads[0] != payloads[0] || ask.Payloads[19] != payloads[19] {
		t.Errorf("ask = %+v, want a release's full-in naming the twenty parts in order", ask)
	}
}

// TestUnloaderCapacityCap_SweepPushBoundedByWindowSlots covers the startup
// sweep path (SweepPushUnloaders), which offers every payload in one ask as the gates'
// rePushOwnUnloader does and hits the same node cap on Core.
func TestUnloaderCapacityCap_SweepPushBoundedByWindowSlots(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	answerLoaderAsks(t, eng)
	payloads := capTestPayloads(20)
	seedCapManualSwap(t, db, "UNLSWEEP-PROC", "UNLOADER-2", protocol.ClaimRoleConsume, payloads, 0, true)
	seedCoreLoader(t, eng, multiPayloadLoaderInfo("UNLOADER-2", "consume", "operator", payloads, 0))

	eng.SweepPushUnloaders()
	awaitAsks(t, eng)

	if got := inFlightFulls(t, db, []string{"UNLOADER-2"}); got != 1 {
		t.Errorf("AutoPush unloader startup sweep staged %d full-in orders, want 1 for the one-bin window", got)
	}
}
