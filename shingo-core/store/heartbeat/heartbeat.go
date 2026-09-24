// Package heartbeat holds the production-heartbeat data layer (plan §12):
// the cell_part_events projection (SQL, in store.go) and the analytical math
// over a stream of part-fire timestamps (this file). The math is a pure
// functional core — ComputeCellState / ComputeStops / ComputeMetrics take an
// ordered []PartEvent and return derived metrics with no DB access — so it is
// unit-testable without Postgres (the codebase's functional-core pattern, cf.
// material/material.go). The thin SQL shell lives in store.go.
package heartbeat

import (
	"sort"
	"time"
)

// PartEvent is one projected PLC counter tick (a cell_part_events row).
type PartEvent struct {
	ID             int64     `json:"id"`
	CellID         string    `json:"cell_id"`
	RecordedAt     time.Time `json:"recorded_at"`
	EdgeSnapshotID int64     `json:"edge_snapshot_id"`
	CountValue     int64     `json:"count_value"`
	Delta          int64     `json:"delta"`
	Anomaly        string    `json:"anomaly"`
	ProcessID      int64     `json:"process_id"`
	StyleID        int64     `json:"style_id"`
}

// Cell live-state buckets (plan §12). Derived from time-since-last-part vs the
// target cycle.
const (
	StateRunning   = "running"    // within 1.2× target
	StateSlowed    = "slowed"     // 1.2–3× target
	StateMicroStop = "micro-stop" // 3–30× target
	StateStopped   = "stopped"    // > 30× target
	StateNoData    = "no-data"    // no events in window
)

// Thresholds are the state-boundary multipliers of the target cycle
// (plan §12, tunable per §8 #12).
type Thresholds struct {
	SlowedMult    float64 // running → slowed boundary (default 1.2)
	MicroStopMult float64 // slowed → micro-stop boundary (default 3)
	StoppedMult   float64 // micro-stop → stopped boundary (default 30)
}

// DefaultThresholds returns the plan's default state boundaries.
func DefaultThresholds() Thresholds {
	return Thresholds{SlowedMult: 1.2, MicroStopMult: 3, StoppedMult: 30}
}

// CellState is the live state of one cell (plan §12 "Live state").
type CellState struct {
	State          string     `json:"state"`
	LastFire       *time.Time `json:"last_fire,omitempty"`
	SinceLastMS    int64      `json:"since_last_ms"`
	CurrentCycleMS int64      `json:"current_cycle_ms"`
	TargetCycleMS  int64      `json:"target_cycle_ms"`
	PartsLastHour  int64      `json:"parts_last_hour"`
	StopActive     *StopEvent `json:"stop_active,omitempty"`
}

// StopEvent is a gap exceeding the micro-stop threshold (plan §12 "Stops").
type StopEvent struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMS int64     `json:"duration_ms"`
	Kind       string    `json:"kind"` // micro-stop | stopped
}

// CellMetrics aggregates a window of events into the exec loss numbers
// (plan §12 "Stops" / "Effective production rate" / "Loss accounting").
type CellMetrics struct {
	Parts                 int64   `json:"parts"`
	RunMinutes            float64 `json:"run_minutes"`
	StopCount             int     `json:"stop_count"`
	TotalDowntimeMS       int64   `json:"total_downtime_ms"`
	MTBFMinutes           float64 `json:"mtbf_minutes"`
	MTTRMinutes           float64 `json:"mttr_minutes"`
	LongestStopMS         int64   `json:"longest_stop_ms"`
	EffectivePartsPerHour float64 `json:"effective_parts_per_hour"`
	PartsLost             int64   `json:"parts_lost"`
}

// AnomalyJump marks an unconfirmed PLC gap: the counter leapt by more than
// the Edge's jump threshold in one poll.
const AnomalyJump = "jump"

// sortedFires returns the events that count as production, sorted ascending
// by RecordedAt. Callers pass store-ordered slices, but the pure functions
// don't trust ordering.
//
// A JUMP IS NOT A FIRE. It is an unconfirmed PLC gap — a counter that leapt
// past the jump threshold in one poll — so it is evidence of neither parts nor
// a cycle nor the end of a stop. It counts toward nothing here: not Parts, not
// MTBF or stops, not the cycle or target. An operator confirms a jump on the
// Edge (counters.ConfirmAnomaly), which releases its units to inventory; that
// confirmation does not reach Core, so on Core a jump never counts. The drill
// still draws it (ComputeCellHeartbeat's Events).
func sortedFires(events []PartEvent) []PartEvent {
	out := make([]PartEvent, 0, len(events))
	for _, e := range events {
		if e.Anomaly != AnomalyJump {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RecordedAt.Before(out[j].RecordedAt) })
	return out
}

// partsIn counts parts by delta, not by row: a poll pass that saw three
// strokes is one row with delta = 3, and it is three parts. Counting rows made
// every part count depend on the poll rate.
func partsIn(fires []PartEvent) int64 {
	var n int64
	for _, e := range fires {
		n += e.Delta
	}
	return n
}

// perPartGap is the cycle a fire implies: the gap since the previous fire,
// divided by the parts it carried when it carried more than one.
func perPartGap(gap time.Duration, delta int64) time.Duration {
	if delta > 1 {
		return gap / time.Duration(delta)
	}
	return gap
}

