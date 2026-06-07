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
// partition creation, projection insert, ordered read, dedup, and retention.
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
	if err := heartbeat.InsertPartEvent(db.DB, e1); err != nil {
		t.Fatalf("InsertPartEvent e1: %v", err)
	}
	if err := heartbeat.InsertPartEvent(db.DB, e2); err != nil {
		t.Fatalf("InsertPartEvent e2: %v", err)
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

	// Dedup: first is new, second is a duplicate.
	if isNew, err := heartbeat.TryDedup(db.DB, "STN-A", 42); err != nil || !isNew {
		t.Fatalf("TryDedup first: isNew=%v err=%v, want true/nil", isNew, err)
	}
	if isNew, err := heartbeat.TryDedup(db.DB, "STN-A", 42); err != nil || isNew {
		t.Fatalf("TryDedup dup: isNew=%v err=%v, want false/nil", isNew, err)
	}
	// Same Edge ID, different station → not a duplicate (composite key, §8 #22).
	if isNew, err := heartbeat.TryDedup(db.DB, "STN-B", 42); err != nil || !isNew {
		t.Fatalf("TryDedup cross-station: isNew=%v err=%v, want true/nil", isNew, err)
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
