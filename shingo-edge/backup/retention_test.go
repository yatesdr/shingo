package backup

import (
	"testing"
	"time"
)

func TestRetainedKeysKeepsLatestAndBuckets(t *testing.T) {
	base := time.Date(2026, 3, 21, 15, 0, 0, 0, time.UTC)
	items := []SnapshotInfo{
		{Key: "latest", CreatedAt: timePtr(base)},
		{Key: "hour-1", CreatedAt: timePtr(base.Add(-1 * time.Hour))},
		{Key: "hour-2", CreatedAt: timePtr(base.Add(-2 * time.Hour))},
		{Key: "day-1", CreatedAt: timePtr(base.Add(-24 * time.Hour))},
		{Key: "week-1", CreatedAt: timePtr(base.Add(-7 * 24 * time.Hour))},
		{Key: "month-1", CreatedAt: timePtr(base.AddDate(0, -1, 0))},
	}

	keep := retainedKeys(items, 2, 2, 1, 1)

	for _, key := range []string{"latest", "hour-1", "day-1"} {
		if _, ok := keep[key]; !ok {
			t.Fatalf("expected key %q to be retained", key)
		}
	}
	if _, ok := keep["hour-2"]; ok {
		t.Fatalf("expected older hourly snapshot to be pruned")
	}
	if len(keep) < 3 {
		t.Fatalf("expected at least three retained snapshots, got %d", len(keep))
	}
}

func timePtr(v time.Time) *time.Time { return &v }
