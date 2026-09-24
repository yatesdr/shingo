//go:build docker

package heartbeat_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/heartbeat"
)

// TestCoverage_HeartbeatStore exercises the partitioned cell_part_events path
// end-to-end against real Postgres (the DDL the local build can't validate):
// partition creation, projection insert, ordered read, the unique-key dedup,
// and retention.
func TestCoverage_HeartbeatStore(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()

	if err := heartbeat.EnsurePartitions(db.DB, now); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}
	// Idempotent.
	if err := heartbeat.EnsurePartitions(db.DB, now); err != nil {
		t.Fatalf("EnsurePartitions (2nd): %v", err)
	}

	e1 := heartbeat.PartEvent{CellID: "STN-A", RecordedAt: now.Add(-2 * time.Minute), EdgeSnapshotID: 1, Delta: 1, CountValue: 100}
	e2 := heartbeat.PartEvent{CellID: "STN-A", RecordedAt: now.Add(-1 * time.Minute), EdgeSnapshotID: 2, Delta: 1, CountValue: 101}
	if got, rej := heartbeat.InsertPartEvents(db.DB, []heartbeat.PartEvent{e1, e2}); rej != nil || len(got) != 2 {
		t.Fatalf("InsertPartEvents: %d rows, rejected %v, want 2/none", len(got), rej)
	}

	got, err := heartbeat.ListEvents(db.DB, "STN-A", now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListEvents len = %d, want 2", len(got))
	}
	if !got[0].RecordedAt.Before(got[1].RecordedAt) {
		t.Error("ListEvents not ascending by recorded_at")
	}

	// Dedup is the table's unique key: the same tick again inserts nothing; the
	// same edge id from another station, or with a new recorded_at (a restored
	// edge), is a new row.
	if got, rej := heartbeat.InsertPartEvents(db.DB, []heartbeat.PartEvent{e1}); rej != nil || len(got) != 0 {
		t.Fatalf("re-sent tick: %d rows, rejected %v, want 0/none", len(got), rej)
	}
	other := e1
	other.CellID = "STN-B"
	restored := e1
	restored.RecordedAt = now.Add(-30 * time.Second)
	if got, rej := heartbeat.InsertPartEvents(db.DB, []heartbeat.PartEvent{other, restored}); rej != nil || len(got) != 2 {
		t.Fatalf("cross-station + restored: %d rows, rejected %v, want 2/none", len(got), rej)
	}

	// Retention: a partition 200 days old should drop with keepDays=90.
	old := now.AddDate(0, 0, -200)
	if err := heartbeat.EnsurePartitions(db.DB, old); err != nil {
		t.Fatalf("EnsurePartitions(old): %v", err)
	}
	dropped, err := heartbeat.DropOldPartitions(db.DB, 90, now)
	if err != nil {
		t.Fatalf("DropOldPartitions: %v", err)
	}
	if dropped < 1 {
		t.Errorf("DropOldPartitions dropped %d, want >= 1 (the 200-day-old month)", dropped)
	}
}
