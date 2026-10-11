package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// [S.6] reads the sim counter's file beside the database and folds the log by
// component, docker prefix and both timestamps stripped.
func TestEdgeWork_ReadsStatementsAndLogComponents(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "shingoedge.db")
	tsv := "SELECT orders\t12\nUPDATE bins\t3\n"
	if err := os.WriteFile(filepath.Join(dir, "edge-statements.tsv"), []byte(tsv), 0o644); err != nil {
		t.Fatal(err)
	}
	log := []byte(
		"edge-1  | 2026-10-10T20:28:00.353622785Z 2026-10-10T20:28:00.353Z [plc] counter delta: rp=6\n" +
			"edge-1  | 2026-10-10T20:28:00.401531279Z 2026-10-10T20:28:00.401Z [production_ticks] sent n=5\n" +
			"2026-10-10T20:28:01.000Z [plc] counter delta: rp=7\n" +
			"edge-1  | a line with no component\n")
	w, err := readEdgeWork(db, log)
	if err != nil {
		t.Fatal(err)
	}
	if !w.stmtsRead || w.stmtTotal != 15 || w.stmts["SELECT orders"] != 12 {
		t.Errorf("statements: read=%v total=%d by key=%v, want read, 15, SELECT orders 12", w.stmtsRead, w.stmtTotal, w.stmts)
	}
	if w.logLines != 4 || w.logByComp["plc"] != 2 || w.logByComp["production_ticks"] != 1 || w.logByComp["other"] != 1 {
		t.Errorf("log: %d lines, by component %v; want 4, plc 2, production_ticks 1, other 1", w.logLines, w.logByComp)
	}

	// No counter file (a production build): not read, not an error.
	w, err = readEdgeWork(filepath.Join(t.TempDir(), "shingoedge.db"), nil)
	if err != nil || w.stmtsRead {
		t.Errorf("no counter file: read=%v err=%v, want not read and no error", w.stmtsRead, err)
	}
}

// taggedTSV is the sim counter's file in the tagged format. With the fixture
// DB's 40 PLC ticks (counter snapshot rows with a positive delta; the
// zero-delta row is not a tick) and 10 completed orders.
const taggedTSV = "tag\tverb\ttable\tstatements\tpath\n" +
	"loop\tSELECT\tstyle_node_claims\t40\tmessaging/plant_claims_publisher.go:(*PlantClaimsPublisher).snapshotTick\n" +
	"order\tSELECT\torders\t60\tmessaging/edge_handler.go:(*EdgeHandler).HandleOrderUpdate > orders/manager.go:(*Manager).HandleOrderUpdate\n" +
	"other\tSELECT\tstyles\t4\tcmd/shingoedge/main.go:main > service/style_service.go:(*StyleService).List\n" +
	"poll\tSELECT\treporting_points\t80\tplc/manager.go:(*Manager).pollAllReportingPoints\n" +
	"sim\tSELECT\tpayload_catalog\t16\tengine/sim_operator.go:(*simOperator).capacity\n" +
	"tick\tINSERT\tcounter_snapshots\t40\tplc/manager.go:(*Manager).pollReportingPoint\n" +
	"tick\tSELECT\tstyle_node_claims\t100\tengine/wiring_counter_delta.go:(*Engine).handleCounterDelta > engine/wiring_counter_delta.go:(*Engine).counterDeltaRows\n" +
	"tick\tSELECT\tstyle_node_claims\t60\tengine/wiring_counter_delta.go:(*Engine).handleCounterDelta > engine/wiring_counter_delta.go:(*Engine).drainLinesideFirst\n"

// writeEdgeFixture is a run dir as the rig collects it: <dir>/<svc>/
// shingoedge.db (only the tables soakstat reads) beside the counter's tsv.
func writeEdgeFixture(t *testing.T, tsv string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "edge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "shingoedge.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE process_nodes (id INTEGER PRIMARY KEY, core_node_name TEXT)`,
		`CREATE TABLE process_node_runtime_states (process_node_id INTEGER, pending_uop_delta INTEGER)`,
		`CREATE TABLE outbox (id INTEGER PRIMARY KEY, msg_type TEXT, payload BLOB)`,
		`CREATE TABLE counter_snapshots (id INTEGER PRIMARY KEY, delta INTEGER NOT NULL DEFAULT 0)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40) INSERT INTO counter_snapshots (delta) SELECT 2 FROM n`,
		`INSERT INTO counter_snapshots (delta) VALUES (0)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "edge-statements.tsv"), []byte(tsv), 0o644); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func edgeWorkOutput(t *testing.T, dbPath string, completed reading) string {
	t.Helper()
	e, err := readEdge(edgeSource{label: "edge", dbPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printEdgeWork(&b, []*edgeReading{e}, completed)
	return b.String()
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q", w)
		}
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

// [S.6] reads the Pi's work per PLC tick and as the run's total, with per
// completed order as a third column; the top statements by table each with its
// tag mix and its most frequent code path; and the split by caller tag.
func TestEdgeWork_PerTickTotalTopTen(t *testing.T) {
	out := edgeWorkOutput(t, writeEdgeFixture(t, taggedTSV), reading{n: 10})
	wantLines(t, out,
		"[S.6] EDGE WORK  per PLC tick, run total, per completed order (10)",
		"edge (edge): PLC ticks 40 (counter_snapshots rows with delta > 0)",
		"statements      10.00/tick        400 total      40.00/order",
		"by tag: tick 200 (50.0%) · order 60 (15.0%) · loop 40 (10.0%) · poll 80 (20.0%) · sim 16 (4.0%) · other 4 (1.0%)",
		"top 10 statements by table",
		" 1. SELECT style_node_claims                   5.00/tick        200 total      20.00/order  tick 80% · loop 20%",
		"engine/wiring_counter_delta.go:(*Engine).handleCounterDelta > engine/wiring_counter_delta.go:(*Engine).counterDeltaRows  (50% of these)",
		" 2. SELECT reporting_points                    2.00/tick         80 total       8.00/order  poll 100%",
		" 3. SELECT orders                              1.50/tick         60 total       6.00/order  order 100%",
		"EDGE WORK (all Edges): PLC ticks 40 · statements 10.00/tick · 400 total · 40.00/order",
	)
	ranked := regexp.MustCompile(`(?m)^\s+\d+\. (SELECT|INSERT) `).FindAllString(out, -1)
	if len(ranked) != 6 {
		t.Errorf("ranked %d statements, want the fixture's six distinct ones (fewer than ten)", len(ranked))
	}
}

// A run dir from before the tagged counter: the old "VERB table<TAB>count"
// file still reads, per tick and total, with a note that it has no tags or
// code paths and that its names are the first table in the text.
func TestEdgeWork_OldFormatDegradesWithANote(t *testing.T) {
	out := edgeWorkOutput(t, writeEdgeFixture(t, "SELECT payload_catalog\t120\nSELECT orders\t80\n"), reading{n: 10})
	wantLines(t, out,
		"statements       5.00/tick        200 total      20.00/order",
		"by tag: n/a (old two-column edge-statements.tsv: no caller tags or code paths; each statement is named by the first table in its text)",
		" 1. SELECT payload_catalog                     3.00/tick        120 total      12.00/order",
	)
}

// No completed orders read still prints per tick and the total; the per-order
// column reads n/a.
func TestEdgeWork_NoCompletedOrdersStillPerTick(t *testing.T) {
	out := edgeWorkOutput(t, writeEdgeFixture(t, taggedTSV), reading{failed: true})
	wantLines(t, out,
		"per completed order (n/a: completed orders not read)",
		"statements      10.00/tick        400 total        n/a/order",
	)
}
