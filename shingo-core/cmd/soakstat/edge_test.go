package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
)

// edgeSnapshot is the Edge's committed schema snapshot, which the Edge's own
// tests hold to its migrations: the tables seeded from it are the real shape.
const edgeSnapshot = "../../../shingo-edge/store/schema/schema.snapshot.sql"

// edgeTableDDL pulls one CREATE TABLE out of the snapshot.
func edgeTableDDL(t *testing.T, table string) string {
	t.Helper()
	b, err := os.ReadFile(edgeSnapshot)
	if err != nil {
		t.Fatalf("read the Edge schema snapshot: %v", err)
	}
	m := regexp.MustCompile(`(?s)CREATE TABLE ` + table + ` \(.*?\n\);`).Find(b)
	if m == nil {
		t.Fatalf("no CREATE TABLE %s in the Edge schema snapshot", table)
	}
	return string(m)
}

func lsReport(t *testing.T, at time.Time, station string, entries ...protocol.LinesideLevelEntry) []byte {
	t.Helper()
	env, err := protocol.NewDataEnvelope(protocol.SubjectLinesideLevelReport,
		protocol.Address{Role: protocol.RoleEdge, Station: station}, protocol.Address{Role: protocol.RoleCore},
		&protocol.LinesideLevelReport{Station: station, ReportedAt: at, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	env.Timestamp = at
	b, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seat(node string, bins, uop, pile int) protocol.LinesideLevelEntry {
	return protocol.LinesideLevelEntry{CoreNodeName: node, BinCount: bins, BinUOP: uop, BucketQty: pile}
}

// TestEdgeStarvation_ASeatIsStarvedOnlyWithNoCarrierCountAndNoPile: a seat
// with no carrier, or a carrier at 0, and no pile is starved for the interval
// its report opens; a carrier at 0 with a pile, or a counted carrier, is not; a
// seat missing from a report is unknown for it; and an interval far past the
// cadence is a gap, counted for nobody.
func TestEdgeStarvation_ASeatIsStarvedOnlyWithNoCarrierCountAndNoPile(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	samples := []linesideSample{
		{at: at(0), entries: []protocol.LinesideLevelEntry{seat("A", 1, 0, 0), seat("B", 1, 40, 0)}},
		{at: at(5), entries: []protocol.LinesideLevelEntry{seat("A", 0, 0, 0), seat("B", 1, 0, 12)}},
		{at: at(10), entries: []protocol.LinesideLevelEntry{seat("A", 1, 30, 0)}},
		{at: at(15), entries: []protocol.LinesideLevelEntry{seat("A", 0, 0, 0), seat("B", 0, 0, 0)}},
		{at: at(60), entries: []protocol.LinesideLevelEntry{seat("A", 0, 0, 0), seat("B", 0, 0, 0)}}, // after a gap
		{at: at(65)},
	}
	seats, gaps := starvation(samples)
	if gaps != 1 {
		t.Errorf("gaps = %d, want the one 45-min interval", gaps)
	}
	got := map[string]seatStarve{}
	for _, s := range seats {
		got[s.node] = s
	}
	if a := got["A"]; a.starved != 15*time.Minute || a.observed != 20*time.Minute {
		t.Errorf("A starved %v of %v, want 15m of 20m", a.starved, a.observed)
	}
	if b := got["B"]; b.starved != 5*time.Minute || b.unknown != 5*time.Minute || b.observed != 15*time.Minute {
		t.Errorf("B starved %v, unknown %v, observed %v; want 5m, 5m, 15m", b.starved, b.unknown, b.observed)
	}
}

// TestEdge_ReadsAnEdgeDatabaseReadOnly seeds the Edge's own tables (from its
// schema snapshot) in an -edge-dir layout and reads them back: the station from
// its reports, its owned nodes, the ticks it holds, the starvation, and the
// log's not-found lines; then proves the open refuses a write.
func TestEdge_ReadsAnEdgeDatabaseReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	svc := filepath.Join(dir, "edge-1")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(svc, "shingoedge.db")
	w, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"process_nodes", "process_node_runtime_states", "outbox"} {
		if _, err := w.Exec(edgeTableDDL(t, table)); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO process_nodes (id, process_id, core_node_name, code, name) VALUES (1, 1, 'ALN_001', 'a', 'a'), (2, 1, 'ALN_002', 'b', 'b')`)
	mustExec(`INSERT INTO process_node_runtime_states (process_node_id, pending_uop_delta) VALUES (1, 7), (2, 0)`)
	t0 := time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)
	for i, e := range []protocol.LinesideLevelEntry{seat("ALN_001", 0, 0, 0), seat("ALN_001", 1, 50, 0), seat("ALN_001", 1, 49, 0)} {
		mustExec(`INSERT INTO outbox (topic, payload, msg_type) VALUES ('orders', ?, ?)`,
			lsReport(t, t0.Add(time.Duration(i)*5*time.Minute), "plant.line-1", e), protocol.SubjectLinesideLevelReport)
	}
	mustExec(`INSERT INTO outbox (topic, payload, msg_type) VALUES ('orders', X'7b7d', 'inventory.bin_uop_delta')`)
	w.Close()
	log := "uop_adjustment: process node \"SLN_004\" not found: sql: no rows\n" +
		"uop_adjustment: process node \"UTN_001\" not found: sql: no rows\n" +
		"uop_adjustment: bound bin 12 to node ALN_001 (remaining=50 epoch=3, moved in Core)\n"
	if err := os.WriteFile(filepath.Join(svc, "edge.log"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	sources, err := edgeSources(nil, dir)
	if err != nil || len(sources) != 1 || sources[0].logPath == "" {
		t.Fatalf("edge-dir found %+v (%v), want edge-1 with its log", sources, err)
	}
	e, err := readEdge(sources[0])
	if err != nil {
		t.Fatal(err)
	}
	if e.station != "plant.line-1" || !e.owned["ALN_001"] || !e.owned["ALN_002"] || e.owned["SLN_004"] {
		t.Errorf("station %q owns %v; want plant.line-1 owning ALN_001 and ALN_002", e.station, e.owned)
	}
	if len(e.pending) != 1 || e.pending[0].node != "ALN_001" || e.pending[0].n != 7 {
		t.Errorf("pending = %+v, want ALN_001 holding 7", e.pending)
	}
	if e.reports != 3 || len(e.seats) != 1 || e.seats[0].starved != 5*time.Minute {
		t.Errorf("%d report(s), seats %+v; want 3 and ALN_001 starved 5m", e.reports, e.seats)
	}
	if !e.logRead || e.logNotFound != 2 || e.logBound != 1 {
		t.Errorf("log read %v: not-found %d, bound %d; want 2 and 1", e.logRead, e.logNotFound, e.logBound)
	}

	ro, err := openEdgeReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`DELETE FROM outbox`); err == nil {
		t.Fatal("the Edge database opened for reading accepted a write")
	}
}

// TestHandoff_PlacementsSplitByTheEdgesOwnNodesOverCoresMirror: every Edge
// receives every placement; an Edge read from its database is judged by its
// process nodes, one without by Core's mirror, and an Edge Core does not know
// still gets its row.
func TestHandoff_PlacementsSplitByTheEdgesOwnNodesOverCoresMirror(t *testing.T) {
	t.Parallel()
	h := &handoffCore{
		placements: []nodeCount{{node: "SLN_001", class: "SLN", n: 10}, {node: "ALN_001", class: "ALN", n: 4}},
		stations:   []string{"line-1", "line-2"},
		claims:     map[string]map[string]bool{"line-1": {"ALN_001": true}, "line-2": {}},
	}
	edges := []*edgeReading{
		{station: "line-2", owned: map[string]bool{"SLN_001": true}},
		{station: "line-9", owned: map[string]bool{}},
		nil, // an Edge that failed to read
	}
	got := map[string]placementSplit{}
	for _, p := range splitPlacements(h, edges) {
		got[p.station] = p
	}
	if p := got["line-1"]; p.ownedBy != "core-mirror" || p.owned != 4 || p.unownedByClass["SLN"] != 10 {
		t.Errorf("line-1 = %+v, want 4 owned by Core's mirror and SLN 10 unowned", p)
	}
	if p := got["line-2"]; p.ownedBy != "edge-db" || p.owned != 10 || p.unowned != 4 {
		t.Errorf("line-2 = %+v, want its own process nodes to own the 10 SLN placements", p)
	}
	if p := got["line-9"]; p.unowned != 14 {
		t.Errorf("line-9 = %+v, want all 14 unowned", p)
	}
	if !strings.Contains(classText(map[string]int{"SLN": 3, "ALN": 9}), "ALN 9, SLN 3") {
		t.Error("classText does not order by count")
	}
	if nodeClass("SLN_001", "") != "SLN" || nodeClass("STORAGE-A1", "STOR") != "STORAGE-A1/STOR" {
		t.Error("nodeClass does not read prefix and type")
	}
}
