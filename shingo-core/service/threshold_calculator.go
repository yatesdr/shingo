// threshold_calculator.go — Core port of the UOP-threshold replenishment
// calculator (loader refactor: thresholds and their suggestion math move to
// Core-owned config). The pure formula is identical to the Edge original
// (shingo-edge/service/threshold_calculator.go); the difference is the input
// source — Core's order_history (Postgres lead-time helpers) instead of the
// Edge SQLite.
//
//	INPUTS  cycle_seconds, l1_queue, l1_transit, l2_load, l2_transit,
//	        market_to_cell, safety_factor (default 1.5)
//	OUTPUTS l1_threshold = ceil(((l1_queue+l1_transit+l2_load+l2_transit)
//	                             / cycle) * safety)
//	        cell_reorder = ceil((market_to_cell / cycle) * safety)
//
// No clamps — the formula output is returned verbatim; min-stock floors and
// over-capacity callouts are UI concerns. The JS modal mirrors this formula in
// recomputeOutputsLocally(); keep the two paired.
//
// Core has no payload_catalog/cycle_seconds (an Edge concept), so CycleSeconds
// is supplied by the caller (the modal already does). When it is 0 the formula
// guards return zero outputs rather than dividing by zero.

package service

import (
	"math"
	"time"

	"shingocore/store"
	"shingocore/store/orders"
)

// ThresholdCalculatorInputs are the values the calculator consumes — auto-
// fetched from the Core lead-time helpers for the date-range path, caller-
// supplied on the pure-formula path.
type ThresholdCalculatorInputs struct {
	CycleSeconds        float64
	L1QueueSeconds      float64
	L1TransitSeconds    float64
	L2LoadSeconds       float64
	L2TransitSeconds    float64
	MarketToCellSeconds float64
	SafetyFactor        float64
	BinCapacityUOP      int
}

// ThresholdCalculatorOutputs are the suggested threshold values — formula
// output verbatim, no clamps. SafetyApplied echoes the effective safety factor;
// Confidence is stamped by Calculate from data coverage.
type ThresholdCalculatorOutputs struct {
	L1Threshold   int
	CellReorder   int
	SafetyApplied float64
	Confidence    string // HIGH | MEDIUM | LOW | ""
}

// CalculateThresholds runs the unified-formula calculation. Pure — no I/O.
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

// ThresholdCalculatorService wraps the pure formula with a date-range-driven
// input fetch from Core's order_history. Used by a (future) Core replenishment
// endpoint; the engineer-triggered Calculate that today lives on the Edge.
type ThresholdCalculatorService struct {
	db *store.DB
}

// NewThresholdCalculatorService constructs the service.
func NewThresholdCalculatorService(db *store.DB) *ThresholdCalculatorService {
	return &ThresholdCalculatorService{db: db}
}

// CalculateRequest captures the engineer's modal choices: payload + date range
// + safety factor + cycle. Unlike the Edge, CycleSeconds is required (Core has
// no payload_catalog fallback); a zero cycle yields zero outputs.
type CalculateRequest struct {
	CoreNodeName   string
	PayloadCode    string
	DateRange      orders.LeadTimeRange
	SafetyFactor   float64
	BinCapacityUOP int
	CycleSeconds   float64
}

// CalculateResult is the API response shape: observed inputs, outputs,
// confidence, sample counts, and the run timestamp.
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

// Calculate fetches observed lead times from Core's order_history over the
// window, runs the formula, and scores confidence from data coverage.
func (s *ThresholdCalculatorService) Calculate(req CalculateRequest) (CalculateResult, error) {
	db := s.db.DB
	in := ThresholdCalculatorInputs{
		SafetyFactor:   req.SafetyFactor,
		BinCapacityUOP: req.BinCapacityUOP,
		CycleSeconds:   req.CycleSeconds,
	}
	if v, err := orders.AvgL1QueueSeconds(db, req.PayloadCode, req.DateRange); err == nil {
		in.L1QueueSeconds = v
	}
	if v, err := orders.AvgL1TransitSeconds(db, req.PayloadCode, req.DateRange); err == nil {
		in.L1TransitSeconds = v
	}
	if v, err := orders.MedianL2LoadSeconds(db, req.PayloadCode, req.DateRange); err == nil {
		in.L2LoadSeconds = v
	}
	// L2 (store) transit was keyed on the plain-store family, removed here — no
	// store orders exist, so this auto-fetch always returned 0. L2TransitSeconds
	// stays an operator-editable modal input, now defaulting to 0.
	if v, err := orders.P95MarketToCellSeconds(db, req.PayloadCode, req.DateRange); err == nil {
		in.MarketToCellSeconds = v
	}

	out := CalculateThresholds(in)

	dateRangeDays := max(int(req.DateRange.End.Sub(req.DateRange.Start).Hours()/24), 1)
	samplesL1, _ := orders.CountCompletedOrdersInWindow(db, "retrieve_empty", "confirmed", req.PayloadCode, req.DateRange)
	// L2 (store) coverage was keyed on the removed plain-store family — no store
	// deliveries exist, so this count was always 0. Kept as 0 so confidence scoring
	// is unchanged (store never contributed a positive coverage sample).
	samplesL2 := 0
	samplesRetrieve, _ := orders.CountCompletedOrdersInWindow(db, "retrieve", "delivered", req.PayloadCode, req.DateRange)

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

// CalculateDays is the handler-facing entrypoint: builds a days-lookback window
// ending now and runs Calculate, so www handlers can call the calculator with
// primitives instead of importing the store's range type.
func (s *ThresholdCalculatorService) CalculateDays(coreNode, payloadCode string, days int, safetyFactor, cycleSeconds float64, binCapacityUOP int) (CalculateResult, error) {
	if days <= 0 {
		days = 14
	}
	end := time.Now()
	return s.Calculate(CalculateRequest{
		CoreNodeName:   coreNode,
		PayloadCode:    payloadCode,
		DateRange:      orders.LeadTimeRange{Start: end.AddDate(0, 0, -days), End: end},
		SafetyFactor:   safetyFactor,
		CycleSeconds:   cycleSeconds,
		BinCapacityUOP: binCapacityUOP,
	})
}

// scoreConfidence reduces data coverage to HIGH / MEDIUM / LOW (identical to
// the Edge): HIGH ≥14d and ≥20 of each cycle type; MEDIUM ≥7d and ≥10 of each;
// LOW otherwise. L2 (store) coverage is scored alongside L1/retrieves because
// L2 timings feed the L1Threshold formula.
func scoreConfidence(days, samplesL1, samplesL2, samplesRetrieve int) string {
	if days >= 14 && samplesL1 >= 20 && samplesL2 >= 20 && samplesRetrieve >= 20 {
		return "HIGH"
	}
	if days >= 7 && samplesL1 >= 10 && samplesL2 >= 10 && samplesRetrieve >= 10 {
		return "MEDIUM"
	}
	return "LOW"
}
