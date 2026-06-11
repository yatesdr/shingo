package downtime

// SQL shell for the downtime-events data layer (G9). Thin persistence around
// the downtime_events projection + downtime_event_dedup guard + monthly
// partition lifecycle. Mirrors the heartbeat/store.go pattern.

import (
	"database/sql"
	"fmt"
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
	name := fmt.Sprintf("downtime_events_y%04dm%02d", month.Year(), month.Month())
	start := month
	end := month.AddDate(0, 1, 0)
	_, err := db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF downtime_events FOR VALUES FROM ('%s') TO ('%s')`,
		name, start.Format("2006-01-02"), end.Format("2006-01-02")))
	return err
}
