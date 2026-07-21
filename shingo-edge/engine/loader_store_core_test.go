package engine

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// TestLoadersFromCore_SharedWindow pins the single read-model for a shared_window
// produce loader: it resolves from the synced cache through the *domain.Loader
// aggregate snapshot, identity is the loader_key token, and the config
// (inbound/outbound/min_stock/uop_threshold/payload set) is projected off the cache.
// (Migrated from the deleted manualSwapNode shim + cache-direct helpers onto the
// snapshot accessors — Tier 1.)
func TestLoadersFromCore_SharedWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	// Seed the loader into the Core-loader cache and warm the aggregate snapshot.
	seedCoreLoader(t, eng, protocol.LoaderInfo{
		Name: "L", LoaderKey: "loader:AGG-LOADER", Role: "produce", Layout: "shared_window", Replenishment: "threshold",
		OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "AGG-LOADER", Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", UOPThreshold: 100}},
	})

	l, err := eng.loaders().LoaderForPayload("PART-A", domain.RoleProduce, true)
	if err != nil || l == nil {
		t.Fatalf("LoaderForPayload(PART-A) = %v, %v; want the cached loader", l, err)
	}
	if l.ID() != "loader:AGG-LOADER" {
		t.Errorf("loader ID = %q, want the loader_key token loader:AGG-LOADER", l.ID())
	}
	if !l.IsShared() || l.Role() != domain.RoleProduce {
		t.Errorf("layout/role = %v/%v, want shared_window/produce", l.Layout(), l.Role())
	}
	if l.InboundSource() != "EMPTY-SUPER" || l.OutboundDest() != "FG-MARKET" {
		t.Errorf("inbound/outbound = %q/%q, want EMPTY-SUPER/FG-MARKET", l.InboundSource(), l.OutboundDest())
	}
	if got := l.Windows(); len(got) != 1 || got[0].Node != "AGG-LOADER" {
		t.Errorf("windows = %+v, want one at AGG-LOADER", got)
	}
	if !l.ServesPayload("PART-A") {
		t.Error("loader should serve PART-A")
	}
	if got := l.UOPThresholdFor("PART-A"); got != 100 {
		t.Errorf("UOPThresholdFor(PART-A) = %d, want 100 (cache threshold)", got)
	}
	if _, err := eng.loaders().LoaderForPayload("NOPE", domain.RoleProduce, true); !errors.Is(err, ErrLoaderNotFound) {
		t.Errorf("LoaderForPayload(NOPE) err = %v, want ErrLoaderNotFound", err)
	}
}

// TestLoadersFromCore_DedicatedAndUnloader pins the snapshot resolution of a
// dedicated_positions produce loader (position + per-position min_stock + layout +
// the replenishment=operator → transitional mapping) and a consume unloader
// (role + replenishment=auto). (Migrated onto the snapshot accessors — Tier 1.)
func TestLoadersFromCore_DedicatedAndUnloader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	seedCoreLoader(t, eng,
		protocol.LoaderInfo{Name: "HL", LoaderKey: "loader:HL-LOADER", Role: "produce", Layout: "dedicated_positions", Replenishment: "operator", ConfigGen: 1,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "POS-1", PayloadCode: "PART-H", Kind: "dedicated", UOPThreshold: 80}}},
		protocol.LoaderInfo{Name: "U", LoaderKey: "loader:UNL-1", Role: "consume", Layout: "shared_window", Replenishment: "operator", ConfigGen: 1,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "UNL-1", Kind: "window"}},
			Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-U"}}},
	)

	// dedicated_positions: resolves by payload to its position, carries the per-position
	// threshold + the dedicated layout, and replenishment=operator → operator-driven.
	l, err := eng.loaders().LoaderForPayload("PART-H", domain.RoleProduce, true)
	if err != nil || l == nil {
		t.Fatalf("LoaderForPayload(PART-H) = %v, %v; want the dedicated loader", l, err)
	}
	if !l.IsDedicated() {
		t.Errorf("layout = %v, want dedicated_positions", l.Layout())
	}
	if got := l.Positions(); len(got) != 1 || got[0].Node != "POS-1" {
		t.Errorf("positions = %+v, want one at POS-1", got)
	}
	if got := l.UOPThresholdFor("PART-H"); got != 80 {
		t.Errorf("UOPThresholdFor(PART-H) = %d, want 80 (position threshold)", got)
	}
	if !l.IsOperatorDriven() {
		t.Error("replenishment=operator loader should be operator-driven")
	}

	// unloader: consume — always operator (the window-queue drain).
	u, err := eng.loaders().LoaderForPayload("PART-U", domain.RoleConsume, true)
	if err != nil || u == nil {
		t.Fatalf("LoaderForPayload(PART-U, consume) = %v, %v; want the unloader", u, err)
	}
	if u.Role() != domain.RoleConsume {
		t.Errorf("role = %v, want consume", u.Role())
	}
	if u.Replenishment() != domain.ReplenishmentOperator {
		t.Errorf("replenishment = %v, want operator (consume drain)", u.Replenishment())
	}

	// Both loaders are in the snapshot (one produce + one consume).
	prod, _ := eng.loaders().Loaders(domain.RoleProduce)
	cons, _ := eng.loaders().Loaders(domain.RoleConsume)
	if len(prod) != 1 || len(cons) != 1 {
		t.Errorf("Loaders: produce=%d consume=%d, want 1 and 1", len(prod), len(cons))
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
		Layout: "shared_window", Replenishment: "threshold", ConfigGen: 1,
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", UOPThreshold: 100}},
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
