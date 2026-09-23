package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestChangeoverLoadOrigin_DedicatedLoader: what the changeover load
// directive decides at a dedicated_positions (home-location) loader whose Core
// row has it set, during a changeover. The two readers of the setting are the
// station check (stationTakesLoadDirective — the same Loader the station
// view's copy reads) and the empty-request attribution (changeoverLoadOrigin).
// Both read the setting, so an empty requested at a home during the changeover
// is the changeover's demand. The dedicated projection branch used to drop the
// setting, and both read false.
func TestChangeoverLoadOrigin_DedicatedLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	processID, err := db.CreateProcess("CLD-PROC", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	node := &processes.Node{ID: 1, ProcessID: processID, CoreNodeName: "CLD-H1", Name: "CLD-H1"}
	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: "LDR-CLD", LoaderKey: "loader:902", Role: string(protocol.ClaimRoleProduce),
		Layout: "dedicated_positions", Replenishment: "operator", ConfigGen: 1,
		ChangeoverLoadDirective: true,
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "CLD-H1", PayloadCode: "PART-A", Kind: "position", HomeKind: "home"},
		},
	}})

	toStyleID, err := db.CreateStyle("CLD-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, started_at, updated_at)
		VALUES (?, ?, 'in_progress', datetime('now'), datetime('now'))`, processID, toStyleID)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	coID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("changeover id: %v", err)
	}
	if err := db.SetChangeoverOriginID(coID, "co-origin-ded"); err != nil {
		t.Fatalf("set changeover origin: %v", err)
	}

	claim := eng.synthLoaderClaim("CLD-H1")
	if claim == nil {
		t.Fatal("fixture: the home must resolve to a synthesized claim")
	}
	if !eng.stationTakesLoadDirective("CLD-H1") {
		t.Error("stationTakesLoadDirective = false; want true (the home's station is opted in)")
	}
	o := eng.changeoverLoadOrigin(node, claim)
	if o.ID != "co-origin-ded" || o.Class != protocol.OriginClassAttached {
		t.Errorf("changeoverLoadOrigin = %+v; want the changeover's episode, attached", o)
	}
}
