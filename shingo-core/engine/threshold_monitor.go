// threshold_monitor.go — UOP-threshold replenishment, Core side.
// See shingo/docs/uop-threshold-replenishment.md for the design
// overview.
//
// The C-push architecture in one paragraph:
//
//   Core owns loader config (the bin_loaders aggregate) and the per-(loader,
//   payload) thresholds derived from it. On any activity for a monitored
//   payload — a BinUOPDelta, a LinesideBucketDelta, or a non-delta bin
//   mutation — Core re-reads the AUTHORITATIVE combined in-loop UOP for
//   that payload (SystemUOPForPayload = SUM(bins.uop_remaining) + active
//   lineside buckets) and evaluates it against the configured threshold.
//   When the total is below the threshold for a (loader, payload) pair,
//   Core emits a LoopBelowThresholdSignal on subject
//   demand.loop_below_threshold. Edge responds by firing L1 retrieve_empty
//   after its in-flight guard.
//
// Reads the source of truth, holds no private tally. The monitor used to
// keep its own incremental in-memory UOP total (uopCache), moved by each
// delta and re-baselined by a 60s reconcile sweep. That tally was a
// second copy of the same number that could — and did — silently drift
// from DB truth (Springfield 2026-07-21: cache stuck ~139 while DB was
// 31, so a threshold nudge fired nothing; 2026-07-23: cache stuck high
// after a direct-DB reassign suppressed ordering). F-1 benchmarked the
// authoritative read at ~0.43 ms/payload at plant scale, so the monitor
// now just READS it on every evaluation. That deletes the drift failure
// category outright: there is no cached belief left to diverge.
//
// Debounce policy: 15-second window per (loader_node, payload).
// In-memory state (lost on Core restart — that's intentional; the
// startup sweep handles the restart case). The debounce timer is
// reset on SyncRegistry when the threshold value for the pair changes,
// so a newly-applied threshold engages immediately.
//
// Startup sweep: on Run() the monitor walks every binding with
// threshold > 0 once, builds its per-payload binding cache
// (thresholdsByPayload — CONFIG, not a UOP tally), seeds the cold-start
// warm-up allowance, and evaluates each binding against the authoritative
// DB read. There is no ongoing reconcile sweep — every evaluation already
// reads the truth.
//
// There is no longer a second path to dedup against. The legacy bin-count
// DemandSignal route is retired: Core still emits produce DemandSignals, but
// Edge routes them to no handler, so the two-signals-race case this used to
// describe cannot happen. Core still sends no LoopBelowThresholdSignal for a
// pair with threshold = 0 — that pair is simply not monitored, and its loader
// is stocked by the operator push instead.
//
// WHAT DEDUPS THE ORDERS IS THE EPISODE, and it is worth saying here because
// this comment used to name the Edge's reservation seam (withLoaderBudget),
// which was deleted with the Edge's half of replenishment. For a while the
// answer to "what stops this firing twice for the same demand" was a function
// that no longer existed, and the real answer was "nothing" — Springfield
// 2026-08-03, 241 duplicate orders at one window. The debounce below is a rate
// limit, not a dedup: it decides how often it is worth ASKING, never whether the
// ask is already outstanding. That question is answered in
// dispatch.ReplenishLoader, which subtracts the episode's own live orders from
// the ask before creating any.
//
// Out of scope: iterate-all-claims for inactive styles (R3),
// queued-retrieve safety net at Edge.

package engine

import (
	"context"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingocore/dispatch"
	"shingocore/store/demands"
)

// thresholdDebounceWindow is the per-(loader, payload) suppression
// window for LoopBelowThresholdSignal. v5 brief: 15 seconds. Faster
// than v4's 30s for legitimate-crossing response, still long enough to
// absorb bursts from rapid bin-move / bucket-delta sequences.
const thresholdDebounceWindow = 15 * time.Second

// warmUpFloor is the floor in the per-binding warm-up cap formula
// max(2, ceil(threshold / C)). The capacity C is per-claim and isn't
// trivially queryable from Core, so for Phase 1 we apply only the
// floor — at least 2 signals on cold start so Springfield's fresh-
// start scenario lands one bin in supermarket + one in flight while
// the line consumes the initial bin. A later phase can lift C from
// claim config and apply the ceiling.
const warmUpFloor = 2

// negativeLogWindow throttles the negative-count warning line. The warning is
// evaluated on every incoming delta — i.e. every consume tick — so an
// unthrottled log would emit per binding per tick and bury the plant log in
// the one situation where an operator most needs to read it. Once a minute per
// binding is enough to keep the condition visible without becoming the noise.
const negativeLogWindow = 60 * time.Second

// Lineside decision modes (R1). Select which in-loop total the fire gate decides
// off — see config.ReplenishmentConfig.LinesideDecisionMode.
const (
	// linesideModeEdgeReports decides off the Edge-report-adjusted total (R1 LIVE,
	// the default). linesideModeLedger decides off Core's ledger alone (the revert
	// knob — pre-R1 behavior).
	linesideModeEdgeReports = "edge_reports"
	linesideModeLedger      = "ledger"
)

