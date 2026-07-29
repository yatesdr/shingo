package downtime

// SQL shell for the downtime-events data layer (G9). Thin persistence around
// the downtime_events projection + downtime_event_dedup guard + monthly
// partition lifecycle. Mirrors the heartbeat/store.go pattern.

import (
	"database/sql"
	"fmt"
	"regexp"
	"time"
)

// TryDedup records (station, edge_event_id) and reports whether it was NEW
// (true) or a duplicate (false). Called BEFORE projection so a redelivered
// downtime event never double-projects. One UPSERT, no SELECT.
func TryDedup(db *sql.DB, station string, edgeEventID int64) (bool, error) {
	res, err := db.Exec(
		`INSERT INTO downtime_event_dedup (station, edge_event_id) VALUES ($1, $2)
		 ON CONFLICT (station, edge_event_id) DO NOTHING`,
		station, edgeEventID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertEvent appends one downtime event to downtime_events. The target
// month partition must exist (EnsurePartitions runs at boot + daily).
func InsertEvent(db *sql.DB, e DowntimeEvent) error {
	_, err := db.Exec(
		`INSERT INTO downtime_events
		 (station, plc_name, reason, started_at, ended_at, duration_ms, edge_event_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		e.Station, e.PLCName, e.Reason, e.StartedAt, e.EndedAt,
		e.DurationMS, e.EdgeEventID)
	if err != nil {
		return fmt.Errorf("insert downtime_event: %w", err)
	}
	return nil
}

// EnsurePartitions creates the current and next month partitions for
// downtime_events. Called at boot.
func EnsurePartitions(db *sql.DB, now time.Time) error {
	m := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := createMonthPartition(db, m); err != nil {
		return err
	}
	return createMonthPartition(db, m.AddDate(0, 1, 0))
}

// EnsurePartitionsRange creates monthly partitions for every month in [start, end].
// Used by sim startup to pre-create partitions across the fast-forward window.
func EnsurePartitionsRange(db *sql.DB, start, end time.Time) error {
	m := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	endMonth := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !m.After(endMonth) {
		if err := createMonthPartition(db, m); err != nil {
			return err
		}
		m = m.AddDate(0, 1, 0)
	}
	return nil
}

func createMonthPartition(db *sql.DB, month time.Time) error {
	name := partitionName(month)
	start := month
	end := month.AddDate(0, 1, 0)
	// name + bounds are derived from validated date components, not user input.
	_, err := db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF downtime_events FOR VALUES FROM ('%s') TO ('%s')`,
		name, start.Format("2006-01-02"), end.Format("2006-01-02")))
	if err != nil {
		return fmt.Errorf("create partition %s: %w", name, err)
	}
	return nil
}

// partitionName renders a month's partition name.
//
// Aligned with store/heartbeat's `cell_part_events_2026_07` form. This package
// is a near-verbatim copy of store/heartbeat and diverged on exactly this
// string (`downtime_events_y2026m07`), for no reason anyone recorded — the
// kind of drift that makes two copies read as two designs.
func partitionName(month time.Time) string {
	return fmt.Sprintf("downtime_events_%04d_%02d", month.Year(), int(month.Month()))
}

// partitionRe matches both the aligned name and the legacy y%04dm%02d form, so
// partitions created before the rename are still found and pruned rather than
// living forever because they no longer match their own regex.
var partitionRe = regexp.MustCompile(`^downtime_events_(?:y)?(\d{4})_?m?(\d{2})$`)

// DropOldPartitions drops downtime_events partitions whose month ends before
// now-keepDays. Returns the number dropped.
//
// This existed exactly once — in store/heartbeat — and the copy that became
// this package dropped it. So downtime partitions were created monthly and
// never pruned: the table grows one empty partition a month, forever, with
// nothing to remove them.
//
// Mirrors heartbeat.DropOldPartitions. Two instances is below the bar for
// extracting a shared store/eventlog; that extraction is recorded as the job
// for the third instance, not this one.
func DropOldPartitions(db *sql.DB, keepDays int, now time.Time) (int, error) {
	cutoff := now.UTC().AddDate(0, 0, -keepDays)
	rows, err := db.Query(`SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'downtime_events'`)
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	dropped := 0
	for _, n := range names {
		m := partitionRe.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		var y, mo int
		fmt.Sscanf(m[1], "%d", &y)
		fmt.Sscanf(m[2], "%d", &mo)
		monthEnd := time.Date(y, time.Month(mo), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		if monthEnd.Before(cutoff) {
			if _, err := db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, n)); err != nil {
				return dropped, fmt.Errorf("drop partition %s: %w", n, err)
			}
			dropped++
		}
	}
	return dropped, nil
}
