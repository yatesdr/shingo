package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// station_view_loader_golden_test.go — a whole-payload pin of BuildView for
// every loader layout, with the loader resolver WIRED the way engine.New wires
// it (stationLoaderResolver over the aggregate LoaderStore, fed by
// SetCoreLoaders from Core's loader list).
//
// service/station_view_golden_test.go pins a press board and carries no loader
// tile at all, so every loader field on the view was unpinned as a whole. These
// goldens hold today's shape for the five layouts the board draws:
//
//   - payload_one_window   a produce shared-window loader with one window
//   - payload_two_windows  the same layout with two windows on one station
//   - dedicated_homes      a produce dedicated-positions loader, two homes on
//                          the station and two buffer spots that are not
//   - slot_grid            a consume shared-window loader (the slot-grid
//                          unloader), three windows
//   - two_stage            a stage-1 unloader that leaves the cart bare beside
//                          a stage-2 unloader pulled directly by the line
//
// THE LOADERS ARE CORE'S, NEVER STORED CLAIMS. Every loader here arrives as a
// protocol.LoaderInfo through SetCoreLoaders and reaches the tile through
// SynthClaim. No fixture writes a manual_swap row: that mode is refused by the
// claim allowlist and a stored one is quarantined on the next sync, so a golden
// built on one would pin a state no door can build.
//
// Each golden is rendered twice and the two renders compared before the file
// is: a field that moves between two builds of the same state is not a shape.
//
// Regenerate with:
//
//	go test ./engine/ -run TestBuildView_LoaderGolden -update-loader-view-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
//
// The same run logs each board's cost as the poll pays it: statements on the
// first build and on the next, and the bytes of the station JSON as the
// handler writes it (apiGetOperatorStationView), raw and gzipped. Logged, not
// asserted: the poll budgets live in service/composer_budget_pins_test.go.

var updateLoaderViewGolden = flag.Bool("update-loader-view-golden", false,
	"rewrite testdata/station_view_loader_*.json")

// Timestamps are wall-clock and the cell version is seeded from the clock (see
// service/station_view_golden_test.go for both); scrubbed, not frozen.
var (
	loaderGoldenTS      = regexp.MustCompile(`"(\d{4}-\d{2}-\d{2}T[^"]*)"`)
	loaderGoldenVersion = regexp.MustCompile(`"cell_version": "[^"]*"`)
)

// loaderBoards is the fixture table. Each build seeds one station and returns
// its id; the Core loaders are installed through eng.SetCoreLoaders.
var loaderBoards = []struct {
	name  string
	build func(t *testing.T, db *store.DB, eng *Engine) int64
}{
	{"payload_one_window", buildPayloadOneWindowBoard},
	{"payload_two_windows", buildPayloadTwoWindowBoard},
	{"dedicated_homes", buildDedicatedHomesBoard},
	{"slot_grid", buildSlotGridBoard},
	{"two_stage", buildTwoStageBoard},
	{"dedicated_22_homes", buildTwentyTwoHomeBoard},
}

func TestBuildView_LoaderGolden(t *testing.T) {
	for _, board := range loaderBoards {
		t.Run(board.name, func(t *testing.T) {
			db, counter := testEngineDBCounting(t)
			eng := wiredLoaderEngine(t, db)
			stationID := board.build(t, db, eng)

			counter.Reset()
			first := buildLoaderView(t, eng, stationID)
			firstStatements := counter.Count()
			counter.Reset()
			second := buildLoaderView(t, eng, stationID)
			steadyStatements := counter.Count()

			got, again := goldenLoaderJSON(t, first), goldenLoaderJSON(t, second)
			if !bytes.Equal(got, again) {
				t.Fatalf("two builds of the same state differ:\n--- first ---\n%s\n--- second ---\n%s", got, again)
			}

			raw, gz := handlerBytes(t, second)
			t.Logf("cost %s: statements first=%d steady=%d; station JSON raw=%d gzip=%d bytes",
				board.name, firstStatements, steadyStatements, raw, gz)

			path := filepath.Join("testdata", "station_view_loader_"+board.name+".json")
			if *updateLoaderViewGolden {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				t.Logf("golden regenerated at %s: read the diff before committing", path)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (regenerate with -update-loader-view-golden): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("BuildView payload for %s changed.\n\nIf the change is intended, regenerate with\n"+
					"  go test ./engine/ -run TestBuildView_LoaderGolden -update-loader-view-golden\n"+
					"and read the diff.\n\n--- want ---\n%s\n--- got ---\n%s", board.name, want, got)
			}
		})
	}
}