// resolveLinesideMode validates a configured lineside_decision_mode value. An
// empty or "edge_reports" value resolves to edge_reports (the default); "ledger"
// resolves to ledger; any other value falls back to edge_reports and warns via
// warnf (called at most once, at construction). Pure so it is unit-testable.
func resolveLinesideMode(raw string, warnf func(string, ...any)) string {
	switch raw {
	case "", linesideModeEdgeReports:
		return linesideModeEdgeReports
	case linesideModeLedger:
		return linesideModeLedger
	default:
		if warnf != nil {
			warnf("threshold_monitor: unknown lineside_decision_mode %q; falling back to %q", raw, linesideModeEdgeReports)
		}
		return linesideModeEdgeReports
	}
}

// swapContradictionWindow is how long a manual-swap-vs-ledger contradiction
// (P2-C9) stays surfaced as a Replenishment Health chip, and the throttle
// window for its log line. A human requesting a swap for a payload the ledger
// reads as fully stocked is a standing condition worth showing for a while, not
// a per-request event.
const swapContradictionWindow = 15 * time.Minute

// thresholdEntry is one (station, loader, payload) binding with its
// configured threshold, cached in memory so the monitor never queries
// demand_registry on the hot path.
type thresholdEntry struct {
	stationID    string
	coreNodeName string
	payloadCode  string
	threshold    int
	loaderID     int64 // the owning loader (cutover); 0 for legacy pre-cutover bindings → no LoaderKey on the signal
}

// ThresholdMonitor tracks in-loop UOP per payload incrementally and
// emits LoopBelowThresholdSignal when a monitored (loader, payload)
// drops below its configured threshold.
type ThresholdMonitor struct {
	eng *Engine

	// fireHook intercepts a fired signal instead of sending it. Test seam
	// only, same pattern as SourceabilityMonitor's publishFn: nil in
	// production, so the send path below is the only one that ever runs. It
	// exists because fireSignalCached dereferences eng, which the pure unit
	// harness leaves nil — without it, "did this fire?" is only answerable by
	// standing up a whole engine.
	fireHook func(b thresholdEntry, total int, reason string)

	mu sync.Mutex
	// debounce is the last-fired timestamp per (station, loader,
	// payload) key. A SendLoopBelowThresholdSignal is only emitted
	// when now > debounce[key] + thresholdDebounceWindow.
	debounce map[string]time.Time
	// warmUp tracks remaining cold-start fires per binding. Decremented
	// each time the monitor signals; once at zero, normal debounced
	// operation continues. Cap is seeded on startup sweep.
	warmUp map[string]int
	// sweepDone gates startup-sweep-only behavior. While false the
	// debounce check is bypassed on the very first signal per binding.
	sweepDone bool
	// thresholdsByPayload caches per-payload threshold bindings. Keyed
	// by payload_code. Built from the startup sweep and kept fresh via
	// OnThresholdChanges. This is CONFIG (which loaders watch which
	// payload at what threshold), NOT a UOP tally — the UOP total is read
	// fresh from the DB on every evaluation.
	thresholdsByPayload map[string][]thresholdEntry
	// negativeLogged is the last time the negative-count warning was logged
	// per binding. Deliberately SEPARATE from debounce: debounce is
	// signal-eligibility budget, this is log volume. Sharing one stamp would
	// mean a negative total consumed the binding's right to fire the moment
	// the ledger was corrected.
	negativeLogged map[string]time.Time
	// swapContradiction is the last time a manual-swap request arrived for a
	// payload the ledger read as fully stocked (P2-C9). Keyed by payload_code;
	// drives the Replenishment Health contradiction chip and throttles the log.
	swapContradiction map[string]time.Time
	// belowThresholdSince converts a LEVEL into an EDGE, keyed by bindingKey.
	//
	// checkBindings is level-triggered: "total < threshold" is true
	// continuously, for as long as it is true, and a level has no memory. That
	// is why 2026-07-21 read as hundreds of unrelated firings rather than one
	// demand — every incoming delta re-asked the same question and got the same
	// yes. debounce and warmUp exist to paper over exactly that absence.
	//
	// Stamped on the FIRST crossing, cleared on recovery. The episode between
	// the two edges is the demand.
	//
	// IN MEMORY, beside thresholdsByPayload and under the same mutex, because
	// Core has nowhere free to hang it. Edge could put its equivalent on the
	// claim row the hot path already loads; here evaluatePayload reads a
	// computed AGGREGATE (SystemUOPForPayload, a SUM over bins and buckets),
	// not a row, and it cannot go on demand_registry — SyncRegistry DELETEs and
	// re-inserts the whole station on every Edge reconnect. A DB read per
	// below-threshold evaluation would land worst precisely where it matters:
	// a binding stuck below threshold is re-evaluated on EVERY delta.
	belowThresholdSince map[string]time.Time
	// openOrigins is the open episode's id per bindingKey — what every signal
	// fired for that demand gets stamped with, so the orders Edge mints in
	// response are children of it.
	//
	// REHYDRATED BY startupSweep, not rebuilt empty. See
	// rehydrateThresholdEpisodes.
	openOrigins map[string]openEpisodeRef

	// linesideMode is the resolved R1 decision mode (edge_reports | ledger),
	// validated once at construction from config. Read on every evaluation to
	// pick which in-loop total the fire gate decides off. Empty is treated as the
	// edge_reports default (the nil-eng unit harness leaves it unset).
	linesideMode string

	// now supplies every timestamp this monitor measures an interval against.
	//
	// It exists because all four of them used to call bare time.Now() while
	// everything they gate moves in SIM time: sim startup installs a
	// fast-forward clock globally (clock.BuildSimClock → clock.SetDefault,
	// cmd/shingocore/sim_enabled.go:47,61) and the rest of the engine reads
	// clock.Now(). So in the sim the 15s debounce, the 60s negative-log window
	// and the 15-minute contradiction window all ran on WALL time while the
	// activity they throttle ran 15× faster — one debounce covering fifteen
	// times more simulated work than it would at a plant. The hysteresis
	// margins get tuned on that sim.
	//
	// Defaults to clock.Now, which IS time.Now in production. Tests that need
	// to drive it set the field; the ones that back-date the maps directly
	// (threshold_monitor_test.go) keep working untouched.
	now func() time.Time
}

