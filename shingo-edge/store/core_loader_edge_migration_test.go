package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/store/stations"
)

// core_loader_edge_migration_test.go — v57: core_loaders.edge_station_uid and
// edge_display_name, operator_stations.core_loader_key.

const v57Probe = `SELECT
	(SELECT COUNT(*) FROM pragma_table_info('core_loaders') WHERE name IN ('edge_station_uid', 'edge_display_name')) +
	(SELECT COUNT(*) FROM pragma_table_info('operator_stations') WHERE name = 'core_loader_key')`

// An Edge before v57 gets the three columns at its next boot, blank on every
// row it holds: no loader names an Edge until Core's next send says so, and no
// screen names a loader until the Edge acts for one. A second boot changes
// nothing.
func TestCoreLoaderEdgeMigration_AddsTheColumnsBlank(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pid, err := db.CreateProcess("V57-MIG", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "V57-SCREEN", Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{Name: "V57 Loader", LoaderKey: "loader:V57", Role: "produce",
		Layout: "shared_window", Replenishment: "operator", Positions: []protocol.LoaderPosition{{CoreNodeName: "V57-W1"}}}}); err != nil {
		t.Fatalf("cache a loader: %v", err)
	}
	for _, drop := range []string{
		`ALTER TABLE core_loaders DROP COLUMN edge_station_uid`,
		`ALTER TABLE core_loaders DROP COLUMN edge_display_name`,
		`ALTER TABLE operator_stations DROP COLUMN core_loader_key`,
	} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("plant the pre-v57 shape: %v", err)
		}
	}
	for _, m := range edgeMigrations() {
		if m.Name == "core_loader_edge_and_screen_loader" {
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, m.Version); err != nil {
				t.Fatalf("unapply v%d: %v", m.Version, err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if probeRaw(t, path, v57Probe) != 0 {
		t.Fatal("the planted database still has a v57 column")
	}

	first := bootAndDump(t, path)
	if probeRaw(t, path, v57Probe) != 3 {
		t.Fatal("the upgrade did not add all three v57 columns")
	}
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("the schema after boot 2 differs from boot 1.\n%s", firstDumpDiff(first, second))
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	l, err := db.GetCoreLoader("loader:V57")
	if err != nil || l == nil {
		t.Fatalf("read the loader: %v, %v", l, err)
	}
	if l.EdgeStationUID != "" || l.EdgeDisplayName != "" || l.Name != "V57 Loader" {
		t.Errorf("after the upgrade the loader reads %+v; want it as it was, naming no Edge", *l)
	}
	st, err := db.GetOperatorStation(sid)
	if err != nil {
		t.Fatalf("read the screen: %v", err)
	}
	if st.CoreLoaderKey != "" || st.Name != "V57-SCREEN" {
		t.Errorf("after the upgrade the screen reads %+v; want it as it was, naming no loader", *st)
	}
}

// A fresh database has the columns from its baseline; the cache keeps what
// Core sends, a screen made for a loader names it, and a screen's edit keeps
// that name.
func TestCoreLoaderEdgeMigration_FreshHasThemAndTheyHold(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	if second := bootAndDump(t, path); first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	if probeRaw(t, path, v57Probe) != 3 {
		t.Fatal("a fresh database lacks a v57 column")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{{Name: "V57 Loader", LoaderKey: "loader:V57", Role: "produce",
		Layout: "shared_window", Replenishment: "operator", EdgeStationUID: "stn-a", EdgeDisplayName: "EDGE A",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "V57-W1"}}}}); err != nil {
		t.Fatalf("cache a loader: %v", err)
	}
	l, err := db.GetCoreLoader("loader:V57")
	if err != nil || l == nil || l.EdgeStationUID != "stn-a" || l.EdgeDisplayName != "EDGE A" {
		t.Fatalf("cached loader = %+v, %v; want it naming stn-a, EDGE A", l, err)
	}
	pid, err := db.CreateProcess("V57-FRESH", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "V57-MADE", Enabled: true, CoreLoaderKey: "loader:V57"})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	if err := db.UpdateOperatorStation(sid, stations.Input{ProcessID: pid, Name: "V57-RENAMED", Enabled: true}); err != nil {
		t.Fatalf("edit the screen: %v", err)
	}
	st, err := db.GetOperatorStation(sid)
	if err != nil {
		t.Fatalf("read the screen: %v", err)
	}
	if st.CoreLoaderKey != "loader:V57" || st.Name != "V57-RENAMED" {
		t.Errorf("screen = %+v; want it renamed and still naming loader:V57", *st)
	}
}
