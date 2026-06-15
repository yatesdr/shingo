package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// buildMultiWindowStation seeds a process + active style + one operator station
// holding a single window node (the per-window-HMI model: one screen per window),
// with that window's active-style manual_swap claim. Returns the station id. The
// caller seeds the Core-loader cache to decide whether the node resolves to a
// multi-window, single-window, or no loader.
func buildMultiWindowStation(t *testing.T, db *store.DB, procName, windowNode string) int64 {
	t.Helper()
	procID, err := db.CreateProcess(procName, "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	styleID, err := db.CreateStyle("RUN", "", procID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(procID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: procID, Name: windowNode + "-STATION"})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, OperatorStationID: &sid, CoreNodeName: windowNode,
		Code: windowNode, Name: windowNode, Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: windowNode,
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeManualSwap,
		PayloadCode: "PART-A", AllowedPayloadCodes: []string{"PART-A"},
		InboundSource: "EMPTY-SUPER", OutboundDestination: "FG-MARKET", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	return sid
}

// TestBuildView_SharedWindow_TagsWindowGroup pins the view-path cutover (C4b):
// a manual_swap node that is one window of a shared MULTI-window loader gets its
// WindowGroupAnchor + WindowNodes populated from the Core aggregate — the
// membership the legacy per-node claim reads structurally cannot express. This is
// what lets the per-window operator board show "this window belongs to <loader>"
// and know it draws on a shared demand budget.
func TestBuildView_SharedWindow_TagsWindowGroup(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng) // explicit aggregate loader store
	// Wire the view to the aggregate resolver exactly as engine.New does (testEngine
	// builds the StationService without it).
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})

	sid := buildMultiWindowStation(t, db, "MW-PROC", "MW-W2")

	// One shared_window loader with three windows; the station holds MW-W2.
	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: "MW", LoaderKey: "loader:MW-LOADER", Role: "produce", Layout: "shared_window", Replenishment: "threshold",
		OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "MW-W1", Kind: protocol.LoaderPositionKindWindow},
			{CoreNodeName: "MW-W2", Kind: protocol.LoaderPositionKindWindow},
			{CoreNodeName: "MW-W3", Kind: protocol.LoaderPositionKindWindow},
		},
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", UOPThreshold: 100}},
	}})

	view, err := eng.stationService.BuildView(sid)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if len(view.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(view.Nodes))
	}
	nv := view.Nodes[0]
	if nv.WindowGroupAnchor != "loader:MW-LOADER" {
		t.Errorf("WindowGroupAnchor = %q, want loader:MW-LOADER (the loader identity token)", nv.WindowGroupAnchor)
	}
	if want := []string{"MW-W1", "MW-W2", "MW-W3"}; !slices.Equal(nv.WindowNodes, want) {
		t.Errorf("WindowNodes = %v, want %v", nv.WindowNodes, want)
	}
}

// TestBuildView_SingleWindowLoader_NoWindowGroup is the parity half: a loader
// with a single window (no homes configured — the legacy single-node shape) must
// NOT be tagged with a window group, so the view is byte-identical to the
// pre-cutover render for every loader that isn't actually multi-window.
func TestBuildView_SingleWindowLoader_NoWindowGroup(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng) // explicit aggregate loader store
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})

	// The station's node IS the loader's anchor (single-window: no positions).
	sid := buildMultiWindowStation(t, db, "SW-PROC", "SW-LOADER")
	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: "SW", LoaderKey: "loader:SW-LOADER", Role: "produce", Layout: "shared_window", Replenishment: "threshold",
		OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "SW-LOADER", Kind: protocol.LoaderPositionKindWindow}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", UOPThreshold: 100}},
	}})

	view, err := eng.stationService.BuildView(sid)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if len(view.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(view.Nodes))
	}
	if nv := view.Nodes[0]; nv.WindowGroupAnchor != "" || nv.WindowNodes != nil {
		t.Errorf("single-window loader tagged a group: anchor=%q windows=%v, want empty",
			nv.WindowGroupAnchor, nv.WindowNodes)
	}
}
