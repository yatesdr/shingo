package engine

import (
	"encoding/json"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// station_view_loader_pins_test.go — characterisations of the loader and
// unloader station as it is today, through BuildView with the loader resolver
// wired (wiredLoaderEngine) and every loader seeded as Core's. Each pin states
// today's outcome, including the ones that are wrong; a change to any of them
// is a changed outcome and has to be named as one.
//
// The glass half (what the board draws from these tiles, and what each tap
// posts) is www/static/operator-station/operator-loader-station.test.js.

// tileFor returns the tile for a core node, failing when it is not drawn.
func tileFor(t *testing.T, view *domain.OperatorStationView, coreNode string) *domain.StationNodeView {
	t.Helper()
	for i := range view.Nodes {
		if view.Nodes[i].Node.CoreNodeName == coreNode {
			return &view.Nodes[i]
		}
	}
	t.Fatalf("no tile for %s on the board", coreNode)
	return nil
}

// stationProcessStyle gives a loader station's process an active style, for
// the fixtures that need a stored claim on it.
func stationProcessStyle(t *testing.T, db *store.DB, stationID int64, name string) (procID, styleID int64) {
	t.Helper()
	st, err := db.GetOperatorStation(stationID)
	if err != nil {
		t.Fatalf("get station: %v", err)
	}
	styleID, err = db.CreateStyle(name, "", st.ProcessID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(st.ProcessID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	return st.ProcessID, styleID
}

// ── today's precedence ──────────────────────────────────────────────

// TestLoaderPrecedence_CoreLoaderWinsOverAStoredClaim pins who answers at a
// node Core's loader names when a stored claim also sits there: the LOADER, in
// the view (buildNodeTile) and in the engine (claimAtNode) alike, and the
// loadable set (loadablePayloads) is the loader's. The stored claim at that node
// is not read as the tile's or the engine's claim (an owner ruling).
//
// The stored claim here is an ordinary single-robot consume claim written
// through the allowlisted door, beside a window with no stored claim at all:
// both windows read as the loader's, and both carry the loader. Nor is the
// stored claim a cell the board serves: at a node Core's loader names it is not
// read, by the served pass either (isLoaderClaim).
//
// Before the served pass: the lineside pass read the stored claim as a cell's
// and credited SMN_P2's 30 to PART-A on both windows. Now no row (owner
// ruling: a loader is not a cell it serves; loader claims are not rows).
//
// Before (the pin at the base): the stored claim won at SMN_P2, in the view and
// the engine, and hid the loader from the tile.
func TestLoaderPrecedence_CoreLoaderWinsOverAStoredClaim(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := wiredLoaderEngine(t, db)
	sid, ids := loaderStation(t, db, "LDR-PREC", "SMN_P1", "SMN_P2")
	_, styleID := stationProcessStyle(t, db, sid, "PREC-RUN")
	cellClaim(t, db, styleID, "SMN_P2", protocol.ClaimRoleConsume, []string{"PART-A"})
	if _, err := db.EnsureProcessNodeRuntime(ids["SMN_P2"]); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	if err := db.SetProcessNodeRuntime(ids["SMN_P2"], nil, 30); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Prec", LoaderKey: "loader:21", Role: "produce", Layout: "shared_window",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: windows("SMN_P1", "SMN_P2"),
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}, {PayloadCode: "PART-B"}},
	})

	view := buildLoaderView(t, eng, sid)

	for _, core := range []string{"SMN_P1", "SMN_P2"} {
		tile := tileFor(t, view, core)
		if tile.ActiveClaim == nil || tile.ActiveClaim.SwapMode != protocol.SwapModeManualSwap || tile.ActiveClaim.ID != 0 {
			t.Fatalf("%s claim = %+v, want the loader's synthesised manual_swap claim", core, tile.ActiveClaim)
		}
		if tile.Loader == nil || tile.Loader.Replenishment != "operator" || len(tile.Loader.Parts) != 2 {
			t.Errorf("%s loader = %+v, want the operator loader with both parts", core, tile.Loader)
		}
	}
	if view.Serves == nil || len(view.Serves) != 0 {
		t.Errorf("serves = %+v, want [] (read): the stored claim at the loader's window is not a cell", view.Serves)
	}

	// The engine's resolver agrees with the tile: the loader first.
	node2, err := db.GetProcessNode(ids["SMN_P2"])
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	claim2 := eng.claimAtNode(node2)
	if claim2 == nil || claim2.SwapMode != protocol.SwapModeManualSwap || claim2.ID != 0 {
		t.Fatalf("claimAtNode(SMN_P2) = %+v, want the loader's synthesised claim", claim2)
	}
	if err := requireLoaderClaim(node2, claim2); err != nil {
		t.Errorf("requireLoaderClaim at SMN_P2 = %v, want nil (a loader window)", err)
	}
	if got := eng.loadablePayloads(node2, claim2); !slices.Equal(got, []string{"PART-A", "PART-B"}) {
		t.Errorf("loadablePayloads(SMN_P2) = %v, want the loader's [PART-A PART-B]", got)
	}
	node1, err := db.GetProcessNode(ids["SMN_P1"])
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if c := eng.claimAtNode(node1); c == nil || c.SwapMode != protocol.SwapModeManualSwap || c.ID != 0 {
		t.Errorf("claimAtNode(SMN_P1) = %+v, want the synthesised loader claim", c)
	}
}