// EstimateTarget derives a fallback target cycle from the recent stream when
// no cell_targets row is configured (§8 #11: auto-derive vs admin-set). Uses
// the MEDIAN per-part gap — the gap before a fire, divided by the parts it
// carried — which is robust to the long stop gaps in the stream (a healthy
// 22.5s cell with occasional stops still medians ≈ 22.5s). Jumps are not
// fires (sortedFires). Returns 0 for < 2 fires; configured targets always
// take precedence.
func EstimateTarget(events []PartEvent) time.Duration {
	ev := sortedFires(events)
	if len(ev) < 2 {
		return 0
	}
	gaps := make([]time.Duration, 0, len(ev)-1)
	for i := 1; i < len(ev); i++ {
		if g := perPartGap(ev[i].RecordedAt.Sub(ev[i-1].RecordedAt), ev[i].Delta); g > 0 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	return gaps[len(gaps)/2]
}

// ComputeCellState derives the live state from the event stream and the target
// cycle, as of `now`. No fires → no-data. State is set by time since the last
// fire relative to the target (running/slowed/micro-stop/stopped). The current
// cycle is the last fire's per-part gap; parts in the last hour are counted by
// delta. Jumps are not fires (sortedFires).
func ComputeCellState(events []PartEvent, target time.Duration, now time.Time, th Thresholds) CellState {
	ev := sortedFires(events)
	if len(ev) == 0 || target <= 0 {
		return CellState{State: StateNoData, TargetCycleMS: target.Milliseconds()}
	}
	last := ev[len(ev)-1].RecordedAt
	sinceLast := now.Sub(last)
	cs := CellState{
		LastFire:      &last,
		SinceLastMS:   sinceLast.Milliseconds(),
		TargetCycleMS: target.Milliseconds(),
	}
	if n := len(ev); n >= 2 {
		cs.CurrentCycleMS = perPartGap(ev[n-1].RecordedAt.Sub(ev[n-2].RecordedAt), ev[n-1].Delta).Milliseconds()
	}
	// Parts in the last hour ending at now, by delta.
	hourAgo := now.Add(-time.Hour)
	for _, e := range ev {
		if !e.RecordedAt.Before(hourAgo) {
			cs.PartsLastHour += e.Delta
		}
	}
	r := float64(sinceLast) / float64(target)
	switch {
	case r <= th.SlowedMult:
		cs.State = StateRunning
	case r <= th.MicroStopMult:
		cs.State = StateSlowed
	case r <= th.StoppedMult:
		cs.State = StateMicroStop
	default:
		cs.State = StateStopped
	}
	if cs.State == StateMicroStop || cs.State == StateStopped {
		cs.StopActive = &StopEvent{Start: last, End: now, DurationMS: sinceLast.Milliseconds(), Kind: stopKind(sinceLast, target, th)}
	}
	return cs
}

// ComputeStops returns every gap between fires that exceeds the micro-stop
// threshold (plan §12 "Stops"). Each becomes a discrete event with a kind. A
// jump inside a silence does not split it (sortedFires).
func ComputeStops(events []PartEvent, target time.Duration, th Thresholds) []StopEvent {
	ev := sortedFires(events)
	if len(ev) < 2 || target <= 0 {
		return nil
	}
	micro := time.Duration(float64(target) * th.MicroStopMult)
	var stops []StopEvent
	for i := 1; i < len(ev); i++ {
		gap := ev[i].RecordedAt.Sub(ev[i-1].RecordedAt)
		if gap > micro {
			stops = append(stops, StopEvent{
				Start:      ev[i-1].RecordedAt,
				End:        ev[i].RecordedAt,
				DurationMS: gap.Milliseconds(),
				Kind:       stopKind(gap, target, th),
			})
		}
	}
	return stops
}

func stopKind(gap, target time.Duration, th Thresholds) string {
	if float64(gap) > float64(target)*th.StoppedMult {
		return StateStopped
	}
	return StateMicroStop
}

// ComputeMetrics aggregates the window [since,until] into the loss numbers
// (plan §12). Parts are counted by delta over fires (partsIn, sortedFires);
// RunMinutes excludes downtime; MTBF/MTTR are over detected stops;
// PartsLost = expected-at-target − actual (clamped ≥ 0).
func ComputeMetrics(events []PartEvent, since, until time.Time, target time.Duration, th Thresholds) CellMetrics {
	m := CellMetrics{Parts: partsIn(sortedFires(events))}
	wall := until.Sub(since)
	if wall <= 0 {
		return m
	}
	stops := ComputeStops(events, target, th)
	m.StopCount = len(stops)
	var downtime time.Duration
	for _, s := range stops {
		d := time.Duration(s.DurationMS) * time.Millisecond
		downtime += d
		if s.DurationMS > m.LongestStopMS {
			m.LongestStopMS = s.DurationMS
		}
	}
	m.TotalDowntimeMS = downtime.Milliseconds()
	runTime := wall - downtime
	if runTime < 0 {
		runTime = 0
	}
	m.RunMinutes = runTime.Minutes()
	if m.StopCount > 0 {
		m.MTBFMinutes = runTime.Minutes() / float64(m.StopCount)
		m.MTTRMinutes = downtime.Minutes() / float64(m.StopCount)
	}
	if runTime > 0 {
		m.EffectivePartsPerHour = float64(m.Parts) / runTime.Hours()
	}
	if target > 0 {
		expected := int64(float64(wall) / float64(target))
		if lost := expected - m.Parts; lost > 0 {
			m.PartsLost = lost
		}
	}
	return m
}
