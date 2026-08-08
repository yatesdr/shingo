//go:build docker

package robotconfidence_test

import (
	"testing"
	"time"

	"shingocore/store/robotconfidence"
)

// catchup_docker_test.go — the scheduler's guards.
//
// The bug these exist to pin was not in any of this package's logic; it was in
// what DECIDED to call it. A 24-hour ticker started at boot never fires on a
// Core whose mean process life is eleven hours, so the aggregates stayed empty
// while every unit test in the suite passed. The scheduling question therefore
// has to be answered from the database, and these assert that it is.

// The headline case: a day's samples land, Core restarts before any timer
// could plausibly have fired, and the aggregates still get written.
func TestCatchUp_RollsUpADayNoTickerWouldHaveReached(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	yesterday := testDay.AddDate(0, 0, -1)
	insert(t, db,
		sample("AMR-01", yesterday.Add(9*time.Hour), 0.90, 5, 0, 1),
		sample("AMR-02", yesterday.Add(9*time.Hour+time.Minute), 0.80, 6, 0, 1),
	)

	// No ticker ever fired; this is the boot pass on a fresh process.
	results, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg())
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("rolled up %d days, want 1", len(results))
	}

	var rows int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, yesterday).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("segment rows for the caught-up day = %d, want 1", rows)
	}
}

// A backlog is processed OLDEST FIRST, so a run of missed days resolves with
// the same trailing baselines it would have had if nothing had been missed.
func TestCatchUp_ProcessesBacklogOldestFirst(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	for i := 3; i >= 1; i-- {
		day := testDay.AddDate(0, 0, -i)
		insert(t, db,
			sample("AMR-01", day.Add(9*time.Hour), 0.90, 5, 0, 1),
			sample("AMR-02", day.Add(9*time.Hour+time.Minute), 0.80, 6, 0, 1),
		)
	}

	results, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg())
	if err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("rolled up %d days, want 3", len(results))
	}
	for i := 1; i < len(results); i++ {
		if !results[i-1].Day.Before(results[i].Day) {
			t.Errorf("results are not oldest-first: %s came before %s",
				results[i-1].Day.Format("2006-01-02"), results[i].Day.Format("2006-01-02"))
		}
	}
}

// The pass runs hourly and at every boot, so it must be nearly free when there
// is nothing to do — otherwise a restart storm turns into a query storm.
func TestCatchUp_IsIdempotentAndQuietWhenCaughtUp(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	yesterday := testDay.AddDate(0, 0, -1)
	insert(t, db,
		sample("AMR-01", yesterday.Add(9*time.Hour), 0.90, 5, 0, 1),
		sample("AMR-02", yesterday.Add(9*time.Hour+time.Minute), 0.80, 6, 0, 1),
	)

	if _, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// Second pass: the day is done, so there is nothing pending.
	again, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second pass rolled up %d days, want 0", len(again))
	}

	var rows int
	db.QueryRow(`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, yesterday).Scan(&rows)
	if rows != 1 {
		t.Errorf("segment rows after two passes = %d, want 1", rows)
	}
}

// TODAY IS NEVER ROLLED UP. It is still accumulating, and a partial day
// written as though it were complete is indistinguishable from a real one
// afterwards — and, because the row exists, would never be recomputed.
func TestPendingDays_ExcludesToday(t *testing.T) {
	db := openWithWindow(t)
	insert(t, db, sample("AMR-01", testDay.Add(9*time.Hour), 0.90, 5, 0, 1))

	days, err := robotconfidence.PendingDays(db.DB, testDay, 14)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, d := range days {
		if d.Equal(testDay) {
			t.Fatal("today is still accumulating and must never be rolled up")
		}
	}
}

// A fresh install must not try to roll up every day of its retention window
// forever, finding nothing each time.
func TestPendingDays_IgnoresDaysWithNoSamples(t *testing.T) {
	db := openWithWindow(t)
	days, err := robotconfidence.PendingDays(db.DB, testDay, 14)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("empty database reported %d pending days, want 0", len(days))
	}
}

// Days whose raw partition has already been dropped can never be rolled up —
// there is nothing left to read — so the window must not reach past retention
// and re-ask an unanswerable question on every tick.
func TestPendingDays_StopsAtTheRetentionWindow(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	old := testDay.AddDate(0, 0, -10)
	insert(t, db, sample("AMR-01", old.Add(9*time.Hour), 0.90, 5, 0, 1))

	// Retention of 3 days: the 10-day-old sample is outside it.
	days, err := robotconfidence.PendingDays(db.DB, testDay, 3)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, d := range days {
		if d.Equal(old) {
			t.Error("a day outside the retention window must not be reported pending")
		}
	}
}

// DONE MEANS ALL FOUR TABLES, NOT JUST THE ROBOT ONE.
//
// The completion check used to ask whether robot_confidence_daily had the day,
// which is one tickbox for the four forms RollUp fills in. A run that wrote the
// robot rows and then died left the day marked done with its lane, zone and
// plant rows missing — and nothing would ever recompute it, because the tickbox
// said otherwise. The raw rows behind the gap then expire at retention.
//
// Deleting the plant row is how that half-finished state is reproduced without
// hand-building a row: it is the LAST thing RollUp writes, so its absence is
// exactly what a run that died partway through leaves behind.
func TestPendingDays_ADayMissingItsPlantRowIsNotDone(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	yesterday := testDay.AddDate(0, 0, -1)
	insert(t, db, sample("AMR-01", yesterday.Add(9*time.Hour), 0.90, 5, 0, 1))

	if _, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	// The run died after the robot rows and before the plant row.
	if _, err := db.Exec(`DELETE FROM plant_confidence_daily WHERE day=$1`, yesterday); err != nil {
		t.Fatalf("delete plant row: %v", err)
	}

	days, err := robotconfidence.PendingDays(db.DB, testDay, 14)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, d := range days {
		if d.Equal(yesterday) {
			return // correctly still pending
		}
	}
	t.Fatal("a day whose roll-up did not finish was reported done; its lane and zone rows can never be rebuilt")
}

// THE CASE THAT ACTUALLY BIT: an aggregate table added after days had already
// been rolled up.
//
// lane_confidence_daily and area_confidence_daily both landed after
// robot_confidence_daily. On the old check every previously-rolled day was
// already "done", so the two tables the localization board reads stayed empty
// forever while the board rendered geometry with no statistics on it.
//
// Keying done on the plant row makes the backfill fall out for free: those days
// have no plant row either, so they come back as pending and re-roll into all
// four tables. Nothing has to be deleted by hand to trigger it.
func TestCatchUp_RefillsAggregatesAddedAfterAnEarlierRollUp(t *testing.T) {
	db := openWithWindow(t)
	addSegment(t, db, "area-a", "SEG", 0, 0, 10, 0)

	yesterday := testDay.AddDate(0, 0, -1)
	insert(t, db, sample("AMR-01", yesterday.Add(9*time.Hour), 0.90, 5, 0, 1))

	if _, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Reproduce a day rolled up before the newer tables existed: the robot rows
	// survive, the newer aggregates do not.
	for _, table := range []string{"lane_confidence_daily", "plant_confidence_daily"} {
		if _, err := db.Exec(`DELETE FROM `+table+` WHERE day=$1`, yesterday); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}

	if _, err := robotconfidence.CatchUp(db.DB, testDay, 14, rollUpCfg()); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	var rows int
	if err := db.QueryRow(
		`SELECT count(*) FROM lane_confidence_daily WHERE day=$1`, yesterday).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Errorf("lane rows after the backfill pass = %d, want 1 — a table added after the fact never fills", rows)
	}
}
