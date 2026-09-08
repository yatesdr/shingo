package store

import (
	"testing"
	"time"

	"shingoedge/store/counters"
)

// mustBucket turns an RFC3339 UTC instant into the hour bucket it belongs to.
// Seeding through this rather than through UpsertHourlyCount is deliberate:
// the writer buckets on time.Now(), which is no use for pinning a day boundary
// or a DST transition.
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

// dailyTotal returns the stored total for one (process, style, date), and
// whether a row exists at all.
func dailyTotal(t *testing.T, db *DB, processID, styleID int64, countDate string) (int64, bool) {
	t.Helper()
	var total int64
	err := db.QueryRow(`SELECT total FROM daily_counts
		WHERE process_id = ? AND style_id = ? AND count_date = ?`,
		processID, styleID, countDate).Scan(&total)
	if err != nil {
		return 0, false
	}
	return total, true
}

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata for America/Chicago: %v", err)
	}
	return loc
}

// TestRollUpDaily_SumsHoursAndStaysIdempotent is the rollup contract: a day's
// hours become one row carrying their sum, re-running changes nothing, and a
// late hour is picked up because the total is RECOMPUTED rather than added to.
func TestRollUpDaily_SumsHoursAndStaysIdempotent(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sidA := seedProcessStyle(t, db, "P", "A")
	sidB, err := db.CreateStyle("B", "", pid)
	if err != nil {
		t.Fatalf("create style B: %v", err)
	}

	const day = "2026-05-04"
	seedHourly(t, db, pid, sidA, mustBucket(t, "2026-05-04T06:00:00Z"), 10)
	seedHourly(t, db, pid, sidA, mustBucket(t, "2026-05-04T07:00:00Z"), 20)
	seedHourly(t, db, pid, sidB, mustBucket(t, "2026-05-04T07:00:00Z"), 5)
	seedHourly(t, db, pid, sidA, mustBucket(t, "2026-05-05T06:00:00Z"), 99)

	if _, err := counters.RollUpDaily(db.DB, time.UTC); err != nil {
		t.Fatalf("roll up: %v", err)
	}

	if got, ok := dailyTotal(t, db, pid, sidA, day); !ok || got != 30 {
		t.Errorf("%s total = %d (present=%v), want 30", day, got, ok)
	}
	if got, ok := dailyTotal(t, db, pid, sidB, day); !ok || got != 5 {
		t.Errorf("style B on %s = %d (present=%v), want 5 — styles were merged into one row", day, got, ok)
	}
	if got, _ := dailyTotal(t, db, pid, sidA, "2026-05-05"); got != 99 {
		t.Errorf("2026-05-05 total = %d, want 99", got)
	}

	// Idempotent: a second pass over unchanged detail leaves the same numbers.
	if _, err := counters.RollUpDaily(db.DB, time.UTC); err != nil {
		t.Fatalf("second roll up: %v", err)
	}
	if got, _ := dailyTotal(t, db, pid, sidA, day); got != 30 {
		t.Errorf("after a second pass total = %d, want 30 — the rollup accumulates instead of recomputing", got)
	}

	// A late hour lands. Recomputation must pick it up.
	seedHourly(t, db, pid, sidA, mustBucket(t, "2026-05-04T08:00:00Z"), 7)
	if _, err := counters.RollUpDaily(db.DB, time.UTC); err != nil {
		t.Fatalf("third roll up: %v", err)
	}
	if got, _ := dailyTotal(t, db, pid, sidA, day); got != 37 {
		t.Errorf("after the late hour total = %d, want 37", got)
	}
}

// TestRollUpDaily_GroupsIntoPlantLocalDays pins the whole point of storing UTC:
// the day a bucket belongs to is decided on the way OUT, in the plant's zone,
// not on the way in.
//
// 04:00Z on the 4th is 23:00 CDT on the 3rd. The last hour of the plant's day
// therefore has to land on the 3rd, which is exactly the hour Hopkinsville was
// getting wrong for three months — its buckets were stamped in an Eastern zone,
// so that production was booked to the following date.
func TestRollUpDaily_GroupsIntoPlantLocalDays(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T04:00:00Z"), 11) // 23:00 CDT on the 3rd
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T05:00:00Z"), 22) // 00:00 CDT on the 4th

	if _, err := counters.RollUpDaily(db.DB, loc); err != nil {
		t.Fatalf("roll up: %v", err)
	}

	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-03"); !ok || got != 11 {
		t.Errorf("2026-05-03 total = %d (present=%v), want 11 — the plant's last hour "+
			"was booked to the wrong calendar day", got, ok)
	}
	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-04"); !ok || got != 22 {
		t.Errorf("2026-05-04 total = %d (present=%v), want 22", got, ok)
	}

	// Stated so the difference is visible rather than asserted in the abstract:
	// rolled up in UTC, both hours fall on the 4th.
	if _, err := counters.RollUpDaily(db.DB, time.UTC); err != nil {
		t.Fatalf("roll up in UTC: %v", err)
	}
	if got, _ := dailyTotal(t, db, pid, sid, "2026-05-04"); got != 33 {
		t.Errorf("UTC grouping put %d on 2026-05-04, want 33 — the two zones must disagree here, "+
			"or this test is not testing anything", got)
	}
}

