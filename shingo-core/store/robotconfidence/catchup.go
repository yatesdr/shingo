package robotconfidence

import (
	"database/sql"
	"fmt"
	"time"
)

// Catch-up: making the roll-up survive a Core that restarts often.
//
// THE BUG THIS EXISTS TO FIX, RECORDED BECAUSE IT LOOKED FINE. The roll-up
// originally hung off a bare `time.NewTicker(24 * time.Hour)` started at boot,
// which is the same shape as the inbox-retention loop beside it and reads as
// obviously correct. It is not, and the reason is external: Springfield's Core
// restarted FIFTEEN TIMES IN SEVEN DAYS — roughly once every eleven hours,
// between deploys, config reloads and reboots. A 24-hour ticker on a process
// with an 11-hour mean life never fires. Not "fires late": never.
//
// The consequence was total rather than partial. robot_confidence_daily and
// segment_confidence_daily would have stayed empty forever while raw samples
// accumulated and then expired at 14 days, so the permanent aggregates — the
// half of this design that cannot be rebuilt once raw is gone — would have
// been silently absent, on a collector that otherwise looked healthy.
//
// The fix is to stop treating "has 24 hours elapsed in THIS process" as the
// question. The real question is "is there a completed day with samples and no
// aggregates", which is answered from the database and is therefore immune to
// how often the process dies. A short ticker plus a boot pass then converges on
// it regardless of restart cadence, and the idempotent upserts make a redundant
// pass free.
//
// This is also what makes the job self-healing: a day missed for any other
// reason — the database being down, a bad snap tolerance, a crash mid-run —
// is picked up on the next pass rather than needing a human to notice.

// PendingDays returns the completed days inside the retention window that have
// raw samples but no roll-up rows yet, oldest first.
//
// Today is deliberately excluded: it is still accumulating, and a partial day
// written as though it were complete would be indistinguishable from a real
// one after the fact.
//
// The window is bounded by retentionDays because a day whose raw partition has
// already been dropped can never be rolled up — there is nothing left to read.
// Looking further back would just re-ask an unanswerable question every tick.
//
// DONE IS THE PLANT ROW, NOT THE ROBOT ROW, AND THE DIFFERENCE IS A BUG THIS
// FIXES. RollUp writes FOUR tables — robot, lane, area, plant — and this check
// used to ask only whether the ROBOT rows existed. That is one tickbox for four
// forms, and it fails in two directions:
//
//   - A run that wrote the robot rows and then died — a bad scene, a dropped
//     connection, an unreadable partition — left the day marked done with its
//     lane and zone rows missing, permanently.
//   - Worse, adding a new aggregate table marks EVERY past day done on arrival.
//     lane_confidence_daily and area_confidence_daily both landed after
//     robot_confidence_daily (migration 77), so every day already rolled up was
//     skipped forever while the two tables the localization board actually reads
//     stayed empty.
//
// Either way the raw rows behind the gap expire at RawRetentionDays and the
// aggregates can never be rebuilt — the exact loss this whole file exists to
// prevent, arriving through the completion check rather than through the ticker.
//
// plant_confidence_daily is the right marker because of WHERE RollUp writes it:
// last, and unconditionally, as the record of what the run did (rollup.go). So
// its presence means all four writes landed and its absence means they did not.
// Checking all four tables instead would be wrong in the other direction — a day
// with no lane-snapped samples legitimately writes no lane rows, and would then
// be re-rolled on every tick forever, which is the failure the sample check
// below already exists to avoid.
//
// This is also the backfill: a day rolled up before the plant table existed has
// no plant row, so it becomes pending again and re-rolls into all four tables on
// the next pass. The upserts are idempotent, so that costs a redundant read and
// nothing else, and no historical row has to be deleted to trigger it.
func PendingDays(db *sql.DB, now time.Time, retentionDays int) ([]time.Time, error) {
	today := dayKey(now)
	var out []time.Time
	for i := retentionDays; i >= 1; i-- {
		day := today.AddDate(0, 0, -i)

		var done bool
		if err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM plant_confidence_daily WHERE day = $1)`, day).
			Scan(&done); err != nil {
			return nil, fmt.Errorf("check rolled-up day %s: %w", day.Format("2006-01-02"), err)
		}
		if done {
			continue
		}

		// Only days that actually have raw samples. Without this a fresh
		// install would try to roll up every day of its retention window on
		// every tick, forever, finding nothing each time.
		var has bool
		if err := db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM `+TableSamples+`
			                WHERE sampled_at >= $1 AND sampled_at < $2)`,
			day, day.AddDate(0, 0, 1)).Scan(&has); err != nil {
			return nil, fmt.Errorf("check samples for %s: %w", day.Format("2006-01-02"), err)
		}
		if has {
			out = append(out, day)
		}
	}
	return out, nil
}

// CatchUp rolls up every pending day, oldest first, and returns what it did.
//
// Oldest first matters: each day's residual is measured against a trailing
// baseline drawn from the days around it, so processing in order means a
// backlog resolves with the same baselines it would have had if nothing had
// been missed.
//
// A failure on one day does not abandon the rest — a single unreadable day
// (a corrupt partition, a scene that will not load) should not block every
// later day behind it forever. The error is returned alongside the successful
// results so the caller can log both.
func CatchUp(db *sql.DB, now time.Time, retentionDays int, cfg RollUpConfig) ([]RollUpResult, error) {
	days, err := PendingDays(db, now, retentionDays)
	if err != nil {
		return nil, err
	}
	var out []RollUpResult
	var firstErr error
	for _, day := range days {
		res, err := RollUp(db, day, cfg)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("roll up %s: %w", day.Format("2006-01-02"), err)
			}
			continue
		}
		out = append(out, res)
	}
	return out, firstErr
}
