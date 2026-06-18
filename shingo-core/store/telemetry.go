package store

// Phase 5 delegate file: mission events + telemetry persistence lives in
// store/telemetry/. This file preserves the *store.DB method surface so
// external callers don't need to change.

import (
	"time"

	"shingocore/store/telemetry"
)

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

func (db *DB) GetMissionStatsV2(f telemetry.Filter) (*telemetry.StatsV2, error) {
	return telemetry.GetStatsV2(db.DB, f)
}

func (db *DB) GetMissionTimeseries(f telemetry.Filter, bucket string) ([]telemetry.Bucket, error) {
	return telemetry.GetTimeseries(db.DB, f, bucket)
}

func (db *DB) GetRobotMissionAggs(f telemetry.Filter) ([]telemetry.RobotMissionAgg, error) {
	return telemetry.GetRobotMissionAggs(db.DB, f)
}

func (db *DB) GetHourlyConcurrency(dayStart time.Time, stationID string) ([]telemetry.HourConcurrency, error) {
	return telemetry.GetHourlyConcurrency(db.DB, dayStart, stationID)
}

func (db *DB) GetDailyConcurrency(since, until time.Time, stationID string) ([]telemetry.DayConcurrency, error) {
	return telemetry.GetDailyConcurrency(db.DB, since, until, stationID)
}

func (db *DB) GetMissionBreakdown(f telemetry.Filter, by string) ([]telemetry.BreakdownRow, error) {
	return telemetry.GetBreakdown(db.DB, f, by)
}

func (db *DB) GetMissionFailures(f telemetry.Filter) ([]telemetry.FailureReason, error) {
	return telemetry.GetFailures(db.DB, f)
}