// wiredLoaderEngine is testEngine with the view wired as engine.New wires it:
// the aggregate loader store, the station view resolving through it, and a
// payload-to-bin-type catalog for the changeover directive.
func wiredLoaderEngine(t *testing.T, db *store.DB) *Engine {
	t.Helper()
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng)
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
	eng.stationService.SetBinTypeResolver(func(p string) string {
		return map[string]string{"PART-A": "TOTE-1", "PART-B": "TOTE-2", "PART-C": "KD-1"}[p]
	})
	return eng
}

func buildLoaderView(t *testing.T, eng *Engine, stationID int64) *domain.OperatorStationView {
	t.Helper()
	view, err := eng.stationService.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	return view
}

func goldenLoaderJSON(t *testing.T, view *domain.OperatorStationView) []byte {
	t.Helper()
	got, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	got = loaderGoldenTS.ReplaceAll(got, []byte(`"<ts>"`))
	got = loaderGoldenVersion.ReplaceAll(got, []byte(`"cell_version": "<version>"`))
	return append(got, '\n')
}

// handlerBytes is the station JSON as apiGetOperatorStationView writes it: the
// view inside the same wrapper, its station without the clocks (www
// viewStation), gzipped at the router's level (middleware.Compress(5)). The
// wrapper's sourcing field is empty here, as on an Edge with no sourcing
// state; the Core
// enrichments (bin_state, containment, curtain) are not stamped, as nothing
// answers for Core in this harness.
func handlerBytes(t *testing.T, view *domain.OperatorStationView) (raw, gz int) {
	t.Helper()
	station := map[string]any{}
	if b, err := json.Marshal(view.Station); err != nil || json.Unmarshal(b, &station) != nil {
		t.Fatalf("station: %v", err)
	}
	for _, clock := range []string{"last_seen_at", "created_at", "updated_at"} {
		delete(station, clock)
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(struct {
		*domain.OperatorStationView
		Station         map[string]any `json:"station"`
		SourcingByStyle map[string]any `json:"sourcing_by_style,omitempty"`
	}{OperatorStationView: view, Station: station}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var zipped bytes.Buffer
	zw, err := gzip.NewWriterLevel(&zipped, 5)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if _, err := zw.Write(buf.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Len(), zipped.Len()
}

// ── fixture helpers ─────────────────────────────────────────────────

// loaderStation seeds a process with no style (a loader's process carries no
// claims of its own: the loader is Core's) and one station holding the named
// nodes, in order. Returns the station id and the node ids by core name.
func loaderStation(t *testing.T, db *store.DB, procName string, nodes ...string) (int64, map[string]int64) {
	t.Helper()
	procID, err := db.CreateProcess(procName, "", "", "", false)
	if err != nil {
		t.Fatalf("create process %s: %v", procName, err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: procID, Code: procName + "-ST", Name: procName + " Station", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	ids := map[string]int64{}
	for i, n := range nodes {
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, OperatorStationID: &sid, CoreNodeName: n,
			Code: n, Name: n, Sequence: i + 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create node %s: %v", n, err)
		}
		ids[n] = id
	}
	return sid, ids
}

// servedCell seeds a cell process whose ACTIVE style has one consume claim at
// coreNode allowing payloads, the bin there counting binUOP. It is what the
// lineside pass walks: an active consume claim, anywhere on the Edge.
func servedCell(t *testing.T, db *store.DB, procName, coreNode string, payloads []string, binUOP int) (procID, styleID, nodeID int64) {
	t.Helper()
	procID, err := db.CreateProcess(procName, "", "", "", false)
	if err != nil {
		t.Fatalf("create process %s: %v", procName, err)
	}
	styleID, err = db.CreateStyle(procName+"-RUN", "", procID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(procID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: coreNode, Code: coreNode, Name: coreNode, Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create cell node: %v", err)
	}
	cellClaim(t, db, styleID, coreNode, protocol.ClaimRoleConsume, payloads)
	// Ensure first: the set is a bare UPDATE and runtime rows are created
	// lazily, so without the row it would change nothing.
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure cell runtime: %v", err)
	}
	if err := db.SetProcessNodeRuntime(nodeID, nil, binUOP); err != nil {
		t.Fatalf("set cell runtime: %v", err)
	}
	return procID, styleID, nodeID
}

// cellClaim upserts an ordinary single-robot claim through the allowlisted
// door: a press or a cell, never a loader.
func cellClaim(t *testing.T, db *store.DB, styleID int64, coreNode string, role protocol.ClaimRole, payloads []string) int64 {
	t.Helper()
	id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: coreNode, Role: role,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: payloads[0], AllowedPayloadCodes: payloads,
		UOPCapacity: 100, InboundSource: "MARKET", OutboundDestination: "MARKET",
		InboundStaging: coreNode + "-IN", OutboundStaging: coreNode + "-OUT",
	})
	if err != nil {
		t.Fatalf("upsert %s claim at %s: %v", role, coreNode, err)
	}
	return id
}

// loaderOrder files an order on a loader node in the given status. The status
// is written directly: the fixture wants the state, not the path to it.
func loaderOrder(t *testing.T, db *store.DB, uuid string, typ protocol.OrderType, nodeID int64,
	delivery, source, payload, status string) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, typ, &nodeID, typ == protocol.OrderTypeRetrieve && payload == "",
		1, delivery, "", source, protocol.Height{}, false, payload, "", "")
	if err != nil {
		t.Fatalf("create order %s: %v", uuid, err)
	}
	if _, err := db.Exec(`UPDATE orders SET status=? WHERE id=?`, status, id); err != nil {
		t.Fatalf("set order %s status: %v", uuid, err)
	}
	return id
}

