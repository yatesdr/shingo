package heartbeat

// SQL shell for the production-heartbeat data layer (plan §12). Thin
// persistence around the cell_part_events projection, cell_targets and the
// monthly partition lifecycle. All analytical math is in heartbeat.go's pure
// functions; this file only reads/writes.

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// partEventCols is the INSERT column list's width; InsertPartEvents binds this
// many parameters per row.
const partEventCols = 8

// RejectedTick is a tick the database refused on its own — typically a
// recorded_at outside every partition, from an Edge with a wrong clock.
type RejectedTick struct {
	Event PartEvent
	Err   error
}

// InsertPartEvents projects ticks into cell_part_events in ONE statement and
// returns the rows it actually inserted.
//
// ONLY WHEN THAT STATEMENT FAILS it retries the page row by row, so one bad
// tick does not take the good ones with it: the Edge moved its cursor when the
// publish succeeded and will not send the page again. The rows the database
// refuses one at a time come back as rejected. The steady state stays one
// statement; the fallback costs one per row, and only on a failed page.
//
// The dedup key is the table's own unique index,
// (cell_id, edge_snapshot_id, recorded_at): a re-sent tick carries the
// recorded_at read back from the Edge's snapshot row, so it conflicts and is
// skipped; a restored Edge that reuses an id carries a new recorded_at, so it
// is a new row. (cell_id, edge_snapshot_id) alone would drop that row, and
// edge_snapshot_id alone would merge stations (Edge ids collide across
// stations, §8 #22). The partition for each recorded_at must exist
// (EnsurePartitions runs at boot and daily).
func InsertPartEvents(db *sql.DB, events []PartEvent) (inserted []PartEvent, rejected []RejectedTick) {
	if len(events) == 0 {
		return nil, nil
	}
	inserted, err := insertPartEventRows(db, events)
	if err == nil {
		return inserted, nil
	}
	if len(events) == 1 {
		return nil, []RejectedTick{{Event: events[0], Err: err}}
	}
	for _, e := range events {
		got, err := insertPartEventRows(db, []PartEvent{e})
		if err != nil {
			rejected = append(rejected, RejectedTick{Event: e, Err: err})
			continue
		}
		inserted = append(inserted, got...)
	}
	return inserted, rejected
}

// insertPartEventRows is the one multi-row INSERT ... ON CONFLICT DO NOTHING
// RETURNING.
func insertPartEventRows(db *sql.DB, events []PartEvent) ([]PartEvent, error) {
	var q strings.Builder
	q.WriteString(`INSERT INTO cell_part_events
		(cell_id, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id) VALUES `)
	args := make([]any, 0, len(events)*partEventCols)
	for i, e := range events {
		if i > 0 {
			q.WriteString(",")
		}
		n := i * partEventCols
		fmt.Fprintf(&q, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8)
		args = append(args, e.CellID, e.RecordedAt, e.EdgeSnapshotID, e.CountValue, e.Delta, e.Anomaly, e.ProcessID, e.StyleID)
	}
	q.WriteString(` ON CONFLICT (cell_id, edge_snapshot_id, recorded_at) DO NOTHING
		RETURNING id, cell_id, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id`)
	rows, err := db.Query(q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("insert cell_part_events: %w", err)
	}
	defer rows.Close()
	var out []PartEvent
	for rows.Next() {
		var e PartEvent
		if err := rows.Scan(&e.ID, &e.CellID, &e.RecordedAt, &e.EdgeSnapshotID,
			&e.CountValue, &e.Delta, &e.Anomaly, &e.ProcessID, &e.StyleID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEvents returns events for a cell in [since, until], ascending. Backs the
// analytical queries and the run/stop strip (composite index (cell_id, recorded_at)).
func ListEvents(db *sql.DB, cellID string, since, until time.Time) ([]PartEvent, error) {
	rows, err := db.Query(
		`SELECT id, cell_id, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id
		 FROM cell_part_events
		 WHERE cell_id=$1 AND recorded_at >= $2 AND recorded_at <= $3
		 ORDER BY recorded_at`, cellID, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartEvent
	for rows.Next() {
		var e PartEvent
		if err := rows.Scan(&e.ID, &e.CellID, &e.RecordedAt, &e.EdgeSnapshotID,
			&e.CountValue, &e.Delta, &e.Anomaly, &e.ProcessID, &e.StyleID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetTarget returns the configured target cycle for a cell/payload, falling
// back to the cell-level default (payload_code=”) and then (0, false).
func GetTarget(db *sql.DB, cellID, payloadCode string) (time.Duration, bool) {
	var ms int64
	err := db.QueryRow(`SELECT target_cycle_ms FROM cell_targets WHERE cell_id=$1 AND payload_code=$2`,
		cellID, payloadCode).Scan(&ms)
	if err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond, true
	}
	if payloadCode != "" {
		return GetTarget(db, cellID, "")
	}
	return 0, false
}

// ── Partition lifecycle (plan §12: from-day-one monthly partitions) ─────────

// EnsurePartitions creates the partitions for the month of ref and the next
// month if absent. Run at boot and daily so INSERTs never fail at a month
// boundary. Idempotent (CREATE TABLE IF NOT EXISTS).
func EnsurePartitions(db *sql.DB, ref time.Time) error {
	for _, m := range []time.Time{ref, ref.AddDate(0, 1, 0)} {
		if err := createMonthPartition(db, m); err != nil {
			return err
		}
	}
	return nil
}

// EnsurePartitionsRange creates partitions for every month in [start, end].
// Used by sim startup to pre-create the full fast-forward window so INSERTs
// never fail during catch-up. Idempotent.
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

func createMonthPartition(db *sql.DB, m time.Time) error {
	start := time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	name := partitionName(start)
	// name + bounds are derived from validated date components, not user input.
	q := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF cell_part_events FOR VALUES FROM ('%s') TO ('%s')`,
		name, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if _, err := db.Exec(q); err != nil {
		return fmt.Errorf("create partition %s: %w", name, err)
	}
	return nil
}

func partitionName(monthStart time.Time) string {
	return fmt.Sprintf("cell_part_events_%04d_%02d", monthStart.Year(), int(monthStart.Month()))
}

var partitionRe = regexp.MustCompile(`^cell_part_events_(\d{4})_(\d{2})$`)

// DropOldPartitions drops cell_part_events partitions whose month ends before
// now-keepDays (plan §12: 90-day retention via DROP TABLE on old partitions —
// O(1) vs a DELETE scan). Returns the number dropped.
func DropOldPartitions(db *sql.DB, keepDays int, now time.Time) (int, error) {
	cutoff := now.UTC().AddDate(0, 0, -keepDays)
	rows, err := db.Query(`SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'cell_part_events'`)
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
