package store

import (
	"path/filepath"
	"testing"

	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// loader_window_migration_test.go — v56: process_nodes.loader_window.

const loaderWindowProbe = `SELECT COUNT(*) FROM pragma_table_info('process_nodes') WHERE name = 'loader_window'`

// An Edge before v56 gets the column at its next boot, 0 on every row it
// holds (the engine marks the held ones at its start, not the migration),
// and the row reads as it did; a second boot changes nothing.
func TestLoaderWindowMigration_AddsTheColumnUnmarked(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE process_nodes DROP COLUMN loader_window`); err != nil {
		t.Fatalf("plant the pre-v56 shape: %v", err)
	}
	for _, m := range edgeMigrations() {
		if m.Name == "process_nodes_loader_window" {
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, m.Version); err != nil {
				t.Fatalf("unapply v%d: %v", m.Version, err)
			}
		}
	}
	pid, err := db.CreateProcess("LW-MIG", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "LW-MIG-SCREEN", Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	nid, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, OperatorStationID: &sid,
		CoreNodeName: "LW-W1", Code: "LW-W1", Name: "LW-W1", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if probeRaw(t, path, loaderWindowProbe) != 0 {
		t.Fatal("the planted database still has loader_window")
	}

	first := bootAndDump(t, path)
	if probeRaw(t, path, loaderWindowProbe) != 1 {
		t.Fatal("process_nodes has no loader_window after the upgrade")
	}
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("the schema after boot 2 differs from boot 1.\n%s", firstDumpDiff(first, second))
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	n, err := db.GetProcessNode(nid)
	if err != nil {
		t.Fatalf("read the node: %v", err)
	}
	if n.LoaderWindow || n.CoreNodeName != "LW-W1" || n.OperatorStationID == nil || *n.OperatorStationID != sid || !n.Enabled {
		t.Errorf("after the upgrade the node reads %+v; want LW-W1 on screen %d, enabled, not marked", *n, sid)
	}
}

// A fresh database has the column from its baseline, and the mark is set by
// MarkProcessNodesLoaderWindow only: on the rows named, never cleared by it.
func TestLoaderWindowMigration_FreshHasItAndTheMarkOnlySets(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	if probeRaw(t, path, loaderWindowProbe) != 1 {
		t.Fatal("a fresh process_nodes has no loader_window")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	pid, err := db.CreateProcess("LW-FRESH", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	var ids []int64
	for i, name := range []string{"LW-A", "LW-B"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: name, Code: name, Name: name, Sequence: i + 1, Enabled: true})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		ids = append(ids, id)
	}
	if err := db.MarkProcessNodesLoaderWindow(ids[:1]); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if err := db.MarkProcessNodesLoaderWindow(nil); err != nil {
		t.Fatalf("mark none: %v", err)
	}
	nodes, err := db.ListProcessNodesByProcess(pid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.CoreNodeName] = n.LoaderWindow
	}
	if !got["LW-A"] || got["LW-B"] {
		t.Errorf("marks = %v, want LW-A marked and LW-B not", got)
	}
}
