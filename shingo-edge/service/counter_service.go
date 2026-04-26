package service

import (
	"shingoedge/store"
	"shingoedge/store/counters"
)

// CounterService owns the counters aggregate's surface: reporting
// points (PLC tag → style mappings), counter snapshots and anomalies
// produced by polling those tags, and hourly counts aggregated for
// production reporting. These three concepts share the same data
// flow (PLC → reporting point → snapshot → hourly count) and are
// grouped at the store level under store/counters/.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
// State-mutation paths (anomaly confirmation, hourly upsert) are
// included where handlers reach them; pure-internal counter polling
// loops in the engine still call *store.DB directly until Phase 6.4.
type CounterService struct {
	db *store.DB
}

// NewCounterService constructs a CounterService wrapping the shared
// *store.DB.
func NewCounterService(db *store.DB) *CounterService {
	return &CounterService{db: db}
}

// ── Reporting points ─────────────────────────────────────────────

// ListReportingPoints returns all reporting_points ordered by id.
func (s *CounterService) ListReportingPoints() ([]counters.ReportingPoint, error) {
	return s.db.ListReportingPoints()
}

// GetReportingPoint returns one reporting_point by id.
func (s *CounterService) GetReportingPoint(id int64) (*counters.ReportingPoint, error) {
	return s.db.GetReportingPoint(id)
}

// CreateReportingPoint inserts a new reporting_point and returns its
// row id.
func (s *CounterService) CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error) {
	return s.db.CreateReportingPoint(plcName, tagName, styleID)
}

// UpdateReportingPoint modifies an existing reporting_point.
func (s *CounterService) UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error {
	return s.db.UpdateReportingPoint(id, plcName, tagName, styleID, enabled)
}

// DeleteReportingPoint removes a reporting_point row by id.
func (s *CounterService) DeleteReportingPoint(id int64) error {
	return s.db.DeleteReportingPoint(id)
}

// ── Anomalies ────────────────────────────────────────────────────

// ListUnconfirmedAnomalies returns counter_snapshots rows whose
// anomaly column is set and operator_confirmed = 0. Used by the
// production HMI to surface counter gaps for operator review.
func (s *CounterService) ListUnconfirmedAnomalies() ([]counters.Snapshot, error) {
	return s.db.ListUnconfirmedAnomalies()
}

// ConfirmAnomaly flips a snapshot's operator_confirmed flag, removing
// it from the unconfirmed list.
func (s *CounterService) ConfirmAnomaly(id int64) error {
	return s.db.ConfirmAnomaly(id)
}

// DismissAnomaly clears the anomaly column on a snapshot. Used when
// an apparent anomaly was a false positive (e.g., a counter rollover
// that the polling logic mis-classified).
func (s *CounterService) DismissAnomaly(id int64) error {
	return s.db.DismissAnomaly(id)
}

// ── Hourly counts ────────────────────────────────────────────────

// ListHourlyCounts returns hourly_counts rows for one (process,
// style, date) tuple.
func (s *CounterService) ListHourlyCounts(processID, styleID int64, countDate string) ([]counters.HourlyCount, error) {
	return s.db.ListHourlyCounts(processID, styleID, countDate)
}

// HourlyTotals returns hour-bucketed totals for one (process, date)
// tuple, summed across all styles. Used by the production view.
func (s *CounterService) HourlyTotals(processID int64, countDate string) (map[int]int64, error) {
	return s.db.HourlyCountTotals(processID, countDate)
}
