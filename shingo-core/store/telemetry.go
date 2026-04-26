package store

// Phase 5 delegate file: mission events + telemetry persistence lives in
// store/telemetry/. This file preserves the *store.DB method surface so
// external callers don't need to change.

import "shingocore/store/telemetry"

func (db *DB) InsertMissionEvent(e *telemetry.Event) error { return telemetry.InsertEvent(db.DB, e) }

func (db *DB) ListMissionEvents(orderID int64) ([]*telemetry.Event, error) {
	return telemetry.ListEvents(db.DB, orderID)
}

func (db *DB) UpsertMissionTelemetry(t *telemetry.Mission) error {
	return telemetry.UpsertMission(db.DB, t)
}

func (db *DB) GetMissionTelemetry(orderID int64) (*telemetry.Mission, error) {
	return telemetry.GetMission(db.DB, orderID)
}

func (db *DB) ListMissions(f telemetry.Filter) ([]*telemetry.Mission, int, error) {
	return telemetry.ListMissions(db.DB, f)
}

func (db *DB) GetMissionStats(f telemetry.Filter) (*telemetry.Stats, error) {
	return telemetry.GetStats(db.DB, f)
}
