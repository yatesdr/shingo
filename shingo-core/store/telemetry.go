package store

// Phase 5 delegate file: mission events + telemetry persistence lives in
// store/telemetry/. This file preserves the *store.DB method surface so
// external callers don't need to change.

import (
	"shingocore/store/telemetry"
)

// Type aliases preserve the store.MissionEvent / MissionTelemetry / MissionFilter / MissionStats public API.
type MissionEvent = telemetry.Event
type MissionTelemetry = telemetry.Mission
type MissionFilter = telemetry.Filter
type MissionStats = telemetry.Stats

func (db *DB) InsertMissionEvent(e *MissionEvent) error { return telemetry.InsertEvent(db.DB, e) }

func (db *DB) ListMissionEvents(orderID int64) ([]*MissionEvent, error) {
	return telemetry.ListEvents(db.DB, orderID)
}

func (db *DB) UpsertMissionTelemetry(t *MissionTelemetry) error {
	return telemetry.UpsertMission(db.DB, t)
}

func (db *DB) GetMissionTelemetry(orderID int64) (*MissionTelemetry, error) {
	return telemetry.GetMission(db.DB, orderID)
}

func (db *DB) ListMissions(f MissionFilter) ([]*MissionTelemetry, int, error) {
	return telemetry.ListMissions(db.DB, f)
}

func (db *DB) GetMissionStats(f MissionFilter) (*MissionStats, error) {
	return telemetry.GetStats(db.DB, f)
}
