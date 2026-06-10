package store

// Delegate file: downtime-event persistence lives in store/downtime/.
// Preserves the *store.DB method surface for the projection worker
// (messaging) and future read services (OEE availability).

import (
	"time"

	"shingocore/store/downtime"
)

func (db *DB) TryDowntimeEventDedup(station string, edgeEventID int64) (bool, error) {
	return downtime.TryDedup(db.DB, station, edgeEventID)
}

func (db *DB) InsertDowntimeEvent(e downtime.DowntimeEvent) error {
	return downtime.InsertEvent(db.DB, e)
}

func (db *DB) EnsureDowntimePartitions(ref time.Time) error {
	return downtime.EnsurePartitions(db.DB, ref)
}

func (db *DB) EnsureDowntimePartitionsRange(start, end time.Time) error {
	return downtime.EnsurePartitionsRange(db.DB, start, end)
}