// NewThresholdMonitor constructs the monitor. Call Run() to perform
// the startup sweep.
func NewThresholdMonitor(e *Engine) *ThresholdMonitor {
	// Resolve the R1 decision mode once, at construction — deployment config, not a
	// hot-reload knob. Validating here means unknown values warn exactly once
	// instead of on every evaluation.
	rawMode := ""
	var warnf func(string, ...any)
	if e != nil {
		warnf = e.logFn
		if e.cfg != nil {
			rawMode = e.cfg.Replenishment.LinesideDecisionMode
		}
	}
	return &ThresholdMonitor{
		eng:                 e,
		debounce:            make(map[string]time.Time),
		warmUp:              make(map[string]int),
		thresholdsByPayload: make(map[string][]thresholdEntry),
		negativeLogged:      make(map[string]time.Time),
		swapContradiction:   make(map[string]time.Time),
		belowThresholdSince: make(map[string]time.Time),
		openOrigins:         make(map[string]openEpisodeRef),
		linesideMode:        resolveLinesideMode(rawMode, warnf),
		now:                 clock.Now,
	}
}

// nowFn returns the monitor's clock, falling back to the shared default.
//
// The fallback is for monitors built as a struct literal rather than through
// NewThresholdMonitor — the pure unit harness does exactly that — so a
// zero-value monitor keeps working instead of panicking on a nil func.
func (m *ThresholdMonitor) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return clock.Now()
}

// decisionMode reports the resolved R1 lineside decision mode. Any value other
// than the explicit ledger mode (including the unset unit-harness default) reads
// as edge_reports — the value is validated at construction, so this is a plain
// read with a safe default.
func (m *ThresholdMonitor) decisionMode() string {
	if m.linesideMode == linesideModeLedger {
		return linesideModeLedger
	}
	return linesideModeEdgeReports
}

// MonitorBinding is one monitored (station, node, payload) threshold binding,
// exported for the Snapshot read model behind the inventory Replenishment
// Health page.
type MonitorBinding struct {
	StationID    string `json:"station_id"`
	CoreNodeName string `json:"core_node_name"`
	Threshold    int    `json:"threshold"`
	LoaderID     int64  `json:"loader_id,omitempty"`
}

// MonitorSnapshotEntry is the set of threshold bindings watching one payload —
// which loaders monitor it, at what threshold. It carries no UOP total: the
// monitor holds no cached belief, so the caller reads DB truth
// (SystemUOPForPayload) directly for the on-hand number.
type MonitorSnapshotEntry struct {
	PayloadCode string           `json:"payload_code"`
	Bindings    []MonitorBinding `json:"bindings"`
	// SwapContradiction is true when a manual-swap request arrived for this
	// payload within swapContradictionWindow while the ledger read it as fully
	// stocked (P2-C9). Surfaced as a Replenishment Health chip.
	SwapContradiction bool `json:"swap_contradiction"`
}

// Snapshot returns which payloads are monitored and the binding set watching
// each — a point-in-time read for the inventory Replenishment Health page.
// Taken under the monitor lock; safe to call from an HTTP handler. It reports
// only the monitored-set + thresholds; the caller reads DB on-hand itself.
func (m *ThresholdMonitor) Snapshot() []MonitorSnapshotEntry {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MonitorSnapshotEntry, 0, len(m.thresholdsByPayload))
	for payload, bindings := range m.thresholdsByPayload {
		bs := make([]MonitorBinding, 0, len(bindings))
		for _, b := range bindings {
			bs = append(bs, MonitorBinding{
				StationID:    b.stationID,
				CoreNodeName: b.coreNodeName,
				Threshold:    b.threshold,
				LoaderID:     b.loaderID,
			})
		}
		contradiction := false
		if last, ok := m.swapContradiction[payload]; ok && now.Sub(last) < swapContradictionWindow {
			contradiction = true
		}
		out = append(out, MonitorSnapshotEntry{
			PayloadCode:       payload,
			Bindings:          bs,
			SwapContradiction: contradiction,
		})
	}
	return out
}

// bindingKey composes the (station, core_node_name, payload) tuple
// used to key per-binding state in the threshold monitor's debounce
// and warm-up maps.
func bindingKey(station, coreNodeName, payload string) string {
	return station + "|" + coreNodeName + "|" + payload
}

// Run performs the startup sweep then returns. Idempotent — calling
// twice is harmless; the second call is a no-op because the sweep flag
// stays set.
//
// Sweep runs in a goroutine so it doesn't block Engine startup; ordering
// vs. uop_backfill is handled at the cmd/shingocore wiring layer
// (sweep runs after a backfill-completion gate). For Phase 1 the
// monitor itself just waits a short grace period before sweeping.
func (m *ThresholdMonitor) Run(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		m.startupSweep(ctx)
	}()
}