// ── the served rows ─────────────────────────────────────────────────

// servedOf indexes a view's served rows by cell, part and incoming, failing on
// a duplicate.
func servedOf(t *testing.T, view *domain.OperatorStationView) map[string]domain.ServedRow {
	t.Helper()
	out := map[string]domain.ServedRow{}
	for _, r := range view.Serves {
		k := r.Cell + "/" + r.Part
		if r.Incoming {
			k += "/incoming"
		}
		if _, dup := out[k]; dup {
			t.Fatalf("two rows for %s", k)
		}
		out[k] = r
	}
	return out
}

// haveOf is a row's count, or a sentinel no fixture produces when it has none.
func haveOf(r domain.ServedRow) int {
	if r.HaveUOP == nil {
		return -1 << 30
	}
	return *r.HaveUOP
}

// TestLoaderLineside_CreditsByPayload pins the served rows a loader board
// carries (view.serves), the source of a part's lineside on the board:
//
//   - a node's bin is credited to the part the bin holds (with no identity
//     established, the running claim's part, Engine.binAtNode's rule) and each
//     pile to its own part, so CELL-X's PART-B reads its pile alone;
//   - the same core node claimed by two processes is a row per process node
//     (the board sums them: PART-C 7 + 9);
//   - only running and incoming styles' consume claims are rows.
//
// Before (the lineside sums): CELL-X's whole 35 was credited to both its parts,
// PART-A and PART-B, so a cell changing over read the outgoing bin's count as
// the incoming part's; and the starved mark came from Core's reorder threshold,
// which sits at or above a bin and so reads every running part as low. It is no
// longer sent: the board grades a part by its coverage.
func TestLoaderLineside_CreditsByPayload(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := wiredLoaderEngine(t, db)
	sid, _ := loaderStation(t, db, "LDR-SUMS", "SMN_L1")
	_, _, nodeX := servedCell(t, db, "CELL-X", "WLN_X", []string{"PART-A", "PART-B"}, 30)
	if _, err := db.CaptureLinesideBucket(nodeX, "PART-B", 5); err != nil {
		t.Fatalf("capture pile: %v", err)
	}
	servedCell(t, db, "CELL-Y", "WLN_S", []string{"PART-C"}, 7)
	servedCell(t, db, "CELL-Z", "WLN_S", []string{"PART-C"}, 9)
	// A cell whose PART-D claim is on a style that is not running.
	idleProc, err := db.CreateProcess("CELL-IDLE", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	idleStyle, err := db.CreateStyle("IDLE-NEXT", "", idleProc)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: idleProc, CoreNodeName: "WLN_I", Code: "WLN_I", Name: "WLN_I", Enabled: true}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	cellClaim(t, db, idleStyle, "WLN_I", protocol.ClaimRoleConsume, []string{"PART-D"})

	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Sums", LoaderKey: "loader:22", Role: "produce", Layout: "shared_window",
		Replenishment: "threshold", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: windows("SMN_L1"),
		Payloads: []protocol.LoaderPayloadInfo{
			{PayloadCode: "PART-A", UOPThreshold: 50}, {PayloadCode: "PART-B", UOPThreshold: 10},
			{PayloadCode: "PART-C", UOPThreshold: 16}, {PayloadCode: "PART-D", UOPThreshold: 99},
		},
	})

	rows := servedOf(t, buildLoaderView(t, eng, sid))
	want := map[string]int{"CELL-X/PART-A": 30, "CELL-X/PART-B": 5, "CELL-Y/PART-C": 7, "CELL-Z/PART-C": 9}
	if len(rows) != len(want) {
		t.Errorf("rows = %v, want exactly %v (the idle style's PART-D is no row)", rows, want)
	}
	for k, n := range want {
		r, ok := rows[k]
		if !ok || haveOf(r) != n || r.Role != "consume" {
			t.Errorf("%s = %+v (ok=%v), want a consume row with %d", k, r, ok, n)
		}
	}
	if r := rows["CELL-X/PART-A"]; r.Carrier != "TOTE-1" || r.Node != "WLN_X" || r.ProcessNodeID == 0 {
		t.Errorf("CELL-X/PART-A = %+v, want carrier TOTE-1 at WLN_X with its process node", r)
	}
}

