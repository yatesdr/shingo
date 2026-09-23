package engine

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// TestBuildView_DedicatedHomeGetsTheLoadDirective: the whole hop for a
// dedicated_positions loader — Core's directive setting, the cache, the
// dedicated projection branch, the station view. During a changeover whose
// incoming style wants the home's own pinned payload, the home's tile carries
// the directive naming that payload's carrier. The home-location board does not
// render it (only renderPayloadBoard reads changeover_load_directive); this pins
// the payload the view sends.
func TestBuildView_DedicatedHomeGetsTheLoadDirective(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng)
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
	eng.stationService.SetBinTypeResolver(func(p string) string {
		if p == "PART-NEW" {
			return "TOTE-BIG"
		}
		return ""
	})

	procID, err := db.CreateProcess("DHD-PROC", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: procID, Name: "DHD-STATION"})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, OperatorStationID: &sid, CoreNodeName: "DHD-H1",
		Code: "DHDH1", Name: "DHD-H1", Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create home node: %v", err)
	}
	fromStyleID, err := db.CreateStyle("DHD-FROM", "", procID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := db.CreateStyle("DHD-TO", "", procID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	if err := db.SetActiveStyle(procID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	// The incoming style's press wants PART-NEW — the home's pinned payload.
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "DHD-PRESS", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-NEW", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "MARKET", InboundStaging: "DHD-STAGE", OutboundStaging: "DHD-OUT",
	}); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by)
		VALUES (?, ?, ?, 'active', 'dedicated-directive-test')`, procID, fromStyleID, toStyleID); err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	if _, err := db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, toStyleID, procID); err != nil {
		t.Fatalf("set target style: %v", err)
	}

	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: "LDR-DHD", LoaderKey: "loader:903", Role: string(protocol.ClaimRoleProduce),
		Layout: "dedicated_positions", Replenishment: "operator", ConfigGen: 1,
		ChangeoverLoadDirective: true,
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "DHD-H1", PayloadCode: "PART-NEW", Kind: "position", HomeKind: "home"},
		},
	}})

	view, err := eng.stationService.BuildView(context.Background(), sid)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.ActiveChangeover == nil || len(view.Nodes) != 1 {
		t.Fatalf("fixture: active changeover %v, nodes %d; want an active changeover and one tile",
			view.ActiveChangeover, len(view.Nodes))
	}
	d := view.Nodes[0].ChangeoverLoadDirective
	if d == nil {
		t.Fatal("the dedicated home's tile carries no directive; want the carrier its pinned payload needs")
	}
	if len(d.BinTypeCodes) != 1 || d.BinTypeCodes[0] != "TOTE-BIG" {
		t.Errorf("directive bin types = %v, want [TOTE-BIG]", d.BinTypeCodes)
	}
	if len(d.ForNodes) != 1 || d.ForNodes[0] != "DHD-PRESS" {
		t.Errorf("directive for-nodes = %v, want [DHD-PRESS]", d.ForNodes)
	}
}
