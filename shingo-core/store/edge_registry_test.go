//go:build docker

package store

import (
	"testing"
	"time"
)

func TestRegisterEdge_InsertAndUpdate(t *testing.T) {
	db := testDB(t)

	if err := db.RegisterEdge("line-1", "host-a", "v1.0.0", []string{"L1", "L2"}); err != nil {
		t.Fatalf("RegisterEdge initial: %v", err)
	}

	edges, err := db.ListEdges()
	if err != nil {
		t.Fatalf("ListEdges initial: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("initial len = %d, want 1", len(edges))
	}
	if edges[0].StationID != "line-1" {
		t.Errorf("station = %q, want line-1", edges[0].StationID)
	}
	if edges[0].Hostname != "host-a" {
		t.Errorf("hostname = %q, want host-a", edges[0].Hostname)
	}
	if edges[0].Version != "v1.0.0" {
		t.Errorf("version = %q, want v1.0.0", edges[0].Version)
	}
	if len(edges[0].LineIDs) != 2 || edges[0].LineIDs[0] != "L1" || edges[0].LineIDs[1] != "L2" {
		t.Errorf("line_ids = %+v, want [L1 L2]", edges[0].LineIDs)
	}
	if edges[0].Status != "active" {
		t.Errorf("status = %q, want active", edges[0].Status)
	}

	// Re-register with different values — upsert path
	if err := db.RegisterEdge("line-1", "host-b", "v2.0.0", []string{"L9"}); err != nil {
		t.Fatalf("RegisterEdge update: %v", err)
	}
	edges2, _ := db.ListEdges()
	if len(edges2) != 1 {
		t.Fatalf("after update len = %d, want 1 (upsert, not duplicate)", len(edges2))
	}
	if edges2[0].Hostname != "host-b" {
		t.Errorf("hostname after update = %q, want host-b", edges2[0].Hostname)
	}
	if edges2[0].Version != "v2.0.0" {
		t.Errorf("version after update = %q, want v2.0.0", edges2[0].Version)
	}
	if len(edges2[0].LineIDs) != 1 || edges2[0].LineIDs[0] != "L9" {
		t.Errorf("line_ids after update = %+v, want [L9]", edges2[0].LineIDs)
	}
}

func TestUpdateHeartbeat_IsNewThenNot(t *testing.T) {
	db := testDB(t)

	// First heartbeat for a never-seen station => isNew = true
	isNew1, err := db.UpdateHeartbeat("line-fresh")
	if err != nil {
		t.Fatalf("UpdateHeartbeat first: %v", err)
	}
	if !isNew1 {
		t.Error("first heartbeat for unknown station should report isNew=true")
	}

	// Verify a row exists now
	edges, _ := db.ListEdges()
	if len(edges) != 1 {
		t.Fatalf("after first heartbeat len = %d, want 1", len(edges))
	}
	if edges[0].LastHeartbeat == nil {
		t.Error("last_heartbeat should be set after UpdateHeartbeat")
	}
	firstBeat := edges[0].LastHeartbeat

	// Sleep a small amount to get a detectable timestamp bump
	time.Sleep(10 * time.Millisecond)

	// Second heartbeat => isNew = false (row already exists)
	isNew2, err := db.UpdateHeartbeat("line-fresh")
	if err != nil {
		t.Fatalf("UpdateHeartbeat second: %v", err)
	}
	if isNew2 {
		t.Error("second heartbeat should report isNew=false")
	}
	edges2, _ := db.ListEdges()
	if len(edges2) != 1 {
		t.Errorf("after second heartbeat len = %d, want 1 (upsert)", len(edges2))
	}
	if edges2[0].LastHeartbeat != nil && firstBeat != nil &&
		!edges2[0].LastHeartbeat.After(*firstBeat) {
		t.Errorf("second heartbeat timestamp should be newer than first (first=%v, second=%v)",
			firstBeat, edges2[0].LastHeartbeat)
	}
}

func TestMarkStaleEdges(t *testing.T) {
	db := testDB(t)

	db.RegisterEdge("line-stale-1", "h1", "v", nil)
	db.UpdateHeartbeat("line-stale-1")
	db.RegisterEdge("line-stale-2", "h2", "v", nil)
	db.UpdateHeartbeat("line-stale-2")

	// A very small threshold (1 nanosecond) makes every existing edge stale
	time.Sleep(5 * time.Millisecond)
	stale, err := db.MarkStaleEdges(1 * time.Nanosecond)
	if err != nil {
		t.Fatalf("MarkStaleEdges: %v", err)
	}
	if len(stale) != 2 {
		t.Fatalf("stale ids len = %d, want 2", len(stale))
	}
	seen := map[string]bool{}
	for _, s := range stale {
		seen[s] = true
	}
	if !seen["line-stale-1"] || !seen["line-stale-2"] {
		t.Errorf("stale ids = %+v, want both", stale)
	}

	// Verify each edge's status is now "stale"
	edges, _ := db.ListEdges()
	for _, e := range edges {
		if e.Status != "stale" {
			t.Errorf("edge %s status = %q, want stale", e.StationID, e.Status)
		}
	}

	// Running MarkStaleEdges again: no active edges left => empty result
	again, err := db.MarkStaleEdges(1 * time.Nanosecond)
	if err != nil {
		t.Fatalf("MarkStaleEdges second call: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second MarkStaleEdges len = %d, want 0 (already stale)", len(again))
	}
}