// TestRollUpDaily_DSTFallBackKeepsBothRepeatedHours is the bug the old schema
// could not even express.
//
// On 2026-11-01 America/Chicago repeats 01:00: 06:00Z is 01:00 CDT and 07:00Z
// is 01:00 CST. Under the old plant-local key both writes carried
// (count_date=2026-11-01, hour=1), collided on the UNIQUE constraint, and the
// upsert SUMMED two different real hours into one row — indistinguishable, once
// stored, from a single busy hour. Bucketed in UTC they stay two rows, and the
// day is 25 hours wide with both of them in it.
func TestRollUpDaily_DSTFallBackKeepsBothRepeatedHours(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	first := mustBucket(t, "2026-11-01T06:00:00Z")  // 01:00 CDT
	second := mustBucket(t, "2026-11-01T07:00:00Z") // 01:00 CST, the repeat
	if first == second {
		t.Fatal("the two repeated local hours share a bucket — they must not")
	}
	seedHourly(t, db, pid, sid, first, 40)
	seedHourly(t, db, pid, sid, second, 2)

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hourly_counts`).Scan(&rows); err != nil {
		t.Fatalf("count hourly: %v", err)
	}
	if rows != 2 {
		t.Fatalf("the repeated hour stored %d row(s), want 2 — a UTC bucket must keep them apart", rows)
	}

	if _, err := counters.RollUpDaily(db.DB, loc); err != nil {
		t.Fatalf("roll up: %v", err)
	}
	if got, ok := dailyTotal(t, db, pid, sid, "2026-11-01"); !ok || got != 42 {
		t.Errorf("fall-back day total = %d (present=%v), want 42 — a 25-hour day must carry "+
			"both passes through 01:00", got, ok)
	}
}

// TestRollUpDaily_DSTSpringForwardIsA23HourDay is the other half. Local 02:00
// never happens on 2026-03-08, so the day is 23 hours; production either side
// of the skip still belongs to that date and nothing is invented for the hour
// that does not exist.
func TestRollUpDaily_DSTSpringForwardIsA23HourDay(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	seedHourly(t, db, pid, sid, mustBucket(t, "2026-03-08T07:00:00Z"), 5) // 01:00 CST
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-03-08T08:00:00Z"), 6) // 03:00 CDT — 02:00 skipped

	if _, err := counters.RollUpDaily(db.DB, loc); err != nil {
		t.Fatalf("roll up: %v", err)
	}
	if got, ok := dailyTotal(t, db, pid, sid, "2026-03-08"); !ok || got != 11 {
		t.Errorf("spring-forward day total = %d (present=%v), want 11", got, ok)
	}
}

// TestDayBounds_CoversTheWholePlantDay pins the read-side half: a plant day is
// the half-open UTC range between its local midnights, which is 23, 24 or 25
// hours wide depending on the date. A fixed 24-hour window would silently drop
// or double an hour twice a year.
func TestDayBounds_CoversTheWholePlantDay(t *testing.T) {
	t.Parallel()
	loc := chicago(t)

	for _, tc := range []struct {
		date  string
		hours int64
	}{
		{"2026-05-04", 24},
		{"2026-03-08", 23}, // spring forward
		{"2026-11-01", 25}, // fall back
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

// TestDailyCounts_CarriesNoForeignKeys pins the deliberate absence of FKs on
// the permanent table: CASCADE would let a process delete destroy the permanent
// record, RESTRICT would rebuild the counter_snapshots trap, and either would
// let the rollup fail on a dangling parent (Springfield carries 457 such hourly
// rows).
func TestDailyCounts_CarriesNoForeignKeys(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	var fks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('daily_counts')`).Scan(&fks); err != nil {
		t.Fatalf("pragma_foreign_key_list: %v", err)
	}
	if fks != 0 {
		t.Errorf("daily_counts declares %d foreign key(s); it must declare none", fks)
	}

	// The rollup must therefore survive a parent that is gone. This is the
	// style-32 row RUNBOOK-0.5 leaves behind, in miniature.
	pid, sid := seedProcessStyle(t, db, "P", "S")
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T06:00:00Z"), 42)
	if _, err := db.Exec(`DELETE FROM styles WHERE id = ?`, sid); err != nil {
		t.Fatalf("hard-delete style: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable fk enforcement: %v", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = OFF`)

	if _, err := counters.RollUpDaily(db.DB, time.UTC); err != nil {
		t.Fatalf("rollup refused a dangling style under foreign_keys(1): %v — "+
			"this is exactly the broken-background-job failure the no-FK decision avoids", err)
	}
	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-04"); !ok || got != 42 {
		t.Errorf("orphaned hour was not rolled up: total = %d, present = %v, want 42", got, ok)
	}
}