// readTotal reads the authoritative in-loop UOP total for one payload straight
// from the DB (SystemUOPForPayload = SUM(bins.uop_remaining) over the lifecycle
// filter + active lineside buckets). This is the single source of truth the
// monitor evaluates on EVERY firing decision; there is no cached belief that
// can drift from it. F-1 benchmarked it at ~0.43 ms/payload at plant scale.
// Returns (0, nil) when there is no engine/inventory service (pure unit
// harness) so callers behave as "zero on-hand" rather than panicking.
func (m *ThresholdMonitor) readTotal(ctx context.Context, payloadCode string) (int, error) {
	if m.eng == nil || m.eng.inventoryService == nil {
		return 0, nil
	}
	uop, err := m.eng.inventoryService.SystemUOPForPayload(ctx, []string{payloadCode})
	if err != nil {
		return 0, err
	}
	if len(uop.Counts) > 0 {
		return uop.Counts[0].TotalUOP, nil
	}
	return 0, nil
}

// evaluatePayload re-reads the authoritative sum for a monitored payload and
// checks its bindings — the single entry point the delta hot path and the
// non-delta bin-update path both funnel through. Empty/unmonitored payloads
// short-circuit BEFORE the DB read, so the hot path only pays for payloads a
// binding actually watches. On a transient DB-read error it logs and SKIPS the
// check (the next delta re-evaluates) rather than manufacturing a fire off a
// zero — the config-edit path (engagePayloads) is the only one that
// deliberately fires on a read failure.
func (m *ThresholdMonitor) evaluatePayload(payloadCode, reason string) {
	if payloadCode == "" {
		return
	}
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payloadCode]
	m.mu.Unlock()
	if !monitored {
		return
	}
	// R1: compute BOTH the pure ledger total and the Edge-report-adjusted total.
	// The decision mode picks which one the fire gate decides off; the audit log
	// records any binding whose firing decision the two totals disagree on, on
	// every eval, whichever mode is active.
	edgeTotal, ledgerTotal, usedEdge, err := m.linesideDecisionTotal(context.Background(), payloadCode)
	if err != nil {
		if m.eng != nil {
			m.eng.logFn("threshold_monitor: SystemUOPForPayload(%s): %v", payloadCode, err)
		}
		return
	}
	decisionTotal := ledgerTotal
	if m.decisionMode() == linesideModeEdgeReports {
		decisionTotal = edgeTotal
	}
	m.auditLinesideDecision(payloadCode, bindings, ledgerTotal, edgeTotal, usedEdge)
	m.checkBindings(bindings, decisionTotal, reason, usedEdge)
}

// startupSweep iterates every (loader, payload) with threshold > 0,
// builds the per-payload binding cache, seeds the cold-start warm-up
// allowance, and evaluates each binding against an authoritative DB read —
// firing signals for any already below threshold. It holds no UOP tally
// afterwards; every later evaluation reads the DB again.
func (m *ThresholdMonitor) startupSweep(ctx context.Context) {
	// REHYDRATE BEFORE EVALUATING ANYTHING. The sweep below re-evaluates every
	// binding, and any that is still below threshold looks like a first
	// crossing to empty maps — so it would mint a second episode for a place
	// that already has one open, on every restart, for every hungry loader.
	// This must come before the early return below too: a ListDemandThresholds
	// failure still leaves the monitor running.
	m.rehydrateThresholdEpisodes()

	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		m.eng.logFn("threshold_monitor: startup sweep ListDemandThresholds: %v", err)
		m.mu.Lock()
		m.sweepDone = true
		m.mu.Unlock()
		return
	}
	m.eng.logFn("threshold_monitor: startup sweep — evaluating %d monitored bindings", len(entries))

	byPayload := map[string][]demands.RegistryEntry{}
	for _, e := range entries {
		if e.ReplenishUOPThreshold <= 0 {
			continue
		}
		byPayload[e.PayloadCode] = append(byPayload[e.PayloadCode], e)
	}

	// Build threshold cache.
	m.mu.Lock()
	for payload, bindings := range byPayload {
		tes := make([]thresholdEntry, 0, len(bindings))
		for _, b := range bindings {
			tes = append(tes, thresholdEntry{
				stationID:    b.StationID,
				coreNodeName: b.CoreNodeName,
				payloadCode:  b.PayloadCode,
				threshold:    b.ReplenishUOPThreshold,
				loaderID:     b.LoaderID,
			})
		}
		m.thresholdsByPayload[payload] = tes
	}
	m.mu.Unlock()

	// Evaluate each binding against the authoritative DB read once at boot.
	for payload, bindings := range byPayload {
		if ctx.Err() != nil {
			return
		}
		total, err := m.readTotal(ctx, payload)
		if err != nil {
			m.eng.logFn("threshold_monitor: startup sweep SystemUOPForPayload(%s): %v", payload, err)
			continue
		}
		// Seed the cold-start warm-up allowance, then let checkBindings make the
		// FIRE decision — it is the only place that decision is made.
		//
		// This block used to compare total < threshold and call fireSignalCached
		// itself, which meant the startup path silently bypassed every guard
		// checkBindings applies. One fire decision, one set of guards — which
		// matters more now, not less: a negative total no longer suppresses,
		// and restart is exactly when that shows up, because restarting Core is
		// the remedy an operator reaches for BECAUSE the counts look wrong.
		//
		// Warm-up is still seeded here, and still only for bindings that are
		// currently below threshold — that is the sweep's own cold-start concern
		// (Springfield's fresh start wants one bin in the supermarket and one in
		// flight), not a fire decision. It must be seeded BEFORE checkBindings
		// because allow() is what consumes it. If the floor suppresses the fire,
		// the allowance simply survives until the ledger is corrected.
		tes := make([]thresholdEntry, 0, len(bindings))
		m.mu.Lock()
		for _, b := range bindings {
			if total < b.ReplenishUOPThreshold {
				m.warmUp[bindingKey(b.StationID, b.CoreNodeName, b.PayloadCode)] = warmUpFloor
			}
			tes = append(tes, thresholdEntry{
				stationID:    b.StationID,
				coreNodeName: b.CoreNodeName,
				payloadCode:  b.PayloadCode,
				threshold:    b.ReplenishUOPThreshold,
				loaderID:     b.LoaderID,
			})
		}
		m.mu.Unlock()
		m.checkBindings(tes, total, "warm_up_startup_sweep", false)
	}

	m.mu.Lock()
	m.sweepDone = true
	m.mu.Unlock()
	m.eng.logFn("threshold_monitor: startup sweep complete — evaluating from authoritative DB reads")
}

