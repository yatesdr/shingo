package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"shingocore/plantspec"
)

// seed_edge_counter_test.go — the process has to SAY it counts.
//
// `processes` carries counter_plc_name / counter_tag_name / counter_enabled, and
// the seeder used to leave all three at their defaults while inserting the
// reporting_points row that does the actual polling. The demo therefore counted
// while every process on it declared no counter, and binDrainedAtCoreNode — which
// gates on the PROCESS columns, not the reporting point — answered DrainUnknown
// plant-wide.
//
// The tie from a reporting point to a process runs through the style:
// plantspec.ReportingPoint.Style names a style, and plantspec.Style.Process
// names the process — seedEdgeDB reads the second into its styleProc map.
//
// SYMBOLS, NOT LINES. This comment cited both of those by line number when it
// was written. One rebase later ReportingPoint had moved ten lines and the
// citation landed on Demand's doc instead — AGENTS.md's "cite a SYMBOL for
// anything you expect to survive" earning itself inside the very branch that
// exists to delete comments that stopped being true.

// loadFixture returns a fresh copy of the seed fixture for a test that mutates it.
func loadFixture(t *testing.T) *plantspec.Plant {
	t.Helper()
	plant, err := plantspec.Load("testdata/seed-fixture.yaml")
	if err != nil {
		t.Fatalf("load seed fixture: %v", err)
	}
	if err := plant.Validate(); err != nil {
		t.Fatalf("validate fixture: %v", err)
	}
	return plant
}

// freshEdgeDB opens an empty edge DB with the hand-mirrored DDL.
func freshEdgeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "edge.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open edge sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(edgeDDL); err != nil {
		t.Fatalf("apply edge DDL: %v", err)
	}
	return db
}

// TestSeedEdge_ProcessDeclaresItsCounter: after a seed, a process that owns a
// reporting point says so in its own three columns, and one that owns none is
// left alone.
func TestSeedEdge_ProcessDeclaresItsCounter(t *testing.T) {
	db, _, plant := openSeededEdge(t)

	// What the yaml asked for: process name → the counter its style's point names.
	styleProcess := map[string]string{}
	for _, s := range plant.Styles {
		styleProcess[s.Name] = s.Process
	}
	want := map[string][2]string{}
	for _, rp := range plant.ReportingPoints {
		proc := styleProcess[rp.Style]
		if proc == "" {
			t.Fatalf("fixture: reporting point %s/%s has style %q, which names no process",
				rp.PLCName, rp.TagName, rp.Style)
		}
		want[proc] = [2]string{rp.PLCName, rp.TagName}
	}
	if len(want) == 0 {
		t.Fatal("fixture has no reporting points; this test would assert nothing")
	}

	rows, err := db.Query(`SELECT name, counter_plc_name, counter_tag_name, counter_enabled FROM processes`)
	if err != nil {
		t.Fatalf("query processes: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name, plc, tag string
		var enabled int
		if err := rows.Scan(&name, &plc, &tag, &enabled); err != nil {
			t.Fatalf("scan: %v", err)
		}
		w, owns := want[name]
		if !owns {
			if plc != "" || tag != "" || enabled != 0 {
				t.Errorf("process %s owns no reporting point but declares a counter "+
					"(%q/%q enabled=%d) — the seeder must not invent one", name, plc, tag, enabled)
			}
			continue
		}
		seen[name] = true
		if plc != w[0] || tag != w[1] || enabled != 1 {
			t.Errorf("process %s: counter = %q/%q enabled=%d, want %q/%q enabled=1. The "+
				"reporting point polls this counter, so binDrainedAtCoreNode — which reads "+
				"these columns and not the point — answers DrainUnknown until they agree.",
				name, plc, tag, enabled, w[0], w[1])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for proc := range want {
		if !seen[proc] {
			t.Errorf("process %s owns a reporting point but no row came back for it", proc)
		}
	}
}

// TestSeedEdge_TwoCountersOnOneProcess_FailsByName: one process, one counter.
// Two different ones is a plant the seeder cannot honour in three columns, and
// picking either silently would make the process declare a counter it does not
// have.
func TestSeedEdge_TwoCountersOnOneProcess_FailsByName(t *testing.T) {
	plant := loadFixture(t)
	// A second, DIFFERENT counter reaching PRESS-1 — here through the same style,
	// which is the same seeder path a second style of the same process takes.
	plant.ReportingPoints = append(plant.ReportingPoints, plantspec.ReportingPoint{
		PLCName: "PRESS-1-B", TagName: "PRESS-1B_COUNTER", Node: "PLN_001", Style: "PRESS-1-RUN",
	})

	err := seedEdgeInTx(t, freshEdgeDB(t), plant)
	if err == nil {
		t.Fatal("the seed accepted two different counters on PRESS-1 and picked one silently; " +
			"it must refuse and name the process")
	}
	if !strings.Contains(err.Error(), "PRESS-1") {
		t.Errorf("seed error = %q; it must name the process so the yaml line is findable", err)
	}
	for _, want := range []string{"PRESS-1_COUNTER", "PRESS-1B_COUNTER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("seed error = %q; it must name both counters (%s missing)", err, want)
		}
	}
}
