package service

import (
	"time"

	"shingoedge/store"
	"shingoedge/store/counters"
)

// CounterService owns the counters aggregate's surface: reporting
// points (PLC tag → style mappings), counter snapshots produced by
// polling those tags, and hourly counts aggregated for production
// reporting. These three concepts share the same data flow
// (PLC → reporting point → snapshot → hourly count) and are grouped at
// the store level under store/counters/.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
// Pure-internal counter polling loops in the engine still call
// *store.DB directly until Phase 6.4.
//
// There is no anomaly confirmation any more (close-out 2b): a jump or a
// reset is counted at the poll like any delta, so nothing waits on an
// operator.
type CounterService struct {
	db *store.DB
	// loc is the plant's reporting zone. Counts are STORED in UTC hour
	// buckets, so this is used only to turn a plant-local date into the UTC
	// range that covers it, and to label the hours coming back. Nil means UTC,
	// which is what an unconfigured edge reports — visibly wrong rather than
	// plausibly wrong. Captured at construction, like every other consumer of
	// cfg.Timezone, so a change takes effect on restart and the whole process
	// agrees about which clock it is on.
	loc *time.Location
}

// NewCounterService constructs a CounterService wrapping the shared
// *store.DB. loc is the plant reporting zone; nil means UTC.
func NewCounterService(db *store.DB, loc *time.Location) *CounterService {
	if loc == nil {
		loc = time.UTC
	}
	return &CounterService{db: db, loc: loc}
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

// ── Hourly counts ────────────────────────────────────────────────

// ListHourlyCounts returns the hourly rows for one (process, style) whose UTC
// buckets fall inside the given PLANT-LOCAL calendar date.
func (s *CounterService) ListHourlyCounts(processID, styleID int64, countDate string) ([]counters.HourlyCount, error) {
	from, to, err := counters.DayBounds(countDate, s.loc)
	if err != nil {
		return nil, err
	}
	return s.db.ListHourlyCounts(processID, styleID, from, to)
}

// HourlyTotals returns totals for one (process, PLANT-LOCAL date) keyed by the
// plant-local HOUR OF DAY, summed across styles. Used by the production view,
// which renders them against shift boundaries.
//
// The map key is a local hour, not a bucket: the store returns UTC buckets and
// this is the one place they become the operator's clock. On the autumn DST
// day two distinct UTC buckets map to the same local hour, so the totals are
// SUMMED rather than assigned — that repeated hour genuinely did happen twice,
// and a 25-hour day is the honest shape of it. The old local-keyed schema had
// no way to say that: both writes collided on one row and were summed by the
// database, which looked identical and was not, because it also silently
// merged them for every other purpose.
func (s *CounterService) HourlyTotals(processID int64, countDate string) (map[int]int64, error) {
	from, to, err := counters.DayBounds(countDate, s.loc)
	if err != nil {
		return nil, err
	}
	buckets, err := s.db.HourlyCountTotals(processID, from, to)
	if err != nil {
		return nil, err
	}
	totals := make(map[int]int64, len(buckets))
	for bucket, sum := range buckets {
		totals[time.Unix(bucket, 0).In(s.loc).Hour()] += sum
	}
	return totals, nil
}
