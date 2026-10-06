package telemetry

import (
	"testing"
	"time"
)

// TestFillBuckets_PlantDaysAcrossDST pins the continuous day series on plant
// midnights across America/Chicago's 25-hour day (2026-11-01). Stepping a fixed
// 24 hours from Oct 31 05:00Z would land Nov 2 at 05:00Z — 23:00 CST on Nov 1 —
// and every later bucket an hour early.
func TestFillBuckets_PlantDaysAcrossDST(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 31, 0, 0, 0, 0, chicago).UTC()
	until := time.Date(2026, 11, 2, 23, 59, 59, 0, chicago).UTC()
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	got := fillBuckets(map[time.Time]*Bucket{}, Filter{Since: &since, Until: &until}, "day", chicago, now)
	want := []string{"2026-10-31T05:00:00Z", "2026-11-01T05:00:00Z", "2026-11-02T06:00:00Z"}
	if len(got) != len(want) {
		t.Fatalf("got %d buckets, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if s := got[i].BucketStart.UTC().Format(time.RFC3339); s != w {
			t.Errorf("bucket %d = %s, want %s", i, s, w)
		}
	}
}

// TestFillBuckets_StopsAtNowAndKeepsRows pins the two edges of the fill: the
// series ends at the bucket now falls in (no future hours drawn as zeros), and a
// row outside the generated span is kept, so the fill never loses a count.
func TestFillBuckets_StopsAtNowAndKeepsRows(t *testing.T) {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24*time.Hour - time.Nanosecond)
	now := since.Add(2*time.Hour + 30*time.Minute)
	stray := since.Add(-time.Hour)
	rows := map[time.Time]*Bucket{
		since.Add(time.Hour): {BucketStart: since.Add(time.Hour), Total: 4},
		stray:                {BucketStart: stray, Total: 1},
	}
	got := fillBuckets(rows, Filter{Since: &since, Until: &until}, "hour", time.UTC, now)
	totals := []int64{1, 0, 4, 0} // stray, 00:00, 01:00, 02:00 (in progress)
	if len(got) != len(totals) {
		t.Fatalf("got %d buckets, want %d: %v", len(got), len(totals), got)
	}
	for i, w := range totals {
		if got[i].Total != w {
			t.Errorf("bucket %d total = %d, want %d", i, got[i].Total, w)
		}
	}
}