func setCoreLoaders(t *testing.T, eng *Engine, infos ...protocol.LoaderInfo) {
	t.Helper()
	if err := eng.SetCoreLoaders(infos); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
}

func windows(names ...string) []protocol.LoaderPosition {
	out := make([]protocol.LoaderPosition, 0, len(names))
	for i, n := range names {
		out = append(out, protocol.LoaderPosition{CoreNodeName: n, Ordinal: i + 1})
	}
	return out
}

// ── the five boards ─────────────────────────────────────────────────

// A produce shared-window loader with one window, on a threshold, serving a
// cell that runs PART-A: a queued call for PART-A, a standing refusal of
// PART-B, and the cell's bin and pile for the lineside pass.
func buildPayloadOneWindowBoard(t *testing.T, db *store.DB, eng *Engine) int64 {
	sid, ids := loaderStation(t, db, "LDR-ONE", "SMN_A1")
	_, _, cellNode := servedCell(t, db, "CELL-1", "WLN_1", []string{"PART-A"}, 30)
	if _, err := db.CaptureLinesideBucket(cellNode, "PART-A", 5); err != nil {
		t.Fatalf("capture pile: %v", err)
	}
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Loader one", LoaderKey: "loader:11", Role: "produce", Layout: "shared_window",
		Replenishment: "threshold", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: windows("SMN_A1"),
		Payloads: []protocol.LoaderPayloadInfo{
			{PayloadCode: "PART-A"},
			{PayloadCode: "PART-B"},
		},
	})
	loaderOrder(t, db, "one-l1", protocol.OrderTypeRetrieve, ids["SMN_A1"], "SMN_A1", "", "PART-A", "queued")
	if err := db.OpenSupplyRefusal("SMN_A1", "PART-B", "Loader one"); err != nil {
		t.Fatalf("open refusal: %v", err)
	}
	return sid
}

// The same layout with two windows on one station: an empty delivered to the
// second window, nothing at the first.
func buildPayloadTwoWindowBoard(t *testing.T, db *store.DB, eng *Engine) int64 {
	sid, ids := loaderStation(t, db, "LDR-TWO", "SMN_B1", "SMN_B2")
	servedCell(t, db, "CELL-2", "WLN_2", []string{"PART-A"}, 10)
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Loader two", LoaderKey: "loader:12", Role: "produce", Layout: "shared_window",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: windows("SMN_B1", "SMN_B2"),
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
	})
	loaderOrder(t, db, "two-l1", protocol.OrderTypeRetrieve, ids["SMN_B2"], "SMN_B2", "", "", "delivered")
	return sid
}

