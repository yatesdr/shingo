package robotconfidence

import (
	"math"
	"time"
)

// The write rule: which of the ~518,400 readings a 12-robot fleet produces in
// a day are worth keeping.
//
// WHY FILTERING IS A CORRECTNESS REQUIREMENT AND NOT AN OPTIMIZATION.
// Measured at Hopkinsville 2026-08-05 over 816 samples (68 ticks × 12 robots
// at the same 2 s cadence Core already polls at): every stationary robot
// reported the IDENTICAL value 44 times running — standard deviation 0.000,
// not 0.003. The value is frozen, not merely stable. Only the two robots
// actually driving varied, and they swung 0.5 within 90 seconds. Parked
// robots were 92% of samples.
//
// Storing those rows would not just waste space. They carry no information,
// and because robots idle and charge in particular corners, they would drag
// every location's average toward wherever the fleet happens to park.
//
// WHY A PURE "ONLY WHEN MOVING" FILTER IS WRONG. It drops precisely the
// sample you most want: localization degrades, the robot stops, and the
// sample is discarded as "parked". That is the shape of Springfield's AMR-03
// dying mid-route in June (alarm 54020, "reflectors match failed"). Clauses
// 3, 4 and 5 exist to catch what clauses 1 and 2 structurally cannot.

// WriteRule holds the sampling thresholds. Every field is deployment config
// (see config.RobotConfidence) rather than a constant, because the right
// values depend on how busy a plant's floor is — unlike Coverage, which
// defines what the statistic means.
type WriteRule struct {
	DeadBandMetres     float64
	DeadBandConfidence float64
	LowThreshold       float64
	// LowInterval rate-limits clause 3 so a robot sitting at low confidence
	// cannot spam a row every 2 seconds.
	LowInterval time.Duration
	// StuckInterval rate-limits clause 4.
	StuckInterval time.Duration
	// FailedInterval rate-limits clause 5.
	FailedInterval time.Duration
}

// DefaultWriteRule matches the shipped shingocore.yaml defaults. Config is
// the source of truth; this exists so tests and any caller without a loaded
// config have one honest set of values rather than a zero struct that would
// store everything.
var DefaultWriteRule = WriteRule{
	DeadBandMetres:     0.25,
	DeadBandConfidence: 0.02,
	LowThreshold:       0.50,
	LowInterval:        10 * time.Second,
	StuckInterval:      30 * time.Second,
	FailedInterval:     10 * time.Second,
}

// LastStored is the per-robot memory of the row most recently WRITTEN — not
// the last one observed. Comparing against the last stored sample is what
// makes the dead-bands cumulative: a robot creeping 0.05 m per poll still
// trips the 0.25 m band on the fifth tick, where comparing against the
// previous observation would never trip it at all.
//
// This state is in-memory and losing it on restart is correct, not a gap:
// the first sample after a restart is always stored, which re-establishes the
// baseline immediately and costs one row per robot.
type LastStored struct {
	X          float64
	Y          float64
	Confidence float64
	At         time.Time
}

// Observation is one robot's reading from one poll.
type Observation struct {
	Connected   bool
	RelocStatus int
	Confidence  float64
	X           float64
	Y           float64
	// OnTask is the vendor-neutral "this robot has a job" signal
	// (fleet.RobotStatus.Busy).
	OnTask bool
}

// Reasons a sample is kept or dropped. Returned alongside the decision so the
// choice is legible in tests and in a debug log, rather than being a bare
// bool nobody can explain in six months.
const (
	ReasonFirst        = "first"        // no prior stored sample (incl. after restart)
	ReasonMoved        = "moved"        // clause 1
	ReasonChanged      = "changed"      // clause 2
	ReasonLow          = "low"          // clause 3
	ReasonStuck        = "stuck"        // clause 4
	ReasonFailed       = "failed"       // clause 5
	ReasonDisconnected = "disconnected" // gate
	ReasonRelocating   = "relocating"   // gate
	ReasonNoChange     = "no-change"    // parked, healthy, idle — the 92%
)

// Decide reports whether this observation should be stored, and why.
func (r WriteRule) Decide(obs Observation, last *LastStored, now time.Time) (bool, string) {
	// ── Gates ──────────────────────────────────────────────────────────────

	// A disconnected robot's last-known value is not a measurement. AMR-11
	// was connection_status=0 on the day of the survey and still reporting a
	// stale 0.660; storing that would record a reading nobody took.
	if !obs.Connected {
		return false, ReasonDisconnected
	}
	// Mid-relocation the pose estimate is in flight and the confidence figure
	// is transient garbage. This is the ONLY reloc state that is rejected —
	// 0 (FAILED) and 3 (COMPLETED) are settled states that mean different
	// things and are both worth recording. See migration v77.
	if obs.RelocStatus == 2 {
		return false, ReasonRelocating
	}

	// No baseline to compare against — store it and establish one.
	if last == nil {
		return true, ReasonFirst
	}

	since := now.Sub(last.At)
	moved := math.Hypot(obs.X-last.X, obs.Y-last.Y) >= r.DeadBandMetres

	// ── Clause 1: moved ────────────────────────────────────────────────────
	// The normal case, and the one that produces the spatial coverage every
	// segment statistic is built from.
	if moved {
		return true, ReasonMoved
	}

	// ── Clause 2: changed ──────────────────────────────────────────────────
	// Catches a robot degrading while creeping — below the movement band but
	// with the number visibly moving.
	if math.Abs(obs.Confidence-last.Confidence) >= r.DeadBandConfidence {
		return true, ReasonChanged
	}

	// ── Clause 3: low ──────────────────────────────────────────────────────
	// A robot sitting at low confidence is the forensic trail for "why did
	// AMR-06 strand at LM120 last month", at a cost of a few thousand rows a
	// week.
	if obs.Confidence < r.LowThreshold && since >= r.LowInterval {
		return true, ReasonLow
	}

	// ── Clause 4: stuck ────────────────────────────────────────────────────
	// On a job and not moving. Without this the dataset is blind to the exact
	// failure it was built to catch, because a robot that has stopped looks
	// identical to a robot that is parked.
	if obs.OnTask && since >= r.StuckInterval {
		return true, ReasonStuck
	}

	// ── Clause 5: failed ───────────────────────────────────────────────────
	// Widening the reloc gate to admit FAILED is necessary but not
	// sufficient. Walk the failure: a robot loses localization, stops, and is
	// not on a job. If it reports a genuinely low number, clause 3 has it.
	// But if it reports a STALE HIGH value — exactly what AMR-11 did while
	// disconnected, holding 0.660 — then it has not moved (1 no), has not
	// changed (2 no), is not below threshold (3 no) and is not on task
	// (4 no). Nothing would be stored, on the one robot in the plant that is
	// definitively lost. A localization failure is an event worth recording
	// regardless of what the confidence number claims.
	if obs.RelocStatus == 0 && since >= r.FailedInterval {
		return true, ReasonFailed
	}

	return false, ReasonNoChange
}
