package audit

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// bin_uop_delta_daily (v94): the permanent daily roll-up of the raw delta
// stream. Owner decision D3 accepted the growth (~10 rows/day) because this
// table is the "how parts moved" answer once the 90-day retention (D6) starts
// deleting the raw rows — bounding it would remove the only permanent record
// of daily part flow. No retention on this table, ever.
//
// Grain: (day, bin_id, epoch_seq, payload_code, reason, actor). The daily job
// re-aggregates a whole day from the raw rows and upserts, which makes it
// idempotent per day AND self-healing: a missed day is re-derivable from the
// raw rows for as long as they survive (90 days), and the re-run overwrites
// whatever partial state a previous failed attempt left.

// RollupBinUOPDeltaDay aggregates one UTC day of raw bin_uop_delta rows into
// bin_uop_delta_daily. The whole day is deleted-then-reinserted per group via
// ON CONFLICT DO UPDATE, so the call is idempotent and safe to re-run after a
// partial failure. Rows whose raw twins have already aged out of the retention
// window are NOT re-derivable — call it for days still inside the window.
//
// day is the UTC calendar day; the caller (the daily ticker) passes yesterday.
func RollupBinUOPDeltaDay(db *sql.DB, day time.Time) (int64, error) {
	bumps := pgBumpOpsArraySQL()
	res, err := db.Exec(`INSERT INTO bin_uop_delta_daily
		(day, bin_id, epoch_seq, payload_code, reason, actor,
		 ticks, consumed, added, first_uop, last_uop, min_uop, crossings)
	SELECT (w.applied_at AT TIME ZONE 'UTC')::date, w.bin_id, w.epoch_seq,
	       w.payload_code,
	       COALESCE(w.metadata->>'reason', ''), w.actor,
	       count(*)::int,
	       COALESCE(sum(-(w.metadata->>'delta')::int) FILTER (WHERE (w.metadata->>'delta')::int < 0), 0)::int,
	       COALESCE(sum((w.metadata->>'delta')::int)  FILTER (WHERE (w.metadata->>'delta')::int > 0), 0)::int,
	       (array_agg(w.before_uop ORDER BY w.id))[1],
	       (array_agg(w.after_uop ORDER BY w.id))[array_upper(array_agg(w.after_uop ORDER BY w.id), 1)],
	       min(w.after_uop),
	       count(*) FILTER (WHERE w.before_uop >= 0 AND w.after_uop < 0)::int
	FROM (
	    SELECT a.id, a.op, a.bin_id, a.applied_at, a.before_uop, a.after_uop,
	           a.payload_code, a.actor, a.metadata,
	           count(*) FILTER (WHERE a.op = ANY(`+bumps+`)) OVER (
		       PARTITION BY a.bin_id ORDER BY a.id ROWS UNBOUNDED PRECEDING) AS epoch_seq
	    FROM bin_uop_audit a
	    WHERE a.applied_at >= $1 AND a.applied_at < $1 + interval '1 day'
	) w
	WHERE w.op = 'bin_uop_delta'
	GROUP BY 1, 2, 3, 4, 5, 6
	ON CONFLICT (day, bin_id, epoch_seq, payload_code, reason, actor) DO UPDATE SET
		ticks     = EXCLUDED.ticks,
		consumed  = EXCLUDED.consumed,
		added     = EXCLUDED.added,
		first_uop = EXCLUDED.first_uop,
		last_uop  = EXCLUDED.last_uop,
		min_uop   = EXCLUDED.min_uop,
		crossings = EXCLUDED.crossings`,
		day.UTC().Truncate(24*time.Hour))
	if err != nil {
		return 0, fmt.Errorf("rollup bin_uop_delta_daily %s: %w", day.Format("2006-01-02"), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pgBumpOpsArraySQL returns EpochBumpOps as a quoted Postgres array literal —
// the same 8-line spelling store/migrations.go carries for its v93/v94
// backfills (audit cannot import store; store imports audit). The job's
// statement is the migration's v94 backfill verbatim apart from the day filter
// and the ON CONFLICT arm, and the lockstep is pinned by the docker test that
// runs both against the same seeded stream and requires equal rows.
func pgBumpOpsArraySQL() string {
	parts := make([]string, len(EpochBumpOps))
	for i, op := range EpochBumpOps {
		parts[i] = `"` + strings.ReplaceAll(op, `"`, `\"`) + `"`
	}
	return `'{` + strings.Join(parts, ",") + `}'`
}
