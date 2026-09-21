package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/stations"
)

// TestStation_SetNodes covers the four-branch contract of
// StationService.SetNodes: create-on-first-set, update-existing,
// dedupe+trim input, and disable-vs-delete based on active orders.
//
// Phase 6.4a moved the orchestration body in from
// store/station_nodes.go's deleted (db *DB).SetStationNodes; this
// test was previously TestOperatorStations_SetStationNodes in
// store/store_coverage_test.go and exercised the *store.DB shim
// directly. Now it exercises the service.

func TestStation_SetNodes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	id, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	// Initial set.
	testutil.MustNoErr(t, svc.SetNodes(id, []string{"N1", "N2"}), "set 1")
	names, err := svc.GetNodeNames(id)
	if err != nil {
		t.Fatalf("get names: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names len = %d", len(names))
	}

	// Verify runtime rows got created for each node.
	nodes, _ := db.ListProcessNodesByStation(id)
	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d", len(nodes))
	}
	for _, n := range nodes {
		rt, err := db.GetProcessNodeRuntime(n.ID)
		if err != nil {
			t.Errorf("runtime missing for node %d: %v", n.ID, err)
		}
		if rt == nil {
			t.Errorf("runtime nil for node %d", n.ID)
		}
	}

	// Update: remove N1, add N3. N1 has no active orders, so it's deleted.
	testutil.MustNoErr(t, svc.SetNodes(id, []string{"N2", "N3"}), "set 2")
	nodes2, _ := db.ListProcessNodesByStation(id)
	if len(nodes2) != 2 {
		t.Fatalf("nodes len after update = %d", len(nodes2))
	}
	nameSet := map[string]bool{}
	for _, n := range nodes2 {
		nameSet[n.CoreNodeName] = true
	}
	if !nameSet["N2"] || !nameSet["N3"] || nameSet["N1"] {
		t.Errorf("after update: %v", nameSet)
	}

	// Input with duplicates + whitespace — dedupe + trim paths.
	testutil.MustNoErr(t, svc.SetNodes(id, []string{" N2 ", "N2", "", "N4"}), "set 3")
	nodes3, _ := db.ListProcessNodesByStation(id)
	if len(nodes3) != 2 { // N2, N4 — N3 removed
		t.Errorf("nodes after dedup = %d, want 2", len(nodes3))
	}
}

func TestStation_SetNodesDisablesRatherThanDeletesWhenOrdersActive(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	id, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	svc.SetNodes(id, []string{"N-KEEP"})
	nodes, _ := db.ListProcessNodesByStation(id)
	var nodeID int64
	for _, n := range nodes {
		if n.CoreNodeName == "N-KEEP" {
			nodeID = n.ID
		}
	}
	// Attach an active order to the node we're about to drop.
	_, err := db.CreateOrder("keep-me", "retrieve", &nodeID, false, 1, "", "", "", "", false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Drop N-KEEP. Node should be disabled, not deleted.
	testutil.MustNoErr(t, svc.SetNodes(id, []string{"N-NEW"}), "set")
	n, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node: %v (should still exist)", err)
	}
	if n.Enabled {
		t.Error("expected disabled=true for node with active orders")
	}
}

