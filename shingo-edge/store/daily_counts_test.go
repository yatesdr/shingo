package store

import (
	"testing"
	"time"

	"shingoedge/store/counters"
)

// seedHourly writes one hourly_counts row directly. UpsertHourlyCount buckets
// on time.Now(), which is no use for testing a 90-day cutoff.
func seedHourly(t *testing.T, db *DB, processID, styleID int64, countDate string, hour int, delta int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO hourly_counts (process_id, style_id, count_date, hour, delta)
		 VALUES (?, ?, ?, ?, ?)`, processID, styleID, countDate, hour, delta); err != nil {
		t.Fatalf("seed hourly %s h%d: %v", countDate, hour, err)
	}
}

func hourlyRowCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hourly_counts`).Scan(&n); err != nil {
		t.Fatalf("count hourly: %v", err)
	}
	return n
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

// TestRollUpDaily_SumsHoursAndStaysIdempotent is the rollup contract: a day's
// hours become one row carrying their sum, re-running changes nothing, and a
// late hour is picked up because the total is RECOMPUTED rather than added to.
//
// Verified red three ways:
//   - SUM(delta) -> COUNT(delta): "2026-05-04 total = 3, want 30".
//   - dropping the GROUP BY style_id: "styles were merged into one row".
//   - DO UPDATE SET total = excluded.total -> total = daily_counts.total (the
//     accumulate spelling): "after the late hour total = 30, want 37".
func TestRollUpDaily_SumsHoursAndStaysIdempotent(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sidA := seedProcessStyle(t, db, "P", "A")
	sidB, err := db.CreateStyle("B", "", pid)
	if err != nil {
		t.Fatalf("create style B: %v", err)
	}

	const day = "2026-05-04"
	seedHourly(t, db, pid, sidA, day, 6, 10)
	seedHourly(t, db, pid, sidA, day, 7, 20)
	seedHourly(t, db, pid, sidB, day, 7, 5)
	seedHourly(t, db, pid, sidA, "2026-05-05", 6, 99)

	// frozenBefore well in the past: nothing is frozen for this test.
	const openWindow = "2000-01-01"
	if _, err := counters.RollUpDaily(db.DB, openWindow); err != nil {
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
	if _, err := counters.RollUpDaily(db.DB, openWindow); err != nil {
		t.Fatalf("second roll up: %v", err)
	}
	if got, _ := dailyTotal(t, db, pid, sidA, day); got != 30 {
		t.Errorf("after a second pass total = %d, want 30 — the rollup accumulates instead of recomputing", got)
	}

	// A late hour lands. Recomputation must pick it up.
	seedHourly(t, db, pid, sidA, day, 8, 7)
	if _, err := counters.RollUpDaily(db.DB, openWindow); err != nil {
		t.Fatalf("third roll up: %v", err)
	}
	if got, _ := dailyTotal(t, db, pid, sidA, day); got != 37 {
		t.Errorf("after the late hour total = %d, want 37", got)
	}
}

// TestRollUpDaily_FreezesDatesPastTheCutoff pins the guard on the word
// "permanent": once a date is older than the retention cutoff its daily total
// can no longer be rewritten, so an hourly row appearing for an already-purged
// day — a backwards clock step on a Pi with no RTC — cannot overwrite years-old
// truth with a fragment.
//
// The INSERT half is asserted in the same test on purpose: freezing must not
// stop a never-summarised old date being captured, which is the normal case on
// the FIRST pass at an existing plant (Springfield's hourly detail reaches back
// 96 days).
//
// Verified red: removing `WHERE daily_counts.count_date >= ?` from the DO
// UPDATE fails with "the stray hour rewrote a frozen day: total = 3, want 500".
func TestRollUpDaily_FreezesDatesPastTheCutoff(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	const oldDay = "2026-01-01"
	const cutoff = "2026-04-01"

	// A never-summarised date older than the cutoff is still captured: this is
	// the INSERT path, which is deliberately not frozen.
	seedHourly(t, db, pid, sid, oldDay, 6, 500)
	if _, err := counters.RollUpDaily(db.DB, cutoff); err != nil {
		t.Fatalf("first roll up: %v", err)
	}
	if got, ok := dailyTotal(t, db, pid, sid, oldDay); !ok || got != 500 {
		t.Fatalf("first pass did not capture a pre-cutoff date: total = %d, present = %v, want 500", got, ok)
	}

	// The detail ages out, and then a stray hour appears for that same day.
	if _, err := counters.PurgeRolledUpHourly(db.DB, cutoff); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := hourlyRowCount(t, db); n != 0 {
		t.Fatalf("purge left %d hourly rows, want 0", n)
	}
	seedHourly(t, db, pid, sid, oldDay, 9, 3)

	if _, err := counters.RollUpDaily(db.DB, cutoff); err != nil {
		t.Fatalf("second roll up: %v", err)
	}
	if got, _ := dailyTotal(t, db, pid, sid, oldDay); got != 500 {
		t.Errorf("the stray hour rewrote a frozen day: total = %d, want 500", got)
	}
}

// TestPurgeRolledUpHourly_OnlyDeletesWhatWasSummarised is the safety property
// the whole 90-day window rests on. An old hour whose day is in daily_counts
// goes; an old hour whose day is NOT goes nowhere, however old it is; a recent
// hour stays regardless.
//
// The un-summarised arm is the one that matters: it is what makes a failed
// rollup fail towards retaining data rather than towards losing it, and it is
// what stops a later refactor that separates the two calls from turning this
// into a plain DELETE.
//
// Verified red: dropping the EXISTS clause fails with "an hour with no daily
// row was deleted — a failed rollup would now lose data" and "purged 2 rows,
// want 1".
func TestPurgeRolledUpHourly_OnlyDeletesWhatWasSummarised(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "P", "S")

	const cutoff = "2026-04-01"
	seedHourly(t, db, pid, sid, "2026-01-01", 6, 11) // old, will be summarised
	seedHourly(t, db, pid, sid, "2026-02-02", 6, 22) // old, will NOT be
	seedHourly(t, db, pid, sid, "2026-06-06", 6, 33) // recent

	// Summarise only the first day.
	if _, err := db.Exec(`INSERT INTO daily_counts (process_id, style_id, count_date, total)
		VALUES (?, ?, '2026-01-01', 11)`, pid, sid); err != nil {
		t.Fatalf("seed daily: %v", err)
	}

	n, err := counters.PurgeRolledUpHourly(db.DB, cutoff)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}

	var jan, feb, jun int
	db.QueryRow(`SELECT COUNT(*) FROM hourly_counts WHERE count_date='2026-01-01'`).Scan(&jan)
	db.QueryRow(`SELECT COUNT(*) FROM hourly_counts WHERE count_date='2026-02-02'`).Scan(&feb)
	db.QueryRow(`SELECT COUNT(*) FROM hourly_counts WHERE count_date='2026-06-06'`).Scan(&jun)

	if jan != 0 {
		t.Error("a summarised old hour survived the purge")
	}
	if feb != 1 {
		t.Error("an hour with no daily row was deleted — a failed rollup would now lose data")
	}
	if jun != 1 {
		t.Error("an hour inside the window was purged")
	}
}

