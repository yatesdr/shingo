//go:build docker

package service

import (
	"testing"

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
	db := testdb.Open(t)
	svc := NewStationService(db)

	pid, _ := db.CreateProcess("P", "", "", "", "", false)
	id, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	// Initial set.
	if err := svc.SetNodes(id, []string{"N1", "N2"}); err != nil {
		t.Fatalf("set 1: %v", err)
	}
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
	if err := svc.SetNodes(id, []string{"N2", "N3"}); err != nil {
		t.Fatalf("set 2: %v", err)
	}
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
	if err := svc.SetNodes(id, []string{" N2 ", "N2", "", "N4"}); err != nil {
		t.Fatalf("set 3: %v", err)
	}
	nodes3, _ := db.ListProcessNodesByStation(id)
	if len(nodes3) != 2 { // N2, N4 — N3 removed
		t.Errorf("nodes after dedup = %d, want 2", len(nodes3))
	}
}

func TestStation_SetNodesDisablesRatherThanDeletesWhenOrdersActive(t *testing.T) {
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
	_, err := db.CreateOrder("keep-me", "retrieve", &nodeID, false, 1, "", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Drop N-KEEP. Node should be disabled, not deleted.
	if err := svc.SetNodes(id, []string{"N-NEW"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	n, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node: %v (should still exist)", err)
	}
	if n.Enabled {
		t.Error("expected disabled=true for node with active orders")
	}
}