// TestStation_SetNodes_AdoptsOrphanInsteadOfDuplicating pins the fix for the
// duplicate-process_nodes bug (HK 2026-07-14: PLN_01 → three rows).
//
// SetNodes used to decide "reuse or create?" from the STATION-local node set, so
// a Core node that existed under the process but was not currently on THIS
// station was invisible to it and got minted afresh. The node's runtime state
// (and its share of every PLC tick) then split across the copies. Reuse must be
// decided process-globally: the existing row is adopted, never re-created.
func TestStation_SetNodes_AdoptsOrphanInsteadOfDuplicating(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P400", "", "", "", "", false)
	stationA, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "A"})

	// Station A claims PLN_01, then drops it. The row survives, orphaned — this is
	// the state that used to be invisible to the next rebind.
	testutil.MustNoErr(t, svc.SetNodes(stationA, []string{"PLN_01"}), "claim PLN_01")
	nodes, _ := db.ListProcessNodesByStation(stationA)
	if len(nodes) != 1 {
		t.Fatalf("setup: nodes = %d, want 1", len(nodes))
	}
	origID := nodes[0].ID

	// Keep the node alive through the un-claim by giving it an active order, so it
	// is disabled rather than deleted — the real-world orphan shape.
	if _, err := db.CreateOrder("uuid-adopt", "complex", &origID, false, 1, "PLN_01", "", "", "", false, "", "", ""); err != nil {
		t.Fatalf("seed active order: %v", err)
	}
	testutil.MustNoErr(t, svc.SetNodes(stationA, []string{}), "un-claim PLN_01")

	// A second station now claims the same Core node. Pre-fix this minted a
	// duplicate row (code pln-01-2) with its own runtime; post-fix it adopts.
	stationB, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "B"})
	testutil.MustNoErr(t, svc.SetNodes(stationB, []string{"PLN_01"}), "station B claims PLN_01")

	all, err := db.ListProcessNodesByProcess(pid)
	if err != nil {
		t.Fatalf("list by process: %v", err)
	}
	var pln int
	for _, n := range all {
		if n.CoreNodeName == "PLN_01" {
			pln++
		}
	}
	if pln != 1 {
		t.Fatalf("PLN_01 process_nodes rows = %d, want 1 — the orphan was duplicated instead of adopted", pln)
	}

	// And it must be the SAME row, re-pointed — not a fresh id carrying no history.
	bNodes, _ := db.ListProcessNodesByStation(stationB)
	if len(bNodes) != 1 {
		t.Fatalf("station B nodes = %d, want 1", len(bNodes))
	}
	if bNodes[0].ID != origID {
		t.Errorf("station B node id = %d, want %d (the existing row must be adopted, not recreated)", bNodes[0].ID, origID)
	}
	if !bNodes[0].Enabled {
		t.Errorf("adopted node should be re-enabled")
	}
}

// TestStation_SetNodes_LeavesRoutingRowsUntouched pins the boundary the
// routing set was put in its own table for. SetNodes re-points, re-sequences,
// disables and deletes process_nodes rows with no role concept; a routing
// row that lived on process_nodes would die on an unrelated board edit. So
// the routing set must survive every SetNodes pass byte-for-byte — including
// the pass that adds one of ITS names as a station position, which is a
// cleanup the engineer makes, never a side effect.
func TestStation_SetNodes_LeavesRoutingRowsUntouched(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, err := db.CreateProcess("P", "", "", "", "", false)
	testutil.MustNoErr(t, err, "db.CreateProcess")
	stID, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	testutil.MustNoErr(t, err, "db.CreateOperatorStation")
	for _, in := range []domain.RoutingNodeInput{
		{ProcessID: pid, CoreNodeName: "SMN_BUF_100", Role: domain.RoutingRoleSource, Sequence: 1, Enabled: true, Origin: domain.RoutingOriginEngineer, CalledBy: "eng"},
		{ProcessID: pid, CoreNodeName: "STG_01", Role: domain.RoutingRoleStaging, Sequence: 2, Enabled: false, Origin: domain.RoutingOriginBackfill},
		{ProcessID: pid, CoreNodeName: "Supermarket Area", Role: domain.RoutingRoleDestination, Sequence: 3, Enabled: true, Origin: domain.RoutingOriginEngineer, CalledBy: "eng"},
	} {
		if _, err := db.UpsertRoutingNode(in); err != nil {
			t.Fatalf("seed routing row %s: %v", in.CoreNodeName, err)
		}
	}
	before, err := db.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("list routing before: %v", err)
	}

	testutil.MustNoErr(t, svc.SetNodes(stID, []string{"N1", "N2"}), "set 1")
	testutil.MustNoErr(t, svc.SetNodes(stID, []string{"N2"}), "set 2 (deletes N1)")
	// A routing name becoming a station position: SetNodes still leaves the
	// routing row alone.
	testutil.MustNoErr(t, svc.SetNodes(stID, []string{"N2", "STG_01"}), "set 3 (adds a routing name)")
	testutil.MustNoErr(t, svc.SetNodes(stID, nil), "set 4 (clears the station)")

	after, err := db.ListRoutingNodes(pid)
	if err != nil {
		t.Fatalf("list routing after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("routing rows: %d before SetNodes, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("routing row %d changed under SetNodes:\n before %+v\n after  %+v", i, before[i], after[i])
		}
	}
}
