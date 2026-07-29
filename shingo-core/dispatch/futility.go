// futility.go — rate-per-tuple futility detector.
//
// The failure class this watches for: a LEVEL trigger converts a bounded
// physical condition into unbounded orchestration work. Springfield,
// 2026-07-21 — zero system stock on 74577-6SA0A.06, the supply leg parked on a
// dry source, the evac cancelled by the peer handler, and then the level fired
// again on the next PLC tick. 484 doomed swaps in under two hours, not one of
// which reached a robot. Every surface stayed green because every surface
// measures state, and the state at any instant was "a couple of orders in
// flight".
//
// WHAT ACTUALLY RE-ARMED IT — an earlier version of this comment said "the
// monitor re-armed, and the planner rebuilt the pair", which reads as Core's
// ThresholdMonitor and the changeover planner. Neither was involved, and that
// sentence sent three of five reviewers to the wrong subsystem before anyone
// checked. For the record:
//
//   - The trigger was CELL AUTOREORDER, on the Edge — style_node_claims.
//     auto_reorder + reorder_point, evaluated on EVERY PLC consume tick at
//     wiring_counter_delta.go:211-240. Claim 31 (SNF3 / ALN_003 /
//     74577-6SA0A.06) had reorder_point = 50, so every tick below 50 re-fired.
//   - The planner was BuildConsumePlan / applyConsumePlan, which expands one
//     RequestNodeMaterial call into a supply + evac pair — the shape that
//     produced "242 skipped + 242 cancelled".
//   - The changeover is the incident's PREHISTORY, not a participant: it is
//     why a bin with a fictional count came to sit on ALN_003.
//     applyChangeoverPlan has one caller (operator_changeover_start.go:84)
//     and does not loop.
//   - CanAcceptOrders is the only guard between the level and unbounded
//     creation, and it only blocks while tracked orders are non-terminal. Both
//     legs terminalised in seconds, so the guard cleared faster than the tick.
//
// Toggling auto_reorder off on claim 31 stopped the loop — one boolean. See
// changeover-sourcing-review-2026-07-21/
// INCIDENT-springfield-negative-uop-runaway-2026-07-21.md.
//
// Five commits in three weeks fixed five instances of this class. It recurs.
//
// WHY RATE, NOT A RUN COUNT
//
// The obvious detector — N consecutive futile orders on one tuple — is refuted
// by the plant's own history. Over 120 days, on real plant tuples with the
// incident window excluded, normal operation produced runs of 5 (six times),
// 6 (twice), 8, and 9 (three times), plus one run of 26. There is no knee: the
// distribution is a power-law tail, so a run threshold either fires ~7 times a
// quarter on healthy operation or sits above 26 and catches nothing.
//
// Time separates them cleanly:
//
//	ALN_001/76683-6TA0A.06, 2026-06-23   26 futile terminals over 6.6h   ~4/h
//	the 07-21 cascade                   484 futile terminals over <2h  ~242/h
//
// 60x. So the counter needed a clock, not a bigger number.
//
// The threshold is ABSOLUTE, not learned. A trailing baseline is not available
// here and would not help if it were: a 30-day window spanning the incident
// would be trained on it, and the database has a 2.5-week hole (06-27 → 07-15)
// that mis-baselines anything computed across it.
//
// SHIPS OBSERVE-ONLY. One log line and one audit_log row. No chip, no alert,
// no brake. A brake on an unmeasured threshold stops real work; the record
// comes first and the threshold gets set from it.

package dispatch

import (
	"fmt"
	"sync"
	"time"

	"shingo/shared/clock"
)

// FutilityKey is the tuple the detector counts on: the unit of work that
// repeats. Station and process node say WHERE, payload says WHAT — together
// they are "the same ask, again".
type FutilityKey struct {
	StationID   string
	ProcessNode string
	PayloadCode string
}

func (k FutilityKey) String() string {
	return fmt.Sprintf("station=%s node=%s payload=%s", k.StationID, k.ProcessNode, k.PayloadCode)
}

// incomplete reports whether the tuple is too vague to count on. A blank
// payload or node collapses unrelated work together — the probe's own run
// analysis had to discard exactly these rows, where blank payloads produced a
// spurious run of 41 across orders that had nothing to do with each other.
func (k FutilityKey) incomplete() bool {
	return k.StationID == "" || k.ProcessNode == "" || k.PayloadCode == ""
}

// FutilityConfig comes from YAML. Defaults live in config.Defaults().
type FutilityConfig struct {
	Enabled       bool
	Threshold     int
	Window        time.Duration
	AlertThrottle time.Duration
}