// TestPurgeRolledUpHourly_NullSafe is the sibling of
// TestPurgeOldCounterSnapshots_DeletesNullAnomalyUnconfirmed, and it exists
// because that bug's shape is a class rather than an incident: a purge
// predicate over a column that can be NULL is three-valued, and the rows it
// silently keeps are invisible from the outside.
//
// The claim under test is that this predicate CANNOT have that defect, and the
// claim is checked rather than asserted. Every column it touches is declared
// NOT NULL, so the schema itself is the proof — the test reads
// pragma_table_info instead of trusting the CREATE TABLE text in the repo,
// because a plant's shape is whatever its migration path left behind. EXISTS
// contributes 0 or 1 and never NULL, so with NOT NULL inputs the whole
// predicate is two-valued.
//
// Verified red: dropping NOT NULL from count_date in schema/sqlite_ddl.go makes
// this fail with "hourly_counts.count_date is nullable — PurgeRolledUpHourly's
// predicate is no longer two-valued".
func TestPurgeRolledUpHourly_NullSafe(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	for _, col := range []string{"process_id", "style_id", "count_date"} {
		var notNull int
		if err := db.QueryRow(
			`SELECT "notnull" FROM pragma_table_info('hourly_counts') WHERE name = ?`, col).
			Scan(&notNull); err != nil {
			t.Fatalf("pragma_table_info hourly_counts.%s: %v", col, err)
		}
		if notNull != 1 {
			t.Errorf("hourly_counts.%s is nullable — PurgeRolledUpHourly's predicate is no longer two-valued; "+
				"it needs the COALESCE treatment counters.PurgeOldSnapshots got", col)
		}
	}
	for _, col := range []string{"process_id", "style_id", "count_date"} {
		var notNull int
		if err := db.QueryRow(
			`SELECT "notnull" FROM pragma_table_info('daily_counts') WHERE name = ?`, col).
			Scan(&notNull); err != nil {
			t.Fatalf("pragma_table_info daily_counts.%s: %v", col, err)
		}
		if notNull != 1 {
			t.Errorf("daily_counts.%s is nullable — the EXISTS subquery would stop matching silently", col)
		}
	}
}

