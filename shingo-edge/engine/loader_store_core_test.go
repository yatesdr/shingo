package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// TestLoadersFromCore_SharedWindow pins the aggregate read path for a
// shared_window produce loader: FindLoaderForPayload resolves from the synced
// cache, synthesizes a NodeClaim (node identity from the Edge process_node,
// config from the cache), and the threshold/min_stock helpers read the cache.
func TestLoadersFromCore_SharedWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("AGG-PROC", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "AGG-LOADER", Code: "AGG", Name: "AGG-LOADER", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		Name: "L", LoaderKey: "loader:AGG-LOADER", Role: "produce", Layout: "shared_window", Replenishment: "auto",
		OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "AGG-LOADER", Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", MinStock: 2, UOPThreshold: 100}},
	}}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	m := eng.FindLoaderForPayload("PART-A")
	if m == nil {
		t.Fatal("FindLoaderForPayload(PART-A) = nil from cache")
	}
	if m.node.ID != nodeID || m.node.CoreNodeName != "AGG-LOADER" {
		t.Errorf("node = %+v, want process_node %d / AGG-LOADER", m.node, nodeID)
	}
	if m.claim.Role != protocol.ClaimRoleProduce || m.claim.SwapMode != protocol.SwapModeManualSwap {
		t.Errorf("claim role/swap = %v/%v, want produce/manual_swap", m.claim.Role, m.claim.SwapMode)
	}
	if m.claim.ReorderPoint != 2 || m.claim.OutboundDestination != "FG-MARKET" || m.claim.InboundSource != "EMPTY-SUPER" {
		t.Errorf("claim = %+v, want ReorderPoint 2 / FG-MARKET / EMPTY-SUPER", m.claim)
	}
	if got := m.claim.AllowedPayloads(); len(got) != 1 || got[0] != "PART-A" {
		t.Errorf("allowed = %v, want [PART-A]", got)
	}
	if !eng.hasOptInLoaderThreshold("AGG-LOADER", "PART-A") {
		t.Error("hasOptInLoaderThreshold should be true (cache threshold 100)")
	}
	if ms, ok := eng.loaderMinStockFromCore("AGG-LOADER", "PART-A"); !ok || ms != 2 {
		t.Errorf("loaderMinStockFromCore = %d,%v, want 2,true", ms, ok)
	}
	if eng.FindLoaderForPayload("NOPE") != nil {
		t.Error("FindLoaderForPayload(NOPE) should be nil (not in cache)")
	}
}

// TestLoadersFromCore_DedicatedAndUnloader pins the dedicated_positions node
// resolution (the POSITION node, with its per-position min_stock), the
// replenishment=operator → transitional mapping, and the consume +
// replenishment=auto → AutoPush mapping.
func TestLoadersFromCore_DedicatedAndUnloader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("AGG2-PROC", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	posID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "POS-1", Code: "p1", Name: "POS-1", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create position node: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "UNL-1", Code: "u1", Name: "UNL-1", Sequence: 2, Enabled: true}); err != nil {
		t.Fatalf("create unloader node: %v", err)
	}

	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{
		{Name: "HL", LoaderKey: "loader:HL-LOADER", Role: "produce", Layout: "dedicated_positions", Replenishment: "operator", ConfigGen: 1,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "POS-1", PayloadCode: "PART-H", MinStock: 3}}},
		{Name: "U", LoaderKey: "loader:UNL-1", Role: "consume", Layout: "shared_window", Replenishment: "auto", ConfigGen: 1,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "UNL-1", Kind: "window"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-U"}}},
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// dedicated_positions: resolves the POSITION node + its per-position min_stock.
	m := eng.FindLoaderForPayload("PART-H")
	if m == nil || m.node.ID != posID {
		t.Fatalf("PART-H resolved %+v, want position node POS-1 (%d)", m, posID)
	}
	if m.claim.ReorderPoint != 3 {
		t.Errorf("ReorderPoint = %d, want 3 (position min_stock)", m.claim.ReorderPoint)
	}
	if !eng.isTransitionalLoader("POS-1") {
		t.Error("POS-1 should be transitional (replenishment=operator)")
	}

	// unloader: consume + replenishment=auto → AutoPush.
	u := eng.FindUnloaderForPayload("PART-U")
	if u == nil || u.claim.Role != protocol.ClaimRoleConsume {
		t.Fatalf("FindUnloaderForPayload(PART-U) = %+v, want a consume loader", u)
	}
	if !u.claim.AutoPush {
		t.Error("consume + replenishment=auto should map to AutoPush=true")
	}

	if all := eng.findManualSwapNodes(""); len(all) != 2 {
		t.Errorf("findManualSwapNodes = %d, want 2 (both cached loaders)", len(all))
	}
}