// AuditAppender is the durable-record seam — *store.DB satisfies it.
// swap_peer.go already writes this exact shape from this exact code path.
type AuditAppender interface {
	AppendAudit(entityType string, entityID int64, action, oldValue, newValue, actor string) error
}

// FutilityDetector counts futile terminals per tuple over a rolling window.
//
// "Futile" means the order reached a terminal status without ever having
// reached in_transit — planned, and abandoned before a robot ever moved. That
// is the signature of manufactured work: real work that fails still gets a
// robot to it.
type FutilityDetector struct {
	cfg   FutilityConfig
	logFn func(string, ...any)
	audit AuditAppender

	// now is the time source. Defaults to clock.Now so the sim's fast-forward
	// drives the window; injected in tests so they never mutate the process
	// default clock out from under a parallel test.
	now func() time.Time

	mu sync.Mutex
	// terminals holds the futile-terminal instants inside the window, per
	// tuple. Bounded by (rate x window): the 07-21 cascade would hold ~484
	// timestamps on one key. Keys are deleted when they empty.
	terminals map[FutilityKey][]time.Time
	lastAlert map[FutilityKey]time.Time
}

// NewFutilityDetector returns a detector, or nil when disabled — every method
// is nil-safe, so callers do not branch.
func NewFutilityDetector(cfg FutilityConfig, logFn func(string, ...any), audit AuditAppender) *FutilityDetector {
	if !cfg.Enabled || cfg.Threshold <= 0 || cfg.Window <= 0 {
		return nil
	}
	return &FutilityDetector{
		cfg:       cfg,
		logFn:     logFn,
		audit:     audit,
		now:       clock.Now,
		terminals: make(map[FutilityKey][]time.Time),
		lastAlert: make(map[FutilityKey]time.Time),
	}
}

// SetClock overrides the time source. Test seam only.
func (d *FutilityDetector) SetClock(now func() time.Time) {
	if d == nil || now == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = now
}

// NoteInTransit clears a tuple's history because work on it is genuinely
// moving. This is the reset, and it is per-TUPLE rather than per-order on
// purpose: one robot actually departing for this station/node/payload proves
// the condition that was manufacturing futile orders has cleared.
func (d *FutilityDetector) NoteInTransit(key FutilityKey) {
	if d == nil || key.incomplete() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.terminals, key)
}

// NoteFutileTerminal records one terminal that never reached a robot, and
// reports whether that crossed the threshold (after the per-tuple throttle).
// orderID is the order that tripped it — the audit row anchors to it so the
// record is reachable from the order as well as from the tuple.
func (d *FutilityDetector) NoteFutileTerminal(key FutilityKey, orderID int64, status, reason string) bool {
	if d == nil || key.incomplete() {
		return false
	}
	d.mu.Lock()
	now := d.now()
	kept := d.terminals[key][:0]
	for _, t := range d.terminals[key] {
		if now.Sub(t) < d.cfg.Window {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	d.terminals[key] = kept
	count := len(kept)

	if count < d.cfg.Threshold {
		d.mu.Unlock()
		return false
	}
	if last, ok := d.lastAlert[key]; ok && now.Sub(last) < d.cfg.AlertThrottle {
		d.mu.Unlock()
		return false
	}
	d.lastAlert[key] = now
	d.mu.Unlock()

	d.report(key, orderID, status, reason, count)
	return true
}

// report emits the two durable outputs: one loud log line and one audit row.
// Deliberately not a chip, an alert or a brake — see the file header.
func (d *FutilityDetector) report(key FutilityKey, orderID int64, status, reason string, count int) {
	msg := fmt.Sprintf(
		"FUTILITY: %d orders for %s reached a terminal status in %s without one reaching in_transit "+
			"(threshold %d). Nothing is being delivered for this tuple and the planner keeps building work for it — "+
			"check that %s is actually stocked and sourceable. Last: order %d %s (%s). "+
			"Observe-only; suppressed for %s.",
		count, key, d.cfg.Window, d.cfg.Threshold, key.PayloadCode, orderID, status, reason, d.cfg.AlertThrottle)

	if d.logFn != nil {
		d.logFn("%s", msg)
	}
	if d.audit != nil {
		if err := d.audit.AppendAudit("order", orderID, "futility_rate_exceeded", "", msg, "system"); err != nil && d.logFn != nil {
			d.logFn("futility: append audit for order %d: %v", orderID, err)
		}
	}
}

// Count returns the tuple's current in-window futile count. For tests and for
// whatever eventually surfaces this; not used in the decision path.
func (d *FutilityDetector) Count(key FutilityKey) int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	n := 0
	for _, t := range d.terminals[key] {
		if now.Sub(t) < d.cfg.Window {
			n++
		}
	}
	return n
}