// OnBinUOPDelta applies a bin UOP delta to the cached total and checks
// thresholds. Called by HandleBinUOPDelta after the delta has been applied to
// bins.uop_remaining. The delta value is retained for interface compatibility
// with the messaging layer but no longer drives the math: the monitor re-reads
// the authoritative sum (which already reflects the just-applied write) instead
// of moving a private tally. Unmonitored/empty payloads short-circuit before
// the read.
func (m *ThresholdMonitor) OnBinUOPDelta(payloadCode string, delta int) {
	m.evaluatePayload(payloadCode, "below_threshold")
}

// OnBucketApplied is invoked by the messaging layer after a successful
// LinesideBucketDelta apply: it re-reads the authoritative sum and checks
// thresholds, short-circuiting for unmonitored or empty payloads.
//
// It used to also emit an engine event, unconditionally, with a comment saying
// other subscribers relied on it. There were none — not one production
// subscriber anywhere, and no catch-all subscriber on Core either — so the only
// thing that ever received it was a test asserting it was sent. Deleted with
// the event type. The station and node arguments survive in the signature
// because the caller has them and a future subscriber would need them; they are
// unused here and marked so.
func (m *ThresholdMonitor) OnBucketApplied(station, coreNodeName, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason) {
	_, _, _, _ = station, coreNodeName, delta, reason
	m.evaluatePayload(payloadCode, "below_threshold")
}

// handleBinUpdated is the EventBinUpdated subscriber for non-delta bin
// mutations (status changes, manual moves, corrections). These events don't
// carry a UOP delta, so — like every other evaluation now — it re-reads the
// authoritative sum for the payload and checks thresholds.
func (m *ThresholdMonitor) handleBinUpdated(ev BinUpdatedEvent) {
	m.evaluatePayload(ev.PayloadCode, "below_threshold")
}

// NoteSwapRequestContradiction is the P2-C9 contradiction check. Called when a
// manual swap request (a complex order — the shape the incident's operator
// swap took) arrives for a payload. If the monitor's ledger reads that payload
// as fully stocked (in-loop total at or above its highest binding threshold)
// yet a human at the line is requesting a swap, that is a contradiction — the
// SNF3 CARRIER-0024 shape, where Core held a 150-UOP phantom on-hand while the
// operator's tile read 46 and the line starved. It logs a
// manual_request_vs_ledger warning, raises a Replenishment Health chip, and
// immediately re-evaluates the payload.
//
// With the private tally gone, "re-evaluate" is simply "re-read" — the only
// mode there is now. It creates NO orders: the re-read fires the normal
// (debounced) signal only if the ledger is genuinely below threshold, which is
// the non-contradiction case; when it reads stocked, nothing fires. The
// contradiction log/chip is throttled to once per swapContradictionWindow per
// payload, so a burst of complex orders can't spam it.
func (m *ThresholdMonitor) NoteSwapRequestContradiction(payloadCode string) {
	if payloadCode == "" {
		return
	}
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payloadCode]
	m.mu.Unlock()
	if !monitored {
		return
	}
	total, err := m.readTotal(context.Background(), payloadCode)
	if err != nil {
		if m.eng != nil {
			m.eng.logFn("threshold_monitor: NoteSwapRequestContradiction SystemUOPForPayload(%s): %v", payloadCode, err)
		}
		return
	}
	maxThreshold := 0
	for _, b := range bindings {
		if b.threshold > maxThreshold {
			maxThreshold = b.threshold
		}
	}
	if maxThreshold > 0 && total >= maxThreshold {
		if m.recordSwapContradiction(payloadCode) && m.eng != nil {
			m.eng.logFn("threshold_monitor: manual_request_vs_ledger — swap requested for payload=%s while the ledger reads STOCKED (in-loop total=%d >= max binding threshold=%d); the line may be starving behind a phantom on-hand — check this payload's bins for a stale staged bin (further occurrences suppressed for %s)",
				payloadCode, total, maxThreshold, swapContradictionWindow)
		}
	}
	// Immediately re-evaluate — a re-read now. Creates no orders when stocked.
	m.checkBindings(bindings, total, "manual_swap_recheck", false)
}

