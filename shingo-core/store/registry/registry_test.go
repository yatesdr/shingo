//go:build docker

package registry_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/registry"
)

func TestCoverage_RegisterEdge_InsertAndUpdate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if err := registry.Register(db.DB, "line-1", "host-a", "v1.0.0", []string{"L1", "L2"}); err != nil { t.Fatalf("Register initial: %v", err) }
	edges, err := registry.List(db.DB)
	if err != nil { t.Fatalf("List initial: %v", err) }
	if len(edges) != 1 { t.Fatalf("initial len = %d, want 1", len(edges)) }
	if edges[0].StationID != "line-1" { t.Errorf("station = %q, want line-1", edges[0].StationID) }
	if edges[0].Hostname != "host-a" { t.Errorf("hostname = %q, want host-a", edges[0].Hostname) }
	if edges[0].Version != "v1.0.0" { t.Errorf("version = %q, want v1.0.0", edges[0].Version) }
	if len(edges[0].LineIDs) != 2 || edges[0].LineIDs[0] != "L1" || edges[0].LineIDs[1] != "L2" { t.Errorf("line_ids = %+v, want [L1 L2]", edges[0].LineIDs) }
	if edges[0].Status != "active" { t.Errorf("status = %q, want active", edges[0].Status) }
	if err := registry.Register(db.DB, "line-1", "host-b", "v2.0.0", []string{"L9"}); err != nil { t.Fatalf("Register update: %v", err) }
	edges2, _ := registry.List(db.DB)
	if len(edges2) != 1 { t.Fatalf("after update len = %d, want 1 (upsert)", len(edges2)) }
	if edges2[0].Hostname != "host-b" { t.Errorf("hostname after update = %q, want host-b", edges2[0].Hostname) }
	if edges2[0].Version != "v2.0.0" { t.Errorf("version after update = %q, want v2.0.0", edges2[0].Version) }
	if len(edges2[0].LineIDs) != 1 || edges2[0].LineIDs[0] != "L9" { t.Errorf("line_ids after update = %+v, want [L9]", edges2[0].LineIDs) }
}

func TestCoverage_UpdateHeartbeat_IsNewThenNot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	isNew1, err := registry.UpdateHeartbeat(db.DB, "line-fresh")
	if err != nil { t.Fatalf("UpdateHeartbeat first: %v", err) }
	if !isNew1 { t.Error("first heartbeat for unknown station should report isNew=true") }
	edges, _ := registry.List(db.DB)
	if len(edges) != 1 { t.Fatalf("after first heartbeat len = %d, want 1", len(edges)) }
	if edges[0].LastHeartbeat == nil { t.Error("last_heartbeat should be set") }
	firstBeat := edges[0].LastHeartbeat
	// KEEP: timestamp separation — second heartbeat must record a later timestamp.
	time.Sleep(10 * time.Millisecond)
	isNew2, err := registry.UpdateHeartbeat(db.DB, "line-fresh")
	if err != nil { t.Fatalf("UpdateHeartbeat second: %v", err) }
	if isNew2 { t.Error("second heartbeat should report isNew=false") }
	edges2, _ := registry.List(db.DB)
	if len(edges2) != 1 { t.Errorf("after second heartbeat len = %d, want 1", len(edges2)) }
	if edges2[0].LastHeartbeat != nil && firstBeat != nil && !edges2[0].LastHeartbeat.After(*firstBeat) { t.Errorf("second heartbeat should be newer") }
}

func TestCoverage_MarkStaleEdges(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	registry.Register(db.DB, "line-stale-1", "h1", "v", nil)
	registry.UpdateHeartbeat(db.DB, "line-stale-1")
	registry.Register(db.DB, "line-stale-2", "h2", "v", nil)
	registry.UpdateHeartbeat(db.DB, "line-stale-2")
	// KEEP: timestamp separation — stale-marking threshold needs distinct timestamps.
	time.Sleep(5 * time.Millisecond)
	stale, err := registry.MarkStale(db.DB, 1*time.Nanosecond)
	if err != nil { t.Fatalf("MarkStale: %v", err) }
	if len(stale) != 2 { t.Fatalf("stale ids len = %d, want 2", len(stale)) }
	seen := map[string]bool{}
	for _, s := range stale { seen[s] = true }
	if !seen["line-stale-1"] || !seen["line-stale-2"] { t.Errorf("stale ids = %+v, want both", stale) }
	edges, _ := registry.List(db.DB)
	for _, e := range edges { if e.Status != "stale" { t.Errorf("edge %s status = %q, want stale", e.StationID, e.Status) } }
	again, err := registry.MarkStale(db.DB, 1*time.Nanosecond)
	if err != nil { t.Fatalf("MarkStale second: %v", err) }
	if len(again) != 0 { t.Errorf("second MarkStale len = %d, want 0", len(again)) }
}
