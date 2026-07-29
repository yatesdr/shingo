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
	db           *store.DB
	deltaEmitter CounterDeltaEmitter
}

// CounterDeltaEmitter is the one thing this service needs from the
// engine: the ability to put a counter delta on the event bus. The
// signature is plc.EventEmitter's EmitCounterDelta verbatim, so the
// engine's existing plcEmitter satisfies it with no adapter — a
// confirmed jump therefore travels the identical path a normal tick
// travels, rather than a second, drift-prone one.
type CounterDeltaEmitter interface {
	EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string)
}

// NewCounterService constructs a CounterService wrapping the shared
// *store.DB.
func NewCounterService(db *store.DB) *CounterService {
	return &CounterService{db: db}
}

// SetDeltaEmitter installs the sink ConfirmAnomaly releases a confirmed
// jump's delta into. Called by Engine.Start once the emitter adapters
// exist; the service package cannot import engine. Nil (the zero value,
// and what test fixtures building a bare service get) means confirmation
// still flips the flag and simply emits nothing.
func (s *CounterService) SetDeltaEmitter(e CounterDeltaEmitter) {
	s.deltaEmitter = e
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

// ConfirmAnomaly accepts a jump: it flips operator_confirmed, removing
// the row from the popover, AND releases the delta downstream.
//
// The release is the point. plc/manager.go's poll withholds the delta
// for a jump — `if anomaly != "jump" && delta > 0` — under a comment
// saying jumps "need operator confirmation", and enqueueProductionTick's
// comment states that "inventory attribution is operator-gated". The gate
// was built; the release was never wired. Confirmation was a bare UPDATE
// that emitted nothing, so a confirmed jump's units reached neither
// hourly_counts nor the UOP path — not before confirmation and not after.
// On the Springfield dump that is 14,532 units behind five CONFIRMED
// jumps, permanently absent from the production record, and it is why
// hourly_counts holds only 91.4% of observed production.
//
// Emitting the same EventCounterDelta the poll emits means both consumers
// wired in engine/wiring.go — HourlyTracker.HandleDelta and
// handleCounterDelta — see it, which is what "operator-gated" was always
// meant to unblock. anomaly is passed through as "jump": neither consumer
// filters on it (both skip only "reset"), and preserving it keeps the
// event honest about where the units came from.
//
// Nothing is emitted when the row did not move (a re-tapped Confirm, a
// non-jump id) or when the reporting point has no style — the same
// StyleID == 0 early return the poll path takes. A jump always carries
// delta > jumpThreshold > 0 (plc.CalculateDelta), so there is no
// delta > 0 guard to mirror.
//
// eventbus.Bus.Emit is SYNCHRONOUS, so the confirm request runs the hourly
// upsert and the UOP arithmetic inline before it answers. That is the same
// bargain plc.pollReportingPoint already makes once a second, and Emit
// wraps each subscriber in its own recover(), so a panic downstream
// degrades to a log line rather than a failed confirmation.
//
// No production.tick is enqueued here. enqueueProductionTick already fires
// for jumps at poll time (`delta > 0 && anomaly != "reset"`) precisely
// because Core's heartbeat must see that the cell physically fired while
// attribution is gated. Re-enqueuing on confirm would double-count on
// Core — which is also why the dedup guard is keyed on the Edge snapshot
// id and not on anything about the confirmation.
func (s *CounterService) ConfirmAnomaly(id int64) error {
	confirmed, err := s.db.ConfirmAnomaly(id)
	if err != nil {
		return err
	}
	if confirmed == nil || s.deltaEmitter == nil || confirmed.StyleID == 0 {
		return nil
	}
	s.deltaEmitter.EmitCounterDelta(
		confirmed.ReportingPointID, confirmed.ProcessID, confirmed.StyleID,
		confirmed.Delta, confirmed.CountValue, "jump")
	return nil
}

// DismissAnomaly DELETES the snapshot row. Used when an apparent anomaly
// was a false positive (e.g., a counter rollover that the polling logic
// mis-classified) — the units are declared not to have happened, so the
// observation goes away rather than being kept with the anomaly cleared.
//
// This comment used to say it "clears the anomaly column on a snapshot",
// which it has never done: store/counters.DismissAnomaly runs
// `DELETE FROM counter_snapshots WHERE id = ? AND anomaly = 'jump' AND
// operator_confirmed = 0`. The distinction matters to the retention
// purge, whose predicate preserves exactly the rows this can still act on.
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

// ── Daily counts ─────────────────────────────────────────────────

// DailyCounts returns per-style day totals for one process over an
// inclusive date range.
//
// This is where production goes once counters.PurgeRolledUpHourly has
// taken the hours underneath it. Deleting detail is only defensible if
// the summary is reachable, so this read and its handler
// (www.apiGetDailyCounts) ship with the purge rather than after it.
//
// It goes straight to store/counters rather than through a *store.DB
// delegate, per the method-surface convention at the top of store/store.go.
//
// The empty case is normalised HERE, not in the handler. A nil slice marshals
// to `null` and every JS caller iterates the result, but www may not import
// store/counters at all — the depguard rule "www handlers must use a service
// interface, not *store.DB directly" makes the handler unable to name the
// element type, so this is the only layer that can spell the empty value.
func (s *CounterService) DailyCounts(processID int64, fromDate, toDate string) ([]counters.DailyCount, error) {
	out, err := counters.ListDaily(s.db.DB, processID, fromDate, toDate)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []counters.DailyCount{}
	}
	return out, nil
}