// recordSwapContradiction stamps a swap-vs-ledger contradiction for the payload
// if the last one is outside the window, returning whether it was newly
// recorded (i.e. should be logged now). Fixed-window throttle: the log fires
// once per window and the chip reads the stamp for the rest of it.
func (m *ThresholdMonitor) recordSwapContradiction(payloadCode string) bool {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.swapContradiction[payloadCode]; ok && now.Sub(last) < swapContradictionWindow {
		return false
	}
	m.swapContradiction[payloadCode] = now
	return true
}

// checkBindings evaluates all threshold bindings for a given total and
// fires signals for any that are below threshold and past debounce.
func (m *ThresholdMonitor) checkBindings(bindings []thresholdEntry, total int, reason string, usedEdgeReports bool) {
	// A NEGATIVE TOTAL NO LONGER SUPPRESSES REPLENISHMENT.
	//
	// It used to. The reasoning was "a negative total is a broken ledger, so
	// refusing to act on it is input validation" — and that is exactly
	// backwards on a plant floor.
	//
	// What a negative count actually means, per the people who run the line:
	// a press overpacked, or a fork truck delivered parts outside ShinGo and
	// nobody told it, or some other human intervention it cannot see. It is a
	// data-quality problem for a person to look at. It is NOT a reason to stop
	// the plant.
	//
	// And the direction is wrong too. A negative reading is too LOW, not too
	// high — so the honest response to it is "order material", which is what
	// the threshold check below already does. Suppressing instead produced the
	// worst possible pairing: a number saying the line is empty, and a system
	// answering by ordering nothing. Springfield logged that refusal 1,119
	// times a DAY, and it is the first link in the 2026-07-21 chain —
	// ledger negative, replenishment silent, payload genuinely dry, changeover
	// arming onto a dry source.
	//
	// So: fall through and evaluate normally. The count still gets flagged for
	// a human — loudly here, and as an exception row on the inventory page —
	// but the line keeps getting material while they sort it out. Over-ordering
	// is recoverable. Starving a line because a count was wrong is not.
	if total < 0 {
		for _, b := range bindings {
			if b.threshold <= 0 {
				continue
			}
			// Throttled per binding: this runs on every incoming delta, so an
			// unthrottled line would bury the plant log exactly when it needs
			// reading. shouldLogNegative touches ONLY negativeLogged — the
			// binding's debounce budget is untouched.
			if !m.shouldLogNegative(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)) {
				continue
			}
			if m.eng != nil { // nil in the pure unit harness (newTestMonitor)
				m.eng.logFn("threshold_monitor: NEGATIVE COUNT station=%s loader=%s payload=%s — in-loop total is %d (threshold %d); the bins ledger for this payload is wrong (overpack, an untracked delivery, or a manual move) and needs a recount. Replenishment CONTINUES on this reading — a wrong count must not starve the line (further occurrences suppressed for %s)",
					b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold, negativeLogWindow)
			}
		}
		// Deliberately NO return — fall through to the normal evaluation.
	}
	for _, b := range bindings {
		if b.threshold <= 0 {
			continue
		}
		if total >= b.threshold {
			// THE RISING EDGE. Until the demand grain existed this branch did
			// nothing at all — recovery was simply the absence of firing, which
			// is why there was no way to say when a demand ENDED, and therefore
			// no way to say what one had cost.
			m.closeThresholdEpisode(bindingKey(b.stationID, b.coreNodeName, b.payloadCode),
				protocol.CloseReasonRecovered, protocol.ClosedByNotification)
			continue
		}
		key := bindingKey(b.stationID, b.coreNodeName, b.payloadCode)
		// THE FALLING EDGE, AND IT IS MINTED BEFORE THE DEBOUNCE GATE ON
		// PURPOSE. The episode is the DEMAND; the signal is the ACTION taken
		// about it. Debounce decides how often it is worth acting — it must not
		// decide whether the need is recorded, or a demand that fired once and
		// then stayed suppressed for hours would look like it lasted an
		// instant. The episode opens when the place goes hungry.
		m.openThresholdEpisode(key, b, total, usedEdgeReports)
		if !m.allow(key) {
			// nil-guarded like every other eng use here. Unreachable with a
			// nil eng until now: a negative total used to return before this
			// point, so the pure unit harness never got here.
			if m.eng != nil {
				m.eng.dbg("threshold_monitor: suppress station=%s loader=%s payload=%s total=%d threshold=%d (debounce)",
					b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold)
			}
			continue
		}
		m.fireSignalCached(b, total, reason)
	}
}

// shouldLogNegative reports whether the broken-ledger refusal should be logged
// for this binding now, stamping it when it should. Pure log-volume control —
// it never touches debounce or warm-up, so refusing to signal on a garbage
// total costs the binding nothing once the total is real again.
func (m *ThresholdMonitor) shouldLogNegative(key string) bool {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, seen := m.negativeLogged[key]; seen && now.Sub(last) < negativeLogWindow {
		return false
	}
	m.negativeLogged[key] = now
	return true
}

