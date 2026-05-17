// threshold_calculator.go — unified calculator for the two
// UOP-threshold values surfaced by the replenishment UI. See
// docs/uop-threshold-replenishment.md for the design overview.
//
//   INPUTS:
//     cycle_seconds          peak cycle time
//     l1_queue_seconds       mean wait in queued
//     l1_transit_seconds     mean L1 robot transit
//     l2_load_seconds        mean operator fill time
//     l2_transit_seconds     mean L2 robot transit
//     market_to_cell_seconds p95 retrieve duration to cell
//     safety_factor          per-claim engineer-set, default 1.5
//
//   OUTPUTS:
//     l1_threshold = ceil(((l1_queue + l1_transit + l2_load + l2_transit)
//                          / cycle_seconds) * safety)
//     cell_reorder = ceil((market_to_cell / cycle_seconds) * safety)
//
// No clamps. The calculator returns the formula output verbatim.
// Safety/advisory concerns layered on top of the math — minimum-stock
// floors, over-capacity callouts — belong in the UI, not in the
// calculation. The replenishment modal/table render an informational
// "≈ N bins" annotation next to the threshold using bin_capacity, and
// engineer Override is the escape hatch for any value the engineer
// judges un-supportable.
//
// Bin capacity is still carried on the inputs so the UI can derive the
// implied-bins annotation server-side if it ever wants to.
//
// Calculate auto-fetches observed inputs from lead_time_queries.go
// over the engineer-chosen date range. CycleSeconds is engineer-
// supplied in the modal (the v6 brief leaves automatic peak-cycle
// derivation from hourly_counts for a later round). The pure-formula
// CalculateThresholds is exposed for callers that want to run the
// math on hand-supplied inputs without touching order_history.
//
// The JS modal in www/static/js/pages/replenishment.js mirrors this
// formula in recomputeOutputsLocally() so engineer edits show a live
// recompute without a server round-trip per keystroke. Keep the two
// formulas paired; a divergence will silently disagree with the
// server's Apply value.

package service

import (
	"math"
	"time"

	"shingoedge/store"
	"shingoedge/store/orders"
)

// ThresholdCalculatorInputs are the values the calculator consumes —
// auto-fetched from lead_time_queries.go for the date-range path,
// engineer-supplied on the pure-formula path.
type ThresholdCalculatorInputs struct {
	CycleSeconds        float64
	L1QueueSeconds      float64
	L1TransitSeconds    float64
	L2LoadSeconds       float64
	L2TransitSeconds    float64
	MarketToCellSeconds float64
	SafetyFactor        float64 // default 1.5 applied when <= 0
	BinCapacityUOP      int     // C, the per-bin UOP capacity — echoed back so the UI can render the implied-bins annotation
}

// ThresholdCalculatorOutputs are the suggested values for the two
// thresholds in play — formula output verbatim, no clamps. SafetyApplied
// is the effective safety factor (echoes the input or fills the default).
// Confidence is stamped by the Calculate driver based on data coverage.
type ThresholdCalculatorOutputs struct {
	L1Threshold   int
	CellReorder   int
	SafetyApplied float64
	Confidence    string // HIGH | MEDIUM | LOW | "" (set by Calculate; empty when no data was sampled)
}

// CalculateThresholds runs the unified-formula calculation. Pure
// function — no I/O, no DB. Suitable for both manual-input callers
// and the auto-input path (Calculate, below) which reads observed
// inputs from lead_time_queries.go and then dispatches here.
func CalculateThresholds(in ThresholdCalculatorInputs) ThresholdCalculatorOutputs {
	safety := in.SafetyFactor
	if safety <= 0 {
		safety = 1.5
	}
	out := ThresholdCalculatorOutputs{SafetyApplied: safety}
	if in.CycleSeconds > 0 {
		l1Lead := in.L1QueueSeconds + in.L1TransitSeconds + in.L2LoadSeconds + in.L2TransitSeconds
		out.L1Threshold = int(math.Ceil((l1Lead / in.CycleSeconds) * safety))
		out.CellReorder = int(math.Ceil((in.MarketToCellSeconds / in.CycleSeconds) * safety))
	}
	return out
}

// ThresholdCalculatorService wraps the pure CalculateThresholds with
// a date-range-driven input-fetch pass: pull observed lead times from
// order_history, plug them into the formula, score confidence by data
// coverage. Used by the engineer-triggered Calculate endpoint.
type ThresholdCalculatorService struct {
	db *store.DB
}

// NewThresholdCalculatorService constructs the service.
func NewThresholdCalculatorService(db *store.DB) *ThresholdCalculatorService {
	return &ThresholdCalculatorService{db: db}
}

