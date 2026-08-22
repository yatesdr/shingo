package service

import (
	"time"

	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/orders"
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

// DwellStats returns p50/p95/count for each requested order_history
// transition over [start, end] — the per-state dwell answer the missions page
// needs: time-to-dispatch, transit, staged dwell, operator fill.
//
// Reads order_history, NOT mission_telemetry: 76.6% of terminal orders have no
// mission_telemetry row (it is written only on a vendor terminal, so every
// skip and most cancels never produce one). Dwell is a question about what
// happened to the ORDERS, which is what order_history answers.
//
// Pass nil pairs for the standard set. payloadCode / orderType "" mean "all".
func (s *MissionService) DwellStats(pairs []domain.DwellPair, payloadCode, orderType string, start, end time.Time) ([]domain.DwellStat, error) {
	if len(pairs) == 0 {
		pairs = domain.FlowDwellPairs()
	}
	return orders.DwellStats(s.db.DB, pairs, payloadCode, orderType,
		orders.LeadTimeRange{Start: start, End: end})
}

// FaultStats returns the /missions Faults card over [start, end].
//
// NOT DwellStats pairs. DwellStats measures an order's OUTERMOST faulted→X span,
// so an order that faulted twice would land in two outcome buckets and its
// recovery dwell would span a fault it had already recovered from. The card
// needs each faulted row paired with what actually followed it, which is a LEAD
// window — see orders.GetFaultStats.
//
// noticeAfter is the config threshold. It is passed in rather than read here so
// the card's split and every other fault surface use the same number.
func (s *MissionService) FaultStats(start, end time.Time, noticeAfter time.Duration) (*orders.FaultStats, error) {
	return s.db.GetFaultStats(orders.LeadTimeRange{Start: start, End: end}, noticeAfter)
}

// Breakdown returns the top-10 mission groups by robot or route (plan §3.F).
func (s *MissionService) Breakdown(f telemetry.Filter, by string) ([]telemetry.BreakdownRow, error) {
	return s.db.GetMissionBreakdown(f, by)
}

// BreakdownByRobot returns the by-robot breakdown with U3's route index attached,
// and reports whether ANY route cleared minRouteSamples.
//
// The bool is not a nicety. "No route had enough missions to be a denominator"
// and "this robot ran on none of the routes that did" are different facts and get
// different UI — the first drops the column, the second dashes one cell — and
// neither can be inferred from a nil index. A caller handed only the rows would
// have to guess, and the guess it would make is the absence-as-value one.
func (s *MissionService) BreakdownByRobot(f telemetry.Filter, minRouteSamples int) ([]telemetry.BreakdownRow, bool, error) {
	rows, err := s.db.GetMissionBreakdown(f, "robot")
	if err != nil {
		return nil, false, err
	}
	idx, qualifyingRoutes, err := s.db.GetRobotRouteIndex(f, minRouteSamples)
	if err != nil {
		return nil, false, err
	}
	for i := range rows {
		if ri, ok := idx[rows[i].Label]; ok {
			v := ri.Index
			rows[i].RouteIndex = &v
			rows[i].IndexSamples = ri.Samples
		}
		// else: RouteIndex stays nil. Not zero — see the field's comment.
	}
	return rows, qualifyingRoutes > 0, nil
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