// allow returns true if the binding may fire now under the debounce
// + warm-up policy. Records the firing time on success so a follow-up
// call within the window returns false.
func (m *ThresholdMonitor) allow(key string) bool {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.warmUp[key]; ok && w > 0 {
		m.warmUp[key] = w - 1
		m.debounce[key] = now
		return true
	}
	last, seen := m.debounce[key]
	if seen && now.Sub(last) < thresholdDebounceWindow {
		return false
	}
	m.debounce[key] = now
	return true
}

// fireSignalCached decides a loader's replenishment from a cached threshold
// entry. Used by checkBindings in steady state and by the startup sweep (which
// constructs a thresholdEntry inline).
//
// THIS IS THE CUTOVER. It used to build a LoopBelowThresholdSignal and send it
// to the Edge, which then worked out how many carriers were needed and where
// they should go. That split is what this whole program exists to end: the two
// halves counted different things, and only one of them could see the plant.
// On 2026-07-31 a loader at Springfield ordered far more carriers than it had
// places to put them, because the half that sized the ask could not see that
// the windows were full.
//
// Core now decides the whole thing. It sizes the ask from the same reading, it
// resolves which windows may take a carrier, and it creates one order per free
// window — so the bound is the window list, and a reading that asks for a
// hundred carriers at a three-window loader creates three.
//
// This is the ONE construction-and-send site, and every path reaches it through
// checkBindings → allow() → here, so it is post-debounce and the per-bin
// capacity read added below is per-fire rather than per-tick.
func (m *ThresholdMonitor) fireSignalCached(b thresholdEntry, total int, reason string) {
	if m.fireHook != nil {
		m.fireHook(b, total, reason)
		return
	}
	if m.eng == nil || m.eng.dispatcher == nil {
		// The dispatcher is built in Start(); the monitor's startup sweep can in
		// principle beat it. Same nil-guard the wiring uses.
		return
	}
	// Per-bin capacity is the one datum the binding does not already carry. Zero
	// means the catalog has no answer for this part, and ReplenishLoader refuses
	// rather than guessing — a guessed carrier count is how a loader ends up with
	// more carriers than places.
	var perBin int
	if pl, err := m.eng.db.GetPayloadByCode(b.payloadCode); err == nil && pl != nil {
		perBin = pl.UOPCapacity
	}

	originID := m.currentThresholdOrigin(bindingKey(b.stationID, b.coreNodeName, b.payloadCode))

	cfg, ok, err := m.eng.dispatcher.LoadReplenishConfig(b.loaderID)
	if err != nil {
		m.eng.logFn("threshold_monitor: load loader %d config for %s/%s: %v",
			b.loaderID, b.coreNodeName, b.payloadCode, err)
		return
	}
	if !ok {
		// Legacy binding with no loader id, or a loader that has been archived.
		// Nothing to decide against; refusing is the safe answer and it is not an
		// error worth a line every debounce window.
		m.eng.dbg("threshold_monitor: no loader config for binding %s/%s (loader=%d) — not replenishing",
			b.coreNodeName, b.payloadCode, b.loaderID)
		return
	}

	res, err := m.eng.dispatcher.ReplenishLoader(dispatch.ReplenishRequest{
		StationID:      b.stationID,
		LoaderID:       b.loaderID,
		PayloadCode:    b.payloadCode,
		MemberNode:     b.coreNodeName,
		Threshold:      b.threshold,
		CurrentUOP:     total,
		PerBinCapacity: perBin,
		// The demand episode the orders belong to. On the old wire path this
		// travelled with the signal and came back on the Edge's orders; now the
		// order is created here, so it is simply stamped.
		OriginID:    originID,
		OriginClass: string(protocol.OriginClassAttached),
	}, cfg)
	if err != nil {
		m.eng.logFn("threshold_monitor: replenish loader=%d station=%s payload=%s: %v",
			b.loaderID, b.stationID, b.payloadCode, err)
		return
	}
	if res.Skipped != "" {
		m.eng.logFn("threshold_monitor: loader_replenish station=%s loader=%d payload=%s current=%d threshold=%d reason=%s skipped=%s",
			b.stationID, b.loaderID, b.payloadCode, total, b.threshold, reason, res.Skipped)
		return
	}
	m.eng.logFn("threshold_monitor: loader_replenish station=%s loader=%d payload=%s current=%d threshold=%d reason=%s created=%d want=%d held=%v",
		b.stationID, b.loaderID, b.payloadCode, total, b.threshold, reason, len(res.Created), res.Want, res.HeldBy)
}

// OnThresholdChanges resets per-binding debounce + warm-up state for
// every (loader, payload) whose threshold value moved, and rebuilds
// the in-memory threshold cache for affected payloads. Called by the
// loader config-edit path (service/loader_service.go) after
// SyncDemandRegistry returns its change list.
//
// After rebuilding the cache, this function re-evaluates the affected
// bindings against the current cached UOP total and fires
// LoopBelowThresholdSignal immediately for any binding already below
// threshold. Closes the gap where a newly-added or threshold-increased
// binding for a payload with no incoming bin/bucket deltas (e.g. a
// zero-stock payload) would stay silent until Core restarted — the
// Springfield 6883 case where a threshold was configured but never
// triggered because no delta arrived to drive checkBindings.
func (m *ThresholdMonitor) OnThresholdChanges(changes []demands.RegistryChange) {
	if len(changes) == 0 {
		return
	}

	affectedPayloads := make(map[string]bool)

	m.mu.Lock()
	for _, c := range changes {
		key := bindingKey(c.StationID, c.CoreNodeName, c.PayloadCode)
		delete(m.debounce, key)
		delete(m.warmUp, key)
		affectedPayloads[c.PayloadCode] = true
		if m.eng != nil {
			m.eng.dbg("threshold_monitor: reset debounce station=%s loader=%s payload=%s old=%d new=%d",
				c.StationID, c.CoreNodeName, c.PayloadCode, c.OldThreshold, c.NewThreshold)
		}
	}
	m.mu.Unlock()

	// The denominator moved, so the episode ends here and the re-evaluation
	// inside engagePayloads opens a fresh one if the place is still hungry.
	// Carrying one episode across the change would measure it against a
	// threshold that was not in force for most of its life.
	//
	// It runs BEFORE engagePayloads, or the re-evaluation would find the old
	// episode still open, treat the level as already-stamped, and the new
	// threshold would never get an episode of its own.
	m.closeThresholdEpisodesForChangedBindings(changes)

	m.engagePayloads(affectedPayloads)
}