// CalculateRequest captures the engineer's choices on the modal:
// payload + date range + safety factor (engineer-editable default).
// BinCapacityUOP comes from the loader claim's UOPCapacity; the
// caller should resolve and pass it.
type CalculateRequest struct {
	CoreNodeName   string
	PayloadCode    string
	DateRange      orders.DateRange
	SafetyFactor   float64
	BinCapacityUOP int
	// CycleSeconds is engineer-supplied — the UI surfaces this as an
	// editable field. Automatic peak-cycle derivation from
	// hourly_counts is a later round; the calculator works fine with
	// a manually-entered cycle until then.
	CycleSeconds float64
}

// CalculateResult is the API response shape: the observed inputs,
// the calculator's outputs, the confidence score, sample counts, and
// the timestamp at which the run was computed. The engineer's Apply /
// Override calls echo these back so the threshold row's
// threshold_calculated / threshold_calculated_at / confidence columns
// reflect this run without a follow-up server lookup.
type CalculateResult struct {
	Inputs          ThresholdCalculatorInputs  `json:"inputs"`
	Outputs         ThresholdCalculatorOutputs `json:"outputs"`
	Confidence      string                     `json:"confidence"`
	ComputedAt      string                     `json:"computed_at"`
	SamplesL1       int                        `json:"samples_l1"`
	SamplesL2       int                        `json:"samples_l2"`
	SamplesRetrieve int                        `json:"samples_retrieve"`
	DateRangeDays   int                        `json:"date_range_days"`
}

// Calculate runs the calculator and returns the result. There is no
// per-run audit persistence — the engineer reads the current
// threshold's source / updated_at / updated_by for "what's the
// current value based on", and re-runs Calculate to inspect fresh
// inputs.
func (s *ThresholdCalculatorService) Calculate(req CalculateRequest) (CalculateResult, error) {
	in := ThresholdCalculatorInputs{
		SafetyFactor:   req.SafetyFactor,
		BinCapacityUOP: req.BinCapacityUOP,
		CycleSeconds:   req.CycleSeconds,
	}
	if v, err := orders.AvgL1QueueSeconds(s.db.DB, req.PayloadCode, req.DateRange); err == nil {
		in.L1QueueSeconds = v
	}
	if v, err := orders.AvgL1TransitSeconds(s.db.DB, req.PayloadCode, req.DateRange); err == nil {
		in.L1TransitSeconds = v
	}
	if v, err := orders.MedianL2LoadSeconds(s.db.DB, req.PayloadCode, req.DateRange); err == nil {
		in.L2LoadSeconds = v
	}
	if v, err := orders.AvgL2TransitSeconds(s.db.DB, req.PayloadCode, req.DateRange); err == nil {
		in.L2TransitSeconds = v
	}
	if v, err := orders.P95MarketToCellSeconds(s.db.DB, req.PayloadCode, req.DateRange); err == nil {
		in.MarketToCellSeconds = v
	}

	out := CalculateThresholds(in)

	// Coverage signals for the confidence score.
	dateRangeDays := int(req.DateRange.End.Sub(req.DateRange.Start).Hours() / 24)
	if dateRangeDays < 1 {
		dateRangeDays = 1
	}
	samplesL1, _ := orders.CountCompletedOrdersInWindow(s.db.DB, "retrieve_empty", "confirmed", req.PayloadCode, req.DateRange)
	samplesL2, _ := orders.CountCompletedOrdersInWindow(s.db.DB, "store", "delivered", req.PayloadCode, req.DateRange)
	samplesRetrieve, _ := orders.CountCompletedOrdersInWindow(s.db.DB, "retrieve", "delivered", req.PayloadCode, req.DateRange)

	confidence := scoreConfidence(dateRangeDays, samplesL1, samplesL2, samplesRetrieve)
	out.Confidence = confidence

	return CalculateResult{
		Inputs:          in,
		Outputs:         out,
		Confidence:      confidence,
		ComputedAt:      time.Now().UTC().Format("2006-01-02 15:04:05"),
		SamplesL1:       samplesL1,
		SamplesL2:       samplesL2,
		SamplesRetrieve: samplesRetrieve,
		DateRangeDays:   dateRangeDays,
	}, nil
}

// scoreConfidence reduces the data-coverage signals to a HIGH /
// MEDIUM / LOW label. The v6 brief leaves the exact thresholds open;
// the rule of thumb is:
//
//	HIGH   — ≥14 days of window AND ≥20 completed L1 cycles AND
//	         ≥20 completed retrieves.
//	MEDIUM — ≥7 days AND ≥10 of each.
//	LOW    — anything below MEDIUM. UI gates the Apply button on
//	         non-LOW; engineer can still Override on LOW.
//
// Thresholds are intentionally conservative for the initial roll-out.
// Springfield is the first plant; we can tune after a calibration
// session shows how much data the plant actually accumulates.
func scoreConfidence(days, samplesL1, samplesL2, samplesRetrieve int) string {
	if days >= 14 && samplesL1 >= 20 && samplesRetrieve >= 20 {
		return "HIGH"
	}
	if days >= 7 && samplesL1 >= 10 && samplesRetrieve >= 10 {
		return "MEDIUM"
	}
	return "LOW"
}

