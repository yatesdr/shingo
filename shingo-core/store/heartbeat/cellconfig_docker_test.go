//go:build docker

package heartbeat_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/heartbeat"
)

// TestCoverage_CellConfig exercises the cell_config CRUD + the admin process
// picker against real Postgres — the v30 DDL and the JSONB sub_process_ids
// round-trip the local build can't validate (Phase E, Q-025).
func TestCoverage_CellConfig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Empty on a fresh DB.
	cells, err := heartbeat.ListCellConfigs(db.DB)
	if err != nil {
		t.Fatalf("ListCellConfigs (empty): %v", err)
	}
	if len(cells) != 0 {
		t.Fatalf("fresh DB has %d cells, want 0", len(cells))
	}

	// Insert a composite cell.
	cfg := heartbeat.CellConfig{
		CellID: "SNF2", Station: "plant-a.line-1", DisplayName: "Assembly NF 2",
		PrimaryProcessID: 100, SubProcessIDs: []int64{200, 300},
	}
	if err := heartbeat.UpsertCellConfig(db.DB, cfg); err != nil {
		t.Fatalf("UpsertCellConfig insert: %v", err)
	}

	got, ok, err := heartbeat.GetCellConfig(db.DB, "SNF2")
	if err != nil || !ok {
		t.Fatalf("GetCellConfig: ok=%v err=%v", ok, err)
	}
	if got.PrimaryProcessID != 100 || got.DisplayName != "Assembly NF 2" || got.Station != "plant-a.line-1" {
		t.Errorf("GetCellConfig fields = %+v", got)
	}
	if len(got.SubProcessIDs) != 2 || got.SubProcessIDs[0] != 200 || got.SubProcessIDs[1] != 300 {
		t.Errorf("sub_process_ids = %v, want [200 300]", got.SubProcessIDs)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at not stamped")
	}

	// Update (same cell_id): change subs to a simple cell + new name.
	cfg.SubProcessIDs = []int64{}
	cfg.DisplayName = "Assembly NF 2 (simple)"
	if err := heartbeat.UpsertCellConfig(db.DB, cfg); err != nil {
		t.Fatalf("UpsertCellConfig update: %v", err)
	}
	got, _, _ = heartbeat.GetCellConfig(db.DB, "SNF2")
	if len(got.SubProcessIDs) != 0 {
		t.Errorf("after update sub_process_ids = %v, want empty", got.SubProcessIDs)
	}
	if got.DisplayName != "Assembly NF 2 (simple)" {
		t.Errorf("after update display_name = %q", got.DisplayName)
	}
	if cells, _ := heartbeat.ListCellConfigs(db.DB); len(cells) != 1 {
		t.Errorf("after update cell count = %d, want 1 (upsert, not insert)", len(cells))
	}

	// Process picker: insert recent ticks, then list distinct processes.
	now := time.Now().UTC()
	if err := heartbeat.EnsurePartitions(db.DB, now); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	ticks := []heartbeat.PartEvent{
		{CellID: "plant-a.line-1", ProcessID: 100, StyleID: 7, PayloadCode: "PART-A", RecordedAt: now.Add(-2 * time.Minute), EdgeSnapshotID: 1, Delta: 1, CountValue: 1},
		{CellID: "plant-a.line-1", ProcessID: 100, StyleID: 7, PayloadCode: "PART-A", RecordedAt: now.Add(-1 * time.Minute), EdgeSnapshotID: 2, Delta: 1, CountValue: 2},
		{CellID: "plant-a.line-1", ProcessID: 200, StyleID: 9, PayloadCode: "SUB-B", RecordedAt: now.Add(-90 * time.Second), EdgeSnapshotID: 3, Delta: 1, CountValue: 1},
	}
	for _, e := range ticks {
		if err := heartbeat.InsertPartEvent(db.DB, e); err != nil {
			t.Fatalf("InsertPartEvent: %v", err)
		}
	}
	procs, err := heartbeat.DistinctProcesses(db.DB, "plant-a.line-1")
	if err != nil {
		t.Fatalf("DistinctProcesses: %v", err)
	}
	if len(procs) != 2 {
		t.Fatalf("DistinctProcesses len = %d, want 2", len(procs))
	}
	// Ordered by ticks DESC → process 100 (2 ticks) first.
	if procs[0].ProcessID != 100 || procs[0].Ticks != 2 || procs[0].PayloadCode != "PART-A" {
		t.Errorf("top process = %+v, want process 100 / 2 ticks / PART-A", procs[0])
	}

	// Delete.
	if err := heartbeat.DeleteCellConfig(db.DB, "SNF2"); err != nil {
		t.Fatalf("DeleteCellConfig: %v", err)
	}
	if _, ok, _ := heartbeat.GetCellConfig(db.DB, "SNF2"); ok {
		t.Error("cell still present after delete")
	}
}
