package main

import (
	"os"
	"path/filepath"
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