// TestDedicatedUnloader_ServesProduceRowsOnly is the server half of the fixed
// "a dedicated-position unloader reads ACTIVE and Lineside 0 UoP on every
// home": an unloader's board is served by the cells that make its parts, so
// with nothing making them it carries serves = [] and the glass draws no home
// ACTIVE (operator-loader-station.test.js, "dedicated unloader").
//
// Before: the lineside pass skipped consume tiles, so each home kept its pinned
// part as active_style_payloads whatever ran.
func TestDedicatedUnloader_ServesProduceRowsOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := wiredLoaderEngine(t, db)
	sid, _ := loaderStation(t, db, "UNL-HOMES", "SMN_U1", "SMN_U2")
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Unloader homes", LoaderKey: "loader:23", Role: "consume", Layout: "dedicated_positions",
		Replenishment: "operator", OutboundDest: "EMPTY-SUPER", InboundSource: "FG-MARKET",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "SMN_U1", PayloadCode: "PART-A", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 1},
			{CoreNodeName: "SMN_U2", PayloadCode: "PART-B", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 2},
		},
	})
	// A cell CONSUMING PART-A is no row of an unloader's board.
	servedCell(t, db, "CELL-USE", "WLN_U", []string{"PART-A"}, 4)

	view := buildLoaderView(t, eng, sid)
	if view.Serves == nil || len(view.Serves) != 0 {
		t.Errorf("serves = %+v, want [] (read, no cell makes its parts)", view.Serves)
	}
	for node, part := range map[string]string{"SMN_U1": "PART-A", "SMN_U2": "PART-B"} {
		l := tileFor(t, view, node).Loader
		if l == nil || l.Layout != "dedicated_positions" || l.Role != "consume" ||
			len(l.Parts) != 1 || l.Parts[0].Code != part {
			t.Errorf("%s loader = %+v, want the dedicated unloader's home of %s", node, l, part)
		}
	}
}

// TestLoaderTile_NoPartRunningIsAnEmptyList is the server half of the fixed
// "no part is running arrives as no data": with no running style using any of
// the loader's parts, the view carries serves: [] (read, nothing running),
// which the glass tells apart from an absent key (could not look), so a queued
// call is no longer escalated as downtime (operator-loader-station.test.js,
// "no part running").
//
// Before: the pass emptied active_style_payloads and the key was omitted.
func TestLoaderTile_NoPartRunningIsAnEmptyList(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := wiredLoaderEngine(t, db)
	sid, ids := loaderStation(t, db, "LDR-IDLE", "SMN_N1")
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Idle", LoaderKey: "loader:24", Role: "produce", Layout: "shared_window",
		Replenishment: "threshold", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: windows("SMN_N1"),
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A", UOPThreshold: 50}},
	})
	loaderOrder(t, db, "idle-l1", protocol.OrderTypeRetrieve, ids["SMN_N1"], "SMN_N1", "", "PART-A", "queued")

	raw, err := json.Marshal(buildLoaderView(t, eng, sid))
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	if got := string(keys["serves"]); got != "[]" {
		t.Errorf("serves = %q, want [] with nothing running", got)
	}
}

// ── the incoming rows ───────────────────────────────────────────────