// A produce dedicated-positions loader: two homes on the station, two buffer
// spots that are not on it. The first home has a blank-payload move of its
// emptied carrier to a buffer spot in flight, the shape the board reads as a
// clear in flight (service bufferStrip).
func buildDedicatedHomesBoard(t *testing.T, db *store.DB, eng *Engine) int64 {
	sid, ids := loaderStation(t, db, "LDR-HOMES", "SMN_H1", "SMN_H2")
	servedCell(t, db, "CELL-3", "WLN_3", []string{"PART-A"}, 12)
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Homes", LoaderKey: "loader:13", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "SMN_H1", PayloadCode: "PART-A", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 1},
			{CoreNodeName: "SMN_H2", PayloadCode: "PART-B", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 2},
			{CoreNodeName: "SMN_F1", HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 3},
			{CoreNodeName: "SMN_F2", HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 4},
		},
	})
	loaderOrder(t, db, "homes-order-a", protocol.OrderTypeMove, ids["SMN_H1"], "SMN_F1", "SMN_H1", "", "queued")
	return sid
}

// A consume shared-window loader, the slot-grid unloader: three windows, a
// full of PART-A delivered to the first.
func buildSlotGridBoard(t *testing.T, db *store.DB, eng *Engine) int64 {
	sid, ids := loaderStation(t, db, "UNL-SLOTS", "ULN_A1", "ULN_A2", "ULN_A3")
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Slots", LoaderKey: "loader:14", Role: "consume", Layout: "shared_window",
		Replenishment: "operator", OutboundDest: "EMPTY-SUPER", InboundSource: "FG-MARKET",
		Positions: windows("ULN_A1", "ULN_A2", "ULN_A3"),
		Payloads: []protocol.LoaderPayloadInfo{
			{PayloadCode: "PART-A"}, {PayloadCode: "PART-B"},
		},
	})
	loaderOrder(t, db, "slots-u1", protocol.OrderTypeRetrieve, ids["ULN_A1"], "ULN_A1", "", "PART-A", "delivered")
	return sid
}

// A two-stage unloader on one station: stage 1's two windows leave the cart
// bare, stage 2's window is pulled directly by the line.
func buildTwoStageBoard(t *testing.T, db *store.DB, eng *Engine) int64 {
	sid, _ := loaderStation(t, db, "UNL-TWO-STAGE", "ST1_A1", "ST1_A2", "ST2_A1")
	setCoreLoaders(t, eng,
		protocol.LoaderInfo{
			Name: "Stage 1", LoaderKey: "loader:15", Role: "consume", Layout: "shared_window",
			Replenishment: "operator", OutboundDest: "ST2-GROUP", InboundSource: "FG-MARKET",
			LeavesBare: true,
			Positions:  windows("ST1_A1", "ST1_A2"),
			Payloads:   []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
		protocol.LoaderInfo{
			Name: "Stage 2", LoaderKey: "loader:16", Role: "consume", Layout: "shared_window",
			Replenishment: "operator", OutboundDest: "EMPTY-SUPER", InboundSource: "ST1-GROUP",
			PulledDirectly: true,
			Positions:      windows("ST2_A1"),
			Payloads:       []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
		},
	)
	return sid
}

// A dedicated loader the size of the biggest board the plants run: 22 homes,
// each pinned to its own part, then 9 buffer spots, serving five cells of four
// or five parts each; one home's clear in flight. Made-up names. The cost the
// poll pays for it is what the Pi pays on its heaviest board.
func buildTwentyTwoHomeBoard(t *testing.T, db *store.DB, eng *Engine) int64 {
	var homes []string
	var positions []protocol.LoaderPosition
	for i := 1; i <= 22; i++ {
		home := fmt.Sprintf("SMN_H%02d", i)
		homes = append(homes, home)
		positions = append(positions, protocol.LoaderPosition{CoreNodeName: home,
			PayloadCode: fmt.Sprintf("PART-%02d", i), HomeKind: protocol.LoaderHomeKindHome, Ordinal: i})
	}
	for i := 1; i <= 9; i++ {
		positions = append(positions, protocol.LoaderPosition{CoreNodeName: fmt.Sprintf("SMN_F%d", i),
			HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 22 + i})
	}
	sid, ids := loaderStation(t, db, "LDR-22", homes...)
	for c := 0; c < 5; c++ {
		var parts []string
		for i := c*5 + 1; i <= c*5+5 && i <= 22; i++ {
			parts = append(parts, fmt.Sprintf("PART-%02d", i))
		}
		servedCell(t, db, fmt.Sprintf("CELL-%d", c+1), fmt.Sprintf("WLN_%d", c+1), parts, 10*(c+1))
	}
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Homes-22", LoaderKey: "loader:22", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: positions,
	})
	loaderOrder(t, db, "homes22-order-a", protocol.OrderTypeMove, ids["SMN_H03"], "SMN_F2", "SMN_H03", "", "queued")
	return sid
}
