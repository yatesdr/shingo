package engine

import (
	"context"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
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
	procID, err := db.CreateProcess(procName, "", "", "", false)
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
	if _, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: windowNode,
		Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeManualSwap,
		PayloadCode: "PART-A", AllowedPayloadCodes: []string{"PART-A"},
		InboundSource: "EMPTY-SUPER", OutboundDestination: "FG-MARKET", UOPCapacity: 100,
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	return sid
}

// TestBuildView_SharedWindow_CarriesTheLoader: a node that is one window of a
// shared multi-window loader carries the loader as the board reads it, from the
// Core aggregate: its role, layout, replenishment, outbound and parts. The
// board draws each window of the station from its own tile, so no window-group
// anchor or sibling list rides the tile (they had no reader).
func TestBuildView_SharedWindow_CarriesTheLoader(t *testing.T) {
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
		OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "MW-W1"},
			{CoreNodeName: "MW-W2"},
			{CoreNodeName: "MW-W3"},
		},
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", UOPThreshold: 100}},
	}})

	view, err := eng.stationService.BuildView(context.Background(), sid)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if len(view.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(view.Nodes))
	}
	l := view.Nodes[0].Loader
	if l == nil {
		t.Fatal("the window's tile carries no loader")
	}
	want := domain.TileLoader{Role: "produce", Layout: "shared_window", Replenishment: "threshold",
		OutboundDest: "FG-MARKET", Parts: []domain.TileLoaderPart{{Code: "PART-A"}}}
	if l.Role != want.Role || l.Layout != want.Layout || l.Replenishment != want.Replenishment ||
		l.OutboundDest != want.OutboundDest || !slices.Equal(l.Parts, want.Parts) || l.Carriers != nil {
		t.Errorf("loader = %+v, want %+v", *l, want)
	}
}
