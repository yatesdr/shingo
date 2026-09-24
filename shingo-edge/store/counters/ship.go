package counters

// The production tick feed's reads over counter_snapshots. The table is the
// feed's queue: the poll's INSERT is the enqueue, and the shipper
// (messaging.TickShipper) reads past a cursor and publishes. See
// protocol.SubjectProductionTicks.

import (
	"database/sql"
	"errors"
)

// shippableWhere is the ship filter, the one the poll applied inline before
// the feed moved here: a count moved (delta > 0), not a reset, and the
// reporting point had a style. NULL style_id (a row written before the shipper
// existed) fails `style_id <> 0` and is never selected.
const shippableWhere = `delta > 0 AND (anomaly IS NULL OR anomaly <> 'reset') AND style_id <> 0`

// ShippableTick is one counter_snapshots row projected for the wire.
type ShippableTick struct {
	ID         int64
	ProcessID  int64
	StyleID    int64
	CountValue int64
	Delta      int64
	Anomaly    string
	RecordedMS int64
}

// ListShippable returns up to limit shippable rows with id > afterID, in id
// order. A range read on the primary key.
func ListShippable(db *sql.DB, afterID int64, limit int) ([]ShippableTick, error) {
	rows, err := db.Query(`SELECT id, process_id, style_id, count_value, delta, COALESCE(anomaly, ''), recorded_ms
		FROM counter_snapshots
		WHERE id > ? AND `+shippableWhere+`
		ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShippableTick
	for rows.Next() {
		var t ShippableTick
		if err := rows.Scan(&t.ID, &t.ProcessID, &t.StyleID, &t.CountValue, &t.Delta, &t.Anomaly, &t.RecordedMS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ShipLag counts shippable rows with id > afterID and returns the smallest
// recorded_ms among them (0 when there are none). One statement.
func ShipLag(db *sql.DB, afterID int64) (pending, oldestMS int64, err error) {
	var oldest sql.NullInt64
	err = db.QueryRow(`SELECT COUNT(*), MIN(recorded_ms) FROM counter_snapshots
		WHERE id > ? AND `+shippableWhere, afterID).Scan(&pending, &oldest)
	return pending, oldest.Int64, err
}

// ShipCursor reads the persisted cursor. ok is false when none was written.
func ShipCursor(db *sql.DB) (id int64, ok bool, err error) {
	err = db.QueryRow(`SELECT last_id FROM production_tick_cursor WHERE id = 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// SetShipCursor persists the cursor. One statement.
func SetShipCursor(db *sql.DB, id int64) error {
	_, err := db.Exec(`INSERT INTO production_tick_cursor (id, last_id, updated_at) VALUES (1, ?, datetime('now'))
		ON CONFLICT(id) DO UPDATE SET last_id = excluded.last_id, updated_at = excluded.updated_at`, id)
	return err
}

// InitialShipCursor is the cursor for a shipper that finds none persisted: the
// last row written without recorded_ms, i.e. by a binary from before the
// shipper, which sent its ticks through the outbox. Every row after it was
// written by this binary and has not been shipped. 0 on a table with no such
// rows (a fresh install), which ships everything.
//
// Not MAX(id): the poll loop starts before the shipper does, so rows written
// in that window would be skipped.
func InitialShipCursor(db *sql.DB) (int64, error) {
	var id sql.NullInt64
	err := db.QueryRow(`SELECT MAX(id) FROM counter_snapshots WHERE recorded_ms IS NULL`).Scan(&id)
	return id.Int64, err
}
