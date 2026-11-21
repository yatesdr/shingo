//go:build docker

package heartbeat_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/heartbeat"
)

// TestDistinctProcesses_RanksAStationsTickingProcesses pins the Processes
// GET /api/cells/processes lists for a station: each that ticked in the last
// 30 days, once, with its tick count and style, most ticks first, and none of
// another station's. (Until v189 this was the picker half of the cell_config
// coverage test, whose CRUD half went with the table.)
func TestDistinctProcesses_RanksAStationsTickingProcesses(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()
	if err := heartbeat.EnsurePartitions(db.DB, now); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	ticks := []heartbeat.PartEvent{
		{StationUID: "stn-dp-a", ProcessID: 100, StyleID: 7, RecordedAt: now.Add(-2 * time.Minute), EdgeSnapshotID: 1, Delta: 1, CountValue: 1},
		{StationUID: "stn-dp-a", ProcessID: 100, StyleID: 7, RecordedAt: now.Add(-1 * time.Minute), EdgeSnapshotID: 2, Delta: 1, CountValue: 2},
		{StationUID: "stn-dp-a", ProcessID: 200, StyleID: 9, RecordedAt: now.Add(-90 * time.Second), EdgeSnapshotID: 3, Delta: 1, CountValue: 1},
		{StationUID: "stn-dp-b", ProcessID: 300, StyleID: 4, RecordedAt: now.Add(-1 * time.Minute), EdgeSnapshotID: 4, Delta: 1, CountValue: 1},
	}
	if _, rej := heartbeat.InsertPartEvents(db.DB, ticks); rej != nil {
		t.Fatalf("InsertPartEvents rejected %v", rej)
	}
	procs, err := heartbeat.DistinctProcesses(db.DB, "stn-dp-a")
	if err != nil {
		t.Fatalf("DistinctProcesses: %v", err)
	}
	if len(procs) != 2 {
		t.Fatalf("DistinctProcesses = %+v, want the station's 2 processes", procs)
	}
	if procs[0].ProcessID != 100 || procs[0].Ticks != 2 || procs[0].StyleID != 7 {
		t.Errorf("top process = %+v, want process 100 / 2 ticks / style 7 (most ticks first)", procs[0])
	}
	if procs[1].ProcessID != 200 || procs[1].Ticks != 1 || procs[1].StyleID != 9 {
		t.Errorf("second process = %+v, want process 200 / 1 tick / style 9", procs[1])
	}
}