// TestDailyCounts_CarriesNoForeignKeys pins the design decision argued in
// schema/sqlite_ddl.go, because it is the kind that gets "tidied" later by
// somebody making the new table look like its sibling.
//
// Two things are at stake. A CASCADE edge would let a hard DELETE of a process
// (store/processes/processes.go, DeleteProcess — still a hard delete) destroy
// the permanent record; a RESTRICT edge would rebuild the trap that made 6 of 8
// style deletions impossible on the Springfield dump. And a table with no FK
// clauses contributes nothing to PRAGMA foreign_key_check, so it cannot regress
// the enforcement gate RUNBOOK-0.5 exists to open — asserted here directly.
//
// Verified red: adding `REFERENCES processes(id) ON DELETE CASCADE` to
// daily_counts.process_id fails with "daily_counts declares 1 foreign key(s)".
func TestDailyCounts_CarriesNoForeignKeys(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)

	var fks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('daily_counts')`).Scan(&fks); err != nil {
		t.Fatalf("pragma_foreign_key_list: %v", err)
	}
	if fks != 0 {
		t.Errorf("daily_counts declares %d foreign key(s); it must declare none — "+
			"CASCADE would let a process delete destroy the permanent record, RESTRICT would "+
			"rebuild the counter_snapshots trap, and either would let the rollup fail on a "+
			"dangling parent (Springfield carries 457 such hourly rows)", fks)
	}

	// The rollup must therefore survive a parent that is gone. This is the
	// style-32 row RUNBOOK-0.5 leaves behind, in miniature.
	pid, sid := seedProcessStyle(t, db, "P", "S")
	seedHourly(t, db, pid, sid, "2026-05-04", 6, 42)
	if _, err := db.Exec(`DELETE FROM styles WHERE id = ?`, sid); err != nil {
		t.Fatalf("hard-delete style: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable fk enforcement: %v", err)
	}
	defer db.Exec(`PRAGMA foreign_keys = OFF`)

	if _, err := counters.RollUpDaily(db.DB, "2000-01-01"); err != nil {
		t.Fatalf("rollup refused a dangling style under foreign_keys(1): %v — "+
			"this is exactly the broken-background-job failure the no-FK decision avoids", err)
	}
	if got, ok := dailyTotal(t, db, pid, sid, "2026-05-04"); !ok || got != 42 {
		t.Errorf("orphaned hour was not rolled up: total = %d, present = %v, want 42", got, ok)
	}
}

// TestCutoffDate_UsesThePlantsTimezone pins the coupling between
// counters.CutoffDate and engine.HourlyTracker: count_date is written in the
// plant's local zone, so a cutoff rendered in UTC is a calendar day too
// aggressive for the hours the plant sits behind it.
//
// Verified red: replacing now.In(loc) with now.UTC() in CutoffDate fails with
// "cutoff = 2026-05-04, want 2026-05-03".
func TestCutoffDate_UsesThePlantsTimezone(t *testing.T) {
	t.Parallel()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no tzdata for America/Chicago: %v", err)
	}

	// 03:00 UTC on 2026-08-02 is 22:00 CDT on 2026-08-01 — a different
	// calendar day, which is the whole point.
	now := time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC)
	got := counters.CutoffDate(now, chicago, 90*24*time.Hour)
	if want := "2026-05-03"; got != want {
		t.Errorf("cutoff = %s, want %s — the window is a calendar day off for the "+
			"hours the plant sits behind UTC", got, want)
	}
	// And UTC's own answer, stated so the difference is visible rather than
	// asserted in the abstract.
	if utc := counters.CutoffDate(now, time.UTC, 90*24*time.Hour); utc != "2026-05-04" {
		t.Errorf("UTC cutoff = %s, want 2026-05-04 (the value this function exists to avoid)", utc)
	}
}
