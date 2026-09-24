//go:build docker

package heartbeat_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/footprint"
	"shingocore/store/heartbeat"
)

// TestProcessPickerAndFootprint_OverFixture (P11) pins the two readers of
// cell_part_events that are not the heartbeat math: the admin process picker
// (DistinctProcesses) and the Overview's "processes managed" count (Footprint).
// The rows go in by plain SQL naming only the columns every shape of the table
// has, so the fixture does not depend on the projection path under change.
func TestProcessPickerAndFootprint_OverFixture(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := heartbeat.EnsurePartitions(db.DB, now); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	if err := heartbeat.EnsurePartitions(db.DB, now.AddDate(0, -1, 0)); err != nil {
		t.Fatalf("EnsurePartitions (last month): %v", err)
	}
	type row struct {
		cell        string
		ago         time.Duration
		edgeID      int64
		proc, style int64
	}
	rows := []row{
		{"stn-a", 50 * time.Minute, 1, 7, 3},
		{"stn-a", 40 * time.Minute, 2, 7, 4},
		{"stn-a", 30 * time.Minute, 3, 7, 4},
		{"stn-a", 20 * time.Minute, 4, 9, 5},
		{"stn-b", 10 * time.Minute, 1, 7, 3}, // same process id, other station
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO cell_part_events (cell_id, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id)
			VALUES ($1, $2, $3, 0, 1, '', $4, $5)`, r.cell, now.Add(-r.ago), r.edgeID, r.proc, r.style); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	got, err := heartbeat.DistinctProcesses(db.DB, "stn-a")
	if err != nil {
		t.Fatalf("DistinctProcesses: %v", err)
	}
	type want struct {
		pid, ticks, style int64
		last              time.Time
	}
	wants := []want{
		{7, 3, 4, now.Add(-30 * time.Minute)},
		{9, 1, 5, now.Add(-20 * time.Minute)},
	}
	if len(got) != len(wants) {
		t.Fatalf("DistinctProcesses = %+v, want %d options", got, len(wants))
	}
	for i, w := range wants {
		g := got[i]
		if g.ProcessID != w.pid || g.Ticks != w.ticks || g.StyleID != w.style || !g.LastSeen.Equal(w.last) {
			t.Errorf("option %d = %+v, want pid=%d ticks=%d style=%d last=%v", i, g, w.pid, w.ticks, w.style, w.last)
		}
		if g.PayloadCode != "" {
			t.Errorf("option %d payload_code = %q, want '' (never written)", i, g.PayloadCode)
		}
	}

	fp, err := footprint.Get(db.DB, time.UTC, "", nil)
	if err != nil {
		t.Fatalf("footprint.Get: %v", err)
	}
	if fp.ProcessesManaged != 3 {
		t.Errorf("ProcessesManaged = %d, want 3 distinct (cell, process) pairs", fp.ProcessesManaged)
	}
}