// TestLoaderIdentityCutover_ResolveByKey pins the step-4 cutover: a SYNTHETIC loader
// (identity is a loader_key token, anchor name is not a node) resolves by LoaderByKey,
// its ID() IS the token (not core_node_name), and the anchor name does not resolve as
// the identity — while its real windows still resolve via LoaderAt (the member path).
// This is the atomic flip the threshold signal relies on; a synthetic loader could not
// be resolved by LoaderAt(anchor) at all.
func TestLoaderIdentityCutover_ResolveByKey(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	procID, err := db.CreateProcess("CUT-PROC", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	for _, w := range []string{"CUT-W1", "CUT-W2"} {
		if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: w, Code: w, Name: w, Sequence: 1, Enabled: true}); err != nil {
			t.Fatalf("create window %s: %v", w, err)
		}
	}

	// PLK_SYN is a synthetic identity (no process_node); loader_key is the real identity.
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		Name: "Syn", LoaderKey: "loader:77", Role: "produce",
		Layout: "shared_window", Replenishment: "auto", ConfigGen: 1,
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", MinStock: 2, UOPThreshold: 100}},
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "CUT-W1", Kind: protocol.LoaderPositionKindWindow},
			{CoreNodeName: "CUT-W2", Kind: protocol.LoaderPositionKindWindow},
		},
	}}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	// Build the aggregate store against the freshly-seeded cache (its constructor
	// Refresh()es the immutable snapshot from ListCoreLoaders).
	eng.loaderStore = newAggregateLoaderStore(eng.db, eng.logFn)

	// Resolves by the IDENTITY token; ID() is the token, not core_node_name.
	l, err := eng.loaders().LoaderByKey("loader:77", domain.RoleProduce)
	if err != nil || l == nil {
		t.Fatalf("LoaderByKey(loader:77) = %v, %v; want the loader", l, err)
	}
	if l.ID() != "loader:77" {
		t.Errorf("loader ID = %q, want loader:77 (the token, not the synthetic core_node_name)", l.ID())
	}
	// The synthetic anchor name is NOT the identity.
	if got, _ := eng.loaders().LoaderByKey("PLK_SYN", domain.RoleProduce); got != nil {
		t.Errorf("LoaderByKey(PLK_SYN) resolved %v; the anchor name is not the loader key", got)
	}
	// But its real windows still resolve via the member path.
	if at, err := eng.loaders().LoaderAt("CUT-W1", domain.RoleProduce); err != nil || at == nil || at.ID() != "loader:77" {
		t.Errorf("LoaderAt(CUT-W1) = %v, %v; want the loader by its window", at, err)
	}
}

// TestSendClaimSync_Retired pins that SendClaimSync is a no-op: under the
// Core-owned aggregate, Core derives the demand registry from bin_loaders, so the
// Edge must not push style_node_claims up and clobber it.
func TestSendClaimSync_Retired(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	sent := 0
	eng.SetSendFunc(func(*protocol.Envelope) error { sent++; return nil })

	eng.SendClaimSync()
	if sent != 0 {
		t.Errorf("SendClaimSync sent %d envelope(s); want 0 (retired)", sent)
	}
}
