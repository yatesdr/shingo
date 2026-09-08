package store

import (
	"testing"
	"time"

	"shingoedge/store/counters"
)

// mustBucket turns an RFC3339 UTC instant into the hour bucket it belongs to.
// Seeding through this rather than through UpsertHourlyCount is deliberate:
// the writer buckets on time.Now(), which is no use for pinning a boundary.
func mustBucket(t *testing.T, rfc3339 string) int64 {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatalf("parse %q: %v", rfc3339, err)
	}
	return counters.HourBucket(ts)
}

// seedHourly writes one hourly_counts row directly, at a chosen UTC bucket.
func seedHourly(t *testing.T, db *DB, processID, styleID, bucket, delta int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO hourly_counts (process_id, style_id, bucket_start, delta)
		 VALUES (?, ?, ?, ?)`, processID, styleID, bucket, delta); err != nil {
		t.Fatalf("seed hourly bucket %d: %v", bucket, err)
	}
}

// TestDayBounds_CoversTheWholePlantDay pins the one place the plant zone still
// enters a read: a plant day is the half-open UTC range between its local
// midnights, which is 23, 24 or 25 hours wide depending on the date.
//
// A fixed 24-hour window would drop or double an hour twice a year, and the old
// plant-local schema could not express a 25-hour day at all — the repeated hour
// collided on the unique key and the upsert summed two different real hours
// into one row.
func TestDayBounds_CoversTheWholePlantDay(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata for America/Chicago: %v", err)
	}

	for _, tc := range []struct {
		date  string
		hours int64
	}{
		{"2026-05-04", 24},
		{"2026-03-08", 23}, // spring forward: local 02:00 never happens
		{"2026-11-01", 25}, // fall back: local 01:00 happens twice
	} {
		from, to, err := counters.DayBounds(tc.date, loc)
		if err != nil {
			t.Fatalf("day bounds %s: %v", tc.date, err)
		}
		if got := (to - from) / 3600; got != tc.hours {
			t.Errorf("%s spans %d hours, want %d", tc.date, got, tc.hours)
		}
	}

	if _, _, err := counters.DayBounds("not-a-date", loc); err == nil {
		t.Error("DayBounds accepted a malformed date")
	}
}

// TestHourBucket_IsTheUTCHourStart pins the single definition writer and reader
// both go through, so they cannot disagree about where an hour begins.
func TestHourBucket_IsTheUTCHourStart(t *testing.T) {
	t.Parallel()
	ts, err := time.Parse(time.RFC3339, "2026-05-04T07:41:37Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := mustBucket(t, "2026-05-04T07:00:00Z")
	if got := counters.HourBucket(ts); got != want {
		t.Errorf("bucket = %d, want %d — an instant must land on its own UTC hour", got, want)
	}
	// And the zone the caller happens to be in must not move it.
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	if got := counters.HourBucket(ts.In(denver)); got != want {
		t.Errorf("bucket = %d in Denver, want %d — bucketing must be zone-free", got, want)
	}
}

// TestDailyCounts_IsGone pins the removal. daily_counts cached
// SUM(hourly_counts.delta) grouped by a PLANT-LOCAL day, which put a timezone
// into stored data one table after the UTC move took it out of another. It was
// dropped rather than frozen because it is pure redundancy — verified on
// Hopkinsville, all 84 rows reproduced exactly from the hour rows, zero
// exceptions — and those hours are now kept permanently, so the second copy
// bought nothing. Pre-migration day totals are still a SUM ... GROUP BY
// count_date away in hourly_counts_local_legacy.
func TestDailyCounts_IsGone(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='daily_counts'`).Scan(&n); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if n != 0 {
		t.Errorf("daily_counts still exists — a fresh database must not carry a table whose " +
			"only job was surviving a purge that no longer happens")
	}
}

// TestHourlyCounts_SurviveADanglingStyle is the live half of the same concern:
// Springfield carries 457 hourly rows whose style is gone, so a read must not
// fail on one. This is the style-32 row RUNBOOK-0.5 leaves behind, in miniature.
func TestHourlyCounts_SurviveADanglingStyle(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	bucket := mustBucket(t, "2026-05-04T06:00:00Z")
	seedHourly(t, db, pid, sid, bucket, 42)
	if _, err := db.Exec(`DELETE FROM styles WHERE id = ?`, sid); err != nil {
		t.Fatalf("hard-delete style: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable fk enforcement: %v", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = OFF`)

	list, err := db.ListHourlyCounts(pid, sid, bucket, bucket+3600)
	if err != nil {
		t.Fatalf("reading an orphaned hour failed under foreign_keys(1): %v — this is exactly "+
			"the broken-background-job failure the no-FK decision avoids", err)
	}
	if len(list) != 1 || list[0].Delta != 42 {
		t.Errorf("orphaned hour = %+v, want one row with delta 42", list)
	}
}
