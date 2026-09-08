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

// seedFrozenDaily writes a row into the pre-migration daily_counts table, which
// nothing writes at runtime any more. This is what a plant's history looks like
// after the 2026-09 migration parked its plant-local hours.
func seedFrozenDaily(t *testing.T, db *DB, processID, styleID int64, countDate string, total int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO daily_counts (process_id, style_id, count_date, total)
		 VALUES (?, ?, ?, ?)`, processID, styleID, countDate, total); err != nil {
		t.Fatalf("seed frozen daily %s: %v", countDate, err)
	}
}

// dailyTotal asks the read path for one (process, style, plant-local date).
func dailyTotal(t *testing.T, db *DB, processID, styleID int64, countDate string, loc *time.Location) (int64, bool) {
	t.Helper()
	rows, err := counters.ListDaily(db.DB, processID, countDate, countDate, loc)
	if err != nil {
		t.Fatalf("list daily %s: %v", countDate, err)
	}
	for _, r := range rows {
		if r.StyleID == styleID {
			return r.Total, true
		}
	}
	return 0, false
}

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata for America/Chicago: %v", err)
	}
	return loc
}

// TestListDaily_SumsHoursIntoDays is the derivation contract: a day's hours add
// up per style, days stay separate, and a late hour needs no job to be picked
// up — there is no stored total to go stale.
func TestListDaily_SumsHoursIntoDays(t *testing.T) {
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

	if got, ok := dailyTotal(t, db, pid, sidA, day, time.UTC); !ok || got != 30 {
		t.Errorf("%s total = %d (present=%v), want 30", day, got, ok)
	}
	if got, ok := dailyTotal(t, db, pid, sidB, day, time.UTC); !ok || got != 5 {
		t.Errorf("style B on %s = %d (present=%v), want 5 — styles were merged", day, got, ok)
	}
	if got, _ := dailyTotal(t, db, pid, sidA, "2026-05-05", time.UTC); got != 99 {
		t.Errorf("2026-05-05 total = %d, want 99", got)
	}

	// A late hour lands. Nothing has to be re-run: the total is a question, not
	// a row, so the next read already includes it.
	seedHourly(t, db, pid, sidA, mustBucket(t, "2026-05-04T08:00:00Z"), 7)
	if got, _ := dailyTotal(t, db, pid, sidA, day, time.UTC); got != 37 {
		t.Errorf("after the late hour total = %d, want 37 — a cached rollup has come back", got)
	}
}

// TestListDaily_GroupsIntoPlantLocalDays pins the whole point of storing UTC:
// the day a bucket belongs to is decided on the way OUT, in the plant's zone.
//
// 04:00Z on the 4th is 23:00 CDT on the 3rd, so the last hour of the plant's
// day has to land on the 3rd — the hour Hopkinsville was getting wrong for
// three months, in the opposite direction.
func TestListDaily_GroupsIntoPlantLocalDays(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T04:00:00Z"), 11) // 23:00 CDT, the 3rd
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T05:00:00Z"), 22) // 00:00 CDT, the 4th

	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-03", loc); !ok || got != 11 {
		t.Errorf("2026-05-03 = %d (present=%v), want 11 — the plant's last hour was "+
			"booked to the wrong calendar day", got, ok)
	}
	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-04", loc); !ok || got != 22 {
		t.Errorf("2026-05-04 = %d (present=%v), want 22", got, ok)
	}

	// Stated so the difference is visible rather than asserted in the abstract:
	// asked in UTC, both hours fall on the 4th. Same rows, different question.
	if got, _ := dailyTotal(t, db, pid, sid, "2026-05-04", time.UTC); got != 33 {
		t.Errorf("UTC grouping put %d on 2026-05-04, want 33 — the two zones must disagree "+
			"here, or this test is not testing anything", got)
	}
}

// TestListDaily_ChangingZoneNeedsNoMigration is the property the whole move
// exists for: the same stored rows answer differently when the plant zone
// changes, with nothing rewritten and no job re-run. Under the old scheme this
// needed a backfill, and before the hours were kept it was not possible at all.
func TestListDaily_ChangingZoneNeedsNoMigration(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T04:00:00Z"), 11)

	if got, _ := dailyTotal(t, db, pid, sid, "2026-05-03", loc); got != 11 {
		t.Errorf("Chicago: 05-03 = %d, want 11", got)
	}
	if got, _ := dailyTotal(t, db, pid, sid, "2026-05-04", time.UTC); got != 11 {
		t.Errorf("UTC: 05-04 = %d, want 11 — the same row must answer both questions", got)
	}
}

// TestListDaily_DSTFallBackKeepsBothRepeatedHours is the bug the old schema
// could not express.
//
// On 2026-11-01 America/Chicago repeats 01:00: 06:00Z is 01:00 CDT and 07:00Z
// is 01:00 CST. Under the old plant-local key both writes carried
// (count_date=2026-11-01, hour=1), collided on the UNIQUE constraint, and the
// upsert SUMMED two different real hours into one row — indistinguishable,
// once stored, from a single busy hour. Bucketed in UTC they stay two rows, and
// the day is 25 hours wide with both in it.
func TestListDaily_DSTFallBackKeepsBothRepeatedHours(t *testing.T) {
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

	if got, ok := dailyTotal(t, db, pid, sid, "2026-11-01", loc); !ok || got != 42 {
		t.Errorf("fall-back day = %d (present=%v), want 42 — a 25-hour day must carry both "+
			"passes through 01:00", got, ok)
	}
}

// TestListDaily_DSTSpringForwardIsA23HourDay is the other half. Local 02:00
// never happens on 2026-03-08, so the day is 23 hours; production either side
// of the skip still belongs to that date.
func TestListDaily_DSTSpringForwardIsA23HourDay(t *testing.T) {
	t.Parallel()
	loc := chicago(t)
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	seedHourly(t, db, pid, sid, mustBucket(t, "2026-03-08T07:00:00Z"), 5) // 01:00 CST
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-03-08T08:00:00Z"), 6) // 03:00 CDT, 02:00 skipped

	if got, ok := dailyTotal(t, db, pid, sid, "2026-03-08", loc); !ok || got != 11 {
		t.Errorf("spring-forward day = %d (present=%v), want 11", got, ok)
	}
}

// TestListDaily_FrozenHistoryAnswersButNeverShadows pins the two-source read.
//
// Pre-migration days have no UTC buckets — their hours are parked in
// hourly_counts_local_legacy and deliberately not reinterpreted — so their
// totals come from the frozen daily_counts table. A day the buckets DO cover
// must come from the buckets, even if a stale rollup row for it survives,
// because the buckets are the live truth and the stored row is a relic.
func TestListDaily_FrozenHistoryAnswersButNeverShadows(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	// Pre-migration: history only.
	seedFrozenDaily(t, db, pid, sid, "2026-06-17", 1234)
	// Post-migration: buckets, plus a stale rollup row that must lose.
	seedFrozenDaily(t, db, pid, sid, "2026-09-20", 999)
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-09-20T06:00:00Z"), 7)

	if got, ok := dailyTotal(t, db, pid, sid, "2026-06-17", time.UTC); !ok || got != 1234 {
		t.Errorf("frozen history = %d (present=%v), want 1234 — pre-migration days must "+
			"still answer", got, ok)
	}

	// COUNT THE ROWS, don't just read the first one. Returning both the derived
	// day and its stale rollup row would leave the right answer sitting in front
	// of the wrong one, which reads as correct through any helper that takes the
	// first match — and is a duplicate day to every caller that iterates.
	rows, err := counters.ListDaily(db.DB, pid, "2026-09-20", "2026-09-20", time.UTC)
	if err != nil {
		t.Fatalf("list daily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("derived day returned %d rows, want 1 — the frozen rollup row was emitted "+
			"alongside the live one: %+v", len(rows), rows)
	}
	if rows[0].Total != 7 {
		t.Errorf("derived day = %d, want 7 — a stale rollup row shadowed the live buckets", rows[0].Total)
	}
}

// TestDailyCounts_CarriesNoForeignKeys pins the deliberate absence of FKs on
// the history table: CASCADE would let a process delete destroy the permanent
// record, RESTRICT would rebuild the counter_snapshots trap, and either would
// let a read fail on a dangling parent (Springfield carries 457 such hourly
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

	// And the read must survive a parent that is gone — the style-32 row
	// RUNBOOK-0.5 leaves behind, in miniature.
	pid, sid := seedProcessStyle(t, db, "P", "S")
	seedHourly(t, db, pid, sid, mustBucket(t, "2026-05-04T06:00:00Z"), 42)
	if _, err := db.Exec(`DELETE FROM styles WHERE id = ?`, sid); err != nil {
		t.Fatalf("hard-delete style: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable fk enforcement: %v", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = OFF`)

	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-04", time.UTC); !ok || got != 42 {
		t.Errorf("orphaned hour did not survive the read: total = %d, present = %v, want 42", got, ok)
	}
}

// TestDayBounds_CoversTheWholePlantDay pins the read-side half: a plant day is
// the half-open UTC range between its local midnights, 23, 24 or 25 hours wide
// depending on the date. A fixed 24-hour window would drop or double an hour
// twice a year.
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
