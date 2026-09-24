package store

// Phase 5b delegate file: counter_snapshots CRUD now lives in
// store/counters/. This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/counters"

// InsertCounterSnapshot writes one counter_snapshots row, the tick's stamp
// included.
func (db *DB) InsertCounterSnapshot(rpID int64, countValue, delta int64, anomaly string, stamp counters.TickStamp) (int64, error) {
	return counters.InsertSnapshot(db.DB, rpID, countValue, delta, anomaly, stamp)
}

// ListShippableTicks returns up to limit production-tick rows past afterID,
// in id order.
func (db *DB) ListShippableTicks(afterID int64, limit int) ([]counters.ShippableTick, error) {
	return counters.ListShippable(db.DB, afterID, limit)
}

// ProductionTickLag counts shippable rows past afterID and returns the oldest
// one's recorded_ms (0 when there are none).
func (db *DB) ProductionTickLag(afterID int64) (pending, oldestMS int64, err error) {
	return counters.ShipLag(db.DB, afterID)
}

// ProductionTickCursor reads the shipper's persisted cursor; ok is false when
// none has been written.
func (db *DB) ProductionTickCursor() (id int64, ok bool, err error) {
	return counters.ShipCursor(db.DB)
}

// SetProductionTickCursor persists the shipper's cursor.
func (db *DB) SetProductionTickCursor(id int64) error {
	return counters.SetShipCursor(db.DB, id)
}

// InitialProductionTickCursor is where a shipper with no persisted cursor
// starts: after the last row written before the shipper existed.
func (db *DB) InitialProductionTickCursor() (int64, error) {
	return counters.InitialShipCursor(db.DB)
}