// TestServedRows_IncomingRowsAndWaiting pins the incoming rows the board grades
// (operator-board-model.js coverage: drawn only while waiting):
//
//   - a served cell changing over brings its target style's rows of the
//     board's role, marked incoming, counted by the same pass;
//   - one rule for every served cell, the station's own process included;
//   - an incoming row waits while Core lists its part missing (consume rows)
//     or the cell has it queued with Core's reason; the loader's directive
//     setting gates nothing here.
//
// Before (the directive): it read the station's own process only, and its
// incoming PRODUCE claims whatever the board's role, and named every incoming
// part whether or not anything was short. A loader acts only when a served cell
// is short of what it makes, and a loader makes what its cells consume.
func TestServedRows_IncomingRowsAndWaiting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		role        string
		ownProcess  bool // the changeover is the loader station's own process
		incoming    protocol.ClaimRole
		part        string
		missing     bool // Core's verdict lists the incoming part missing
		queued      bool // the cell has the incoming part queued with Core's reason
		wantRow     bool
		wantWaiting bool
		wantRole    string
	}{
		{name: "served cell, incoming consume part, nothing short", role: "produce",
			incoming: protocol.ClaimRoleConsume, part: "PART-B", wantRow: true, wantRole: "consume"},
		{name: "served cell, incoming part missing", role: "produce", incoming: protocol.ClaimRoleConsume,
			part: "PART-B", missing: true, wantRow: true, wantWaiting: true, wantRole: "consume"},
		{name: "served cell, incoming part queued", role: "produce", incoming: protocol.ClaimRoleConsume,
			part: "PART-B", queued: true, wantRow: true, wantWaiting: true, wantRole: "consume"},
		{name: "the station's own process, incoming part missing", role: "produce", ownProcess: true,
			incoming: protocol.ClaimRoleConsume, part: "PART-B", missing: true, wantRow: true, wantWaiting: true,
			wantRole: "consume"},
		{name: "part the loader does not serve", role: "produce", incoming: protocol.ClaimRoleConsume,
			part: "PART-C", missing: true},
		{name: "incoming produce claim on a loader's board", role: "produce", incoming: protocol.ClaimRoleProduce,
			part: "PART-B", missing: true},
		{name: "unloader, incoming produce part missing", role: "consume", incoming: protocol.ClaimRoleProduce,
			part: "PART-B", missing: true, wantRow: true, wantRole: "produce"},
		{name: "unloader, incoming produce part queued", role: "consume", incoming: protocol.ClaimRoleProduce,
			part: "PART-B", queued: true, wantRow: true, wantWaiting: true, wantRole: "produce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := wiredLoaderEngine(t, db)
			sid, _ := loaderStation(t, db, "LDR-CO", "SMN_C1")
			procName, styleName := "CELL-CO", "CO-TO"
			var procID, nodeID int64
			if tc.ownProcess {
				procName = "LDR-CO"
				procID, _ = stationProcessStyle(t, db, sid, "CO-FROM")
				id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "WLN_C1",
					Code: "WLN_C1", Name: "WLN_C1", Sequence: 9, Enabled: true})
				if err != nil {
					t.Fatalf("create node: %v", err)
				}
				nodeID = id
				if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
					t.Fatalf("ensure runtime: %v", err)
				}
			} else {
				procID, _, nodeID = servedCell(t, db, "CELL-CO", "WLN_C1", []string{"PART-A"}, 10)
			}
			toStyle, err := db.CreateStyle(styleName, "", procID)
			if err != nil {
				t.Fatalf("create to style: %v", err)
			}
			cellClaim(t, db, toStyle, "WLN_C1", tc.incoming, []string{tc.part})
			if _, err := db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, toStyle, procID); err != nil {
				t.Fatalf("set target style: %v", err)
			}
			if tc.missing {
				if err := db.ReplaceSourcingState([]protocol.SourcingState{
					{ProcessID: procName, StyleID: styleName, Status: "red", Missing: []string{tc.part}},
				}); err != nil {
					t.Fatalf("sourcing state: %v", err)
				}
			}
			if tc.queued {
				id := loaderOrder(t, db, "co-q", protocol.OrderTypeRetrieve, nodeID, "WLN_C1", "", tc.part, "queued")
				if _, err := db.Exec(`UPDATE orders SET queue_reason='no bin in the market' WHERE id=?`, id); err != nil {
					t.Fatalf("queue reason: %v", err)
				}
			}
			setCoreLoaders(t, eng, protocol.LoaderInfo{
				Name: "CO", LoaderKey: "loader:25", Role: tc.role, Layout: "shared_window",
				Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
				ChangeoverLoadDirective: true,
				Positions:               windows("SMN_C1"),
				Payloads:                []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}, {PayloadCode: "PART-B"}},
			})

			view := buildLoaderView(t, eng, sid)
			r, ok := servedOf(t, view)[procName+"/"+tc.part+"/incoming"]
			if ok != tc.wantRow {
				t.Fatalf("incoming row present = %v, want %v; rows %+v", ok, tc.wantRow, view.Serves)
			}
			if !ok {
				return
			}
			// The incoming row counts the same way every row does: the cell's
			// bin holds its running part, so the incoming part reads 0.
			if r.Waiting != tc.wantWaiting || haveOf(r) != 0 || r.Role != tc.wantRole || r.Node != "WLN_C1" {
				t.Errorf("row = %+v, want %s, waiting=%v, have 0 at WLN_C1", r, tc.wantRole, tc.wantWaiting)
			}
		})
	}
}