// Resync re-engages the monitor's bindings for one station from demand_registry,
// firing any binding already below threshold. Called when an Edge (re)connects.
//
// The startup sweep reads demand_registry once, ~3s after Core boot. But the
// registry is populated out-of-band: seeddev and migrateloaders write it directly
// (separate processes that can't notify a running monitor), and the live runtime
// trigger (loader config edit → OnThresholdChanges) only fires for edits made
// through the loader UI, not for seed/CLI writes. Without a re-engage on
// (re)connect, a binding seeded after the startup sweep stays dark until Core
// restarts — exactly the dev-sim symptom (seed populates the registry, edge
// restarts, but C-push never fires).
//
// Idempotent: engagePayloads only adds bindings and fires those below threshold;
// the Edge's reservation seam dedups any redundant signal (never-2N), so
// re-firing a still-below binding on a reconnect is safe.
func (m *ThresholdMonitor) Resync(stationID string) {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		m.eng.logFn("threshold_monitor: Resync(%s) list thresholds: %v", stationID, err)
		return
	}
	affected := make(map[string]bool)
	m.mu.Lock()
	for _, e := range entries {
		if e.StationID != stationID || e.ReplenishUOPThreshold <= 0 {
			continue
		}
		// Clear debounce/warm-up so an already-below binding fires immediately on
		// (re)connect instead of waiting out the window.
		key := bindingKey(e.StationID, e.CoreNodeName, e.PayloadCode)
		delete(m.debounce, key)
		delete(m.warmUp, key)
		affected[e.PayloadCode] = true
	}
	m.mu.Unlock()
	if len(affected) == 0 {
		return
	}
	m.eng.logFn("threshold_monitor: Resync station=%s — re-engaging %d monitored payload(s)", stationID, len(affected))
	m.engagePayloads(affected)
}

// engagePayloads (re)builds the binding cache for each affected payload from
// demand_registry, seeds its UOP baseline, and fires any binding below threshold.
// Shared by OnThresholdChanges (incremental edits) and Resync ((re)connect).
func (m *ThresholdMonitor) engagePayloads(affectedPayloads map[string]bool) {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	for payload := range affectedPayloads {
		entries, err := m.eng.db.LookupDemandThresholdsByPayload(payload)
		if err != nil {
			m.eng.logFn("threshold_monitor: engagePayloads rebuild for %s: %v", payload, err)
			continue
		}
		tes := make([]thresholdEntry, 0, len(entries))
		for _, e := range entries {
			tes = append(tes, thresholdEntry{
				stationID:    e.StationID,
				coreNodeName: e.CoreNodeName,
				payloadCode:  e.PayloadCode,
				threshold:    e.ReplenishUOPThreshold,
				loaderID:     e.LoaderID,
			})
		}
		// Which bindings SURVIVED the rebuild. Any open episode for this payload
		// whose binding is not among them lost its precondition — the config
		// went away underneath a live demand, and this is the only site that
		// can notice.
		live := make(map[string]bool, len(tes))
		for _, te := range tes {
			live[bindingKey(te.stationID, te.coreNodeName, te.payloadCode)] = true
		}
		m.closeThresholdEpisodesForPayloadNotIn(payload, live)

		m.mu.Lock()
		if len(tes) == 0 {
			delete(m.thresholdsByPayload, payload)
			m.mu.Unlock()
			continue
		}
		m.thresholdsByPayload[payload] = tes
		m.mu.Unlock()

		// Read the authoritative in-loop sum and evaluate. This is the same
		// single-source-of-truth read the hot path uses, just issued eagerly on
		// a config edit (OnThresholdChanges) or an Edge (re)connect (Resync) —
		// the moment an engineer is actively correcting the system's belief.
		// There is no cached tally to "re-baseline" against anymore; the private
		// copy that once sat at ~139 while DB truth was 31 (Springfield
		// 2026-07-21, threshold nudged 120→121→120, fired nothing) is gone, so
		// "re-baseline" collapsed to "read".
		//
		// UNLIKE the hot path, this path deliberately fires on a read error
		// (total falls through to 0): a config edit that hits a transient DB
		// error should still arm a zero-stock payload rather than stay silent.
		total, err := m.readTotal(context.Background(), payload)
		if err != nil {
			m.eng.logFn("threshold_monitor: engagePayloads read for %s: %v", payload, err)
			total = 0
		}

		m.checkBindings(tes, total, "below_threshold", false)
	}
}
