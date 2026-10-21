package engine

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/internal/testdb"
)

// station_view_read_set_test.go — the tables a station view reads, pinned.
//
// The board does not poll: it refetches when a door that writes one of its
// inputs says so (the /api nudge, EventBoardNudge, the scoped events), and the
// door walk (cmd/shingoedge door_walk_test.go) proves every door does. That
// proof is only as wide as the inputs the walk's fixtures move. A NEW table
// read by BuildView is a new input whose writers nobody has walked, so it
// fails here until its doors are named below and walked.
//
// Read from the statements BuildView sends (store.QueryCounter), over every
// loader layout's board and a press board seeded from a real plant's setup.

// stationViewTables maps each table BuildView reads to the doors that write
// it. Every route and inbound key named is run by the door walk; the PLC count
// is the one writer that is neither, and announces itself (counter-update).
//
// The set is read from these boards' states: a branch of BuildView that only a
// state none of them is in reaches (an active changeover's node tasks) is not
// seen here, and is the walk's to cover through the changeover routes.
var stationViewTables = map[string]string{
	"node_lineside_bucket":        "POST /api/lineside/buckets/{id}/clear, the release verbs; the PLC count",
	"operator_stations":           "/api/operator-stations (POST, PUT, move, DELETE), POST /api/loader-boards",
	"orders":                      "/api/orders/*, the process-node verbs, the changeover routes; inbound order.*, order.projected, order.status_response",
	"payload_catalog":             "POST /api/payload-catalog/sync; inbound catalog.payloads_response",
	"process_changeovers":         "/api/processes/{id}/changeover/*",
	"process_node_runtime_states": "the process-node verbs; inbound transit.bin_landed, inventory.uop_adjustment, transit.bin_picked_up; the PLC count",
	"process_nodes":               "/api/process-nodes (POST, PUT, curtain-setting), PUT .../claimed-nodes, POST /api/loader-boards",
	"processes":                   "/api/processes (POST, PUT, PATCH, DELETE, active-style, group)",
	"style_node_claims":           "/api/style-node-claims, POST /api/styles/{id}/claims/copy-to, clone, generate, flow/save",
	"styles":                      "/api/styles (POST, PUT, DELETE, restore, clone, generate)",
	"supply_refusals_open":        "/api/process-nodes/{id}/supply-refusal (POST, DELETE, ack); inbound supply.refusal_state, supply.refusal_snapshot",
}

var sqlTable = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_]*)`)

func TestBuildView_ReadSetIsPinned(t *testing.T) {
	read := map[string]bool{}
	note := func(stmts []string) {
		for _, q := range stmts {
			for _, m := range sqlTable.FindAllStringSubmatch(q, -1) {
				read[strings.ToLower(m[1])] = true
			}
		}
	}
	for _, board := range loaderBoards {
		db, counter := testEngineDBCounting(t)
		eng := wiredLoaderEngine(t, db)
		sid := board.build(t, db, eng)
		counter.Reset()
		buildLoaderView(t, eng, sid)
		note(counter.Statements())
	}
	{
		db, counter := testEngineDBCounting(t)
		eng := wiredLoaderEngine(t, db)
		seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
		counter.Reset()
		if _, err := eng.stationService.BuildView(context.Background(), seeded.Stations["screen-a4"]); err != nil {
			t.Fatalf("BuildView press: %v", err)
		}
		note(counter.Statements())
	}
	var got []string
	for tbl := range read {
		got = append(got, tbl)
	}
	sort.Strings(got)
	var want []string
	for tbl := range stationViewTables {
		want = append(want, tbl)
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("BuildView reads %v\npinned          %v\n\nA table read by the station view is an input of the board, which "+
			"refetches only when a door says so. Name the doors that write a new table in stationViewTables, and "+
			"walk them (cmd/shingoedge door_walk_test.go), before pinning it.", got, want)
	}
}
