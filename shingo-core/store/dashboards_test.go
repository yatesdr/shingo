//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/dashboards"
)

// TestDashboardCRUD exercises the dashboard platform's persistence + service
// normalization end-to-end: create (with trimming, station de-dup, and
// defaults), get, list, update, validation rejections, and delete.
func TestDashboardCRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := service.NewDashboardService(db)

	// Create: name is trimmed, kind defaults to task-board, stations are
	// trimmed + de-duped (order preserved) + empties dropped, config -> "{}".
	id, err := svc.Create(dashboards.Input{
		Name:     "  North Cell  ",
		Stations: []string{"ALN_001", "ALN_002", "ALN_001", "  "},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("create returned id 0")
	}

	got, err := svc.Get(id)
	if err != nil || got == nil {
		t.Fatalf("get: %v (nil=%v)", err, got == nil)
	}
	if got.Name != "North Cell" {
		t.Errorf("name = %q, want trimmed %q", got.Name, "North Cell")
	}
	if got.Kind != "task-board" {
		t.Errorf("kind = %q, want default task-board", got.Kind)
	}
	if len(got.Stations) != 2 || got.Stations[0] != "ALN_001" || got.Stations[1] != "ALN_002" {
		t.Errorf("stations = %v, want [ALN_001 ALN_002] (de-duped, empties dropped)", got.Stations)
	}
	if string(got.Config) != "{}" {
		t.Errorf("config = %q, want default {}", string(got.Config))
	}

	// List contains the created row.
	list, err := svc.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, d := range list {
		if d.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("list did not contain id %d", id)
	}

	// Update: clearing stations means plant-wide; enabled flips on.
	if err := svc.Update(id, dashboards.Input{
		Name: "North", Kind: "task-board", Stations: nil, Enabled: true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := svc.Get(id)
	if got2.Name != "North" {
		t.Errorf("post-update name = %q", got2.Name)
	}
	if len(got2.Stations) != 0 {
		t.Errorf("post-update stations = %v, want empty (plant-wide)", got2.Stations)
	}
	if !got2.Enabled {
		t.Errorf("post-update enabled = false, want true")
	}

	// Validation: empty name and invalid config JSON are rejected at write.
	if _, err := svc.Create(dashboards.Input{Name: "   "}); err == nil {
		t.Error("create with blank name should error")
	}
	if _, err := svc.Create(dashboards.Input{Name: "Bad", Config: []byte("not json")}); err == nil {
		t.Error("create with invalid config JSON should error")
	}

	// Delete removes it.
	if err := svc.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	gone, err := svc.Get(id)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if gone != nil {
		t.Error("dashboard still present after delete")
	}
}
