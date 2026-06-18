package service

import (
	"time"

	"shingocore/store"
	"shingocore/store/telemetry"
)

// MissionService centralizes mission telemetry and statistics queries.
// Handlers call MissionService instead of reaching through engine
// passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today.
type MissionService struct {
	db *store.DB
}

func NewMissionService(db *store.DB) *MissionService {
	return &MissionService{db: db}
}

// Stats returns summary counters across missions matching the filter.
func (s *MissionService) Stats(f telemetry.Filter) (*telemetry.Stats, error) {
	return s.db.GetMissionStats(f)
}

// StatsV2 returns the corrected dashboard mission stats (plan §3.A / §8 #5):
// success_rate = Confirmed/(Confirmed+Failed), cancelled + skipped excluded,
// system stops counted as failures.
func (s *MissionService) StatsV2(f telemetry.Filter) (*telemetry.StatsV2, error) {
	return s.db.GetMissionStatsV2(f)
}

// Timeseries returns mission metrics bucketed by hour or day for the trend
// charts (plan §3.B / §15.B).
func (s *MissionService) Timeseries(f telemetry.Filter, bucket string) ([]telemetry.Bucket, error) {
	return s.db.GetMissionTimeseries(f, bucket)
}

// RobotMissionAggs returns per-robot mission count + busy time over the
// window for the Robot Fleet section's utilization bars (plan §15.C).
func (s *MissionService) RobotMissionAggs(f telemetry.Filter) ([]telemetry.RobotMissionAgg, error) {
	return s.db.GetRobotMissionAggs(f)
}

// HourlyConcurrency returns 24 hourly fleet-concurrency points for the Fleet
// Load chart's single-day (Today) view (plan §15.C).
func (s *MissionService) HourlyConcurrency(dayStart time.Time, stationID string) ([]telemetry.HourConcurrency, error) {
	return s.db.GetHourlyConcurrency(dayStart, stationID)
}

// DailyConcurrency returns per-day peak/avg fleet concurrency over [since,
// until] for the Fleet Load chart's multi-day (7d/30d) view (plan §15.C).
func (s *MissionService) DailyConcurrency(since, until time.Time, stationID string) ([]telemetry.DayConcurrency, error) {
	return s.db.GetDailyConcurrency(since, until, stationID)
}

// Breakdown returns the top-10 mission groups by robot or route (plan §3.F).
func (s *MissionService) Breakdown(f telemetry.Filter, by string) ([]telemetry.BreakdownRow, error) {
	return s.db.GetMissionBreakdown(f, by)
}

// Failures returns the classified failure-reason Pareto (plan §3.G).
func (s *MissionService) Failures(f telemetry.Filter) ([]telemetry.FailureReason, error) {
	return s.db.GetMissionFailures(f)
}

// Telemetry returns the latest telemetry snapshot for a single
// mission (keyed by order ID).
func (s *MissionService) Telemetry(orderID int64) (*telemetry.Mission, error) {
	return s.db.GetMissionTelemetry(orderID)
}

// ListEvents returns the event timeline for a single mission.
func (s *MissionService) ListEvents(orderID int64) ([]*telemetry.Event, error) {
	return s.db.ListMissionEvents(orderID)
}

// List returns telemetry for every mission matching the filter along
// with a total row count (for pagination).
func (s *MissionService) List(f telemetry.Filter) ([]*telemetry.Mission, int, error) {
	return s.db.ListMissions(f)
}
