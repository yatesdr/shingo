// threshold_monitor.go — UOP-threshold replenishment, Core side.
// See shingo/docs/uop-threshold-replenishment.md for the design
// overview.
//
// The C-push architecture in one paragraph:
//
//   Edge owns claim config and ships per-(loader, payload) thresholds
//   to Core via ClaimSync. On any activity for a monitored payload —
//   a BinUOPDelta, a LinesideBucketDelta, or a non-delta bin mutation —
//   Core re-reads the AUTHORITATIVE combined in-loop UOP for that payload
//   (SystemUOPForPayload = SUM(bins.uop_remaining) + active lineside
//   buckets) and evaluates it against the configured threshold. When the
//   total is below the threshold for a (loader, payload) pair, Core emits
//   a LoopBelowThresholdSignal on subject demand.loop_below_threshold.
//   Edge responds by firing L1 retrieve_empty after its in-flight guard.
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
// Dedup with the legacy DemandSignal path:
//   - Core never sends LoopBelowThresholdSignal for (loader, payload)
//     pairs with threshold = 0 (opt-out — bin-count owned by Edge).
//   - Edge's HandleDemandSignal explicitly skips opted-in pairs.
//   - If both signals race, Edge's countLoaderInFlightEmptyIn guard is
//     the dedup contract — second caller sees inflight≥1 and returns.
//
// Out of scope: iterate-all-claims for inactive styles (R3),
// queued-retrieve safety net at Edge.

package engine

import (
	"context"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/store/demands"
	"shingocore/store/loaders"
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

// negativeLogWindow throttles the broken-ledger refusal line. The floor is
// evaluated on every incoming delta — i.e. every consume tick — so an
// unthrottled log would emit per binding per tick and bury the plant log in
// the one situation where an operator most needs to read it. Once a minute per
// binding is enough to keep the condition visible without becoming the noise.
const negativeLogWindow = 60 * time.Second

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
	loaderID     int64 // the owning loader (cutover); 0 for legacy ClaimSync bindings → no LoaderKey on the signal
}

// ThresholdMonitor tracks in-loop UOP per payload incrementally and
// emits LoopBelowThresholdSignal when a monitored (loader, payload)
// drops below its configured threshold.
type ThresholdMonitor struct {
	eng *Engine

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
	// negativeLogged is the last time the broken-ledger refusal was logged
	// per binding. Deliberately SEPARATE from debounce: debounce is
	// signal-eligibility budget, this is log volume. Sharing one stamp would
	// mean a negative total consumed the binding's right to fire the moment
	// the ledger was corrected.
	negativeLogged map[string]time.Time
	// swapContradiction is the last time a manual-swap request arrived for a
	// payload the ledger read as fully stocked (P2-C9). Keyed by payload_code;
	// drives the Replenishment Health contradiction chip and throttles the log.
	swapContradiction map[string]time.Time
}

// NewThresholdMonitor constructs the monitor. Call Run() to perform
// the startup sweep.
func NewThresholdMonitor(e *Engine) *ThresholdMonitor {
	return &ThresholdMonitor{
		eng:                 e,
		debounce:            make(map[string]time.Time),
		warmUp:              make(map[string]int),
		thresholdsByPayload: make(map[string][]thresholdEntry),
		negativeLogged:      make(map[string]time.Time),
		swapContradiction:   make(map[string]time.Time),
	}
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
	now := time.Now()
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
	total, err := m.readTotal(context.Background(), payloadCode)
	if err != nil {
		if m.eng != nil {
			m.eng.logFn("threshold_monitor: SystemUOPForPayload(%s): %v", payloadCode, err)
		}
		return
	}
	m.checkBindings(bindings, total, reason)
}

// startupSweep iterates every (loader, payload) with threshold > 0,
// builds the per-payload binding cache, seeds the cold-start warm-up
// allowance, and evaluates each binding against an authoritative DB read —
// firing signals for any already below threshold. It holds no UOP tally
// afterwards; every later evaluation reads the DB again.
func (m *ThresholdMonitor) startupSweep(ctx context.Context) {
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
		// checkBindings applies — including the negative-total floor. Restart is
		// exactly when that matters: restarting Core is the remedy an operator
		// reaches for BECAUSE the ledger looks wrong, and the sweep would then
		// fire on the garbage total, twice per binding (warm-up bypasses
		// debounce). One fire decision, one set of guards.
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
		m.checkBindings(tes, total, "warm_up_startup_sweep")
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
// LinesideBucketDelta apply. Emits the engine event for other subscribers, then
// re-reads the authoritative sum and checks thresholds. The event emit is
// unconditional (other subscribers rely on it); the threshold re-evaluation
// short-circuits for unmonitored/empty payloads.
func (m *ThresholdMonitor) OnBucketApplied(station, coreNodeName, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason) {
	m.eng.Events.Emit(Event{Type: EventLinesideBucketApplied, Payload: LinesideBucketAppliedEvent{
		Station:      station,
		CoreNodeName: coreNodeName,
		PayloadCode:  payloadCode,
		Delta:        delta,
		Reason:       reason,
	}})
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
	m.checkBindings(bindings, total, "manual_swap_recheck")
}

// recordSwapContradiction stamps a swap-vs-ledger contradiction for the payload
// if the last one is outside the window, returning whether it was newly
// recorded (i.e. should be logged now). Fixed-window throttle: the log fires
// once per window and the chip reads the stamp for the rest of it.
func (m *ThresholdMonitor) recordSwapContradiction(payloadCode string) bool {
	now := time.Now()
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
func (m *ThresholdMonitor) checkBindings(bindings []thresholdEntry, total int, reason string) {
	// Validity floor. A negative plant-wide in-loop total is never a real
	// demand signal — it is always a broken ledger. bins.uop_remaining is
	// allowed to go negative by SME lock (overpack/underpack), buckets are
	// CHECK (qty >= 0), so a negative SUM means the bin side is wrong, not
	// that the plant owes itself parts. Springfield 2026-07-21 signalled
	// 74577-6SA0A.06 at an in-loop total of −443.
	//
	// Firing on that produces legitimate-LOOKING L1s off garbage input, and
	// the fleet has no way to tell you the number was wrong. Refuse to signal
	// and say so loudly instead — a monitored payload going quiet with a log
	// line beats robot traffic nobody can trace. This is input validation,
	// not a toggle: there is nothing to turn on or off.
	//
	// Zero is NOT rejected: a genuinely out-of-stock payload is real demand.
	if total < 0 {
		for _, b := range bindings {
			if b.threshold <= 0 {
				continue
			}
			// Throttled per binding: this runs on every incoming delta, so an
			// unthrottled line would bury the plant log exactly when it needs
			// reading. shouldLogNegative touches ONLY negativeLogged — the
			// binding's debounce budget is untouched, so it stays immediately
			// eligible for the moment the ledger is corrected.
			if !m.shouldLogNegative(bindingKey(b.stationID, b.coreNodeName, b.payloadCode)) {
				continue
			}
			if m.eng != nil { // nil in the pure unit harness (newTestMonitor)
				m.eng.logFn("threshold_monitor: REFUSING to signal station=%s loader=%s payload=%s — in-loop total is negative (total=%d threshold=%d); the bins ledger for this payload is broken, reconcile it before trusting replenishment (further occurrences suppressed for %s)",
					b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold, negativeLogWindow)
			}
		}
		return
	}
	for _, b := range bindings {
		if b.threshold <= 0 {
			continue
		}
		if total >= b.threshold {
			continue
		}
		key := bindingKey(b.stationID, b.coreNodeName, b.payloadCode)
		if !m.allow(key) {
			m.eng.dbg("threshold_monitor: suppress station=%s loader=%s payload=%s total=%d threshold=%d (debounce)",
				b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold)
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
	now := time.Now()
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
	now := time.Now()
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

// fireSignalCached builds and ships a LoopBelowThresholdSignal from a
// cached threshold entry. Used by checkBindings in steady state and
// by the startup sweep (which constructs a thresholdEntry inline).
func (m *ThresholdMonitor) fireSignalCached(b thresholdEntry, total int, reason string) {
	signal := &protocol.LoopBelowThresholdSignal{
		PayloadCode:  b.payloadCode,
		CurrentUOP:   total,
		Threshold:    b.threshold,
		CoreNodeName: b.coreNodeName,
		// MemberNodeName is the binding's loader member (a dedicated position, or the
		// shared anchor). Today it equals CoreNodeName; the Edge routes the empty to
		// THIS node (the same-payload-two-positions fix). Step 4 splits identity from
		// address — CoreNodeName becomes the loader_key and this stays the address —
		// and populates LoaderKey here (free once demand_registry carries loader_id).
		MemberNodeName: b.coreNodeName,
		Reason:         reason,
	}
	// The loader IDENTITY token (step-4 cutover). The Edge resolves the loader by this
	// instead of CoreNodeName, so a synthetic loader (no anchor node) resolves cleanly.
	// 0 for legacy ClaimSync bindings → empty key → Edge falls back to CoreNodeName.
	if b.loaderID > 0 {
		signal.LoaderKey = loaders.Key(b.loaderID)
	}
	if err := m.eng.SendDataToEdge(protocol.SubjectLoopBelowThreshold, b.stationID, signal); err != nil {
		m.eng.logFn("threshold_monitor: send LoopBelowThresholdSignal to %s loader=%s payload=%s: %v",
			b.stationID, b.coreNodeName, b.payloadCode, err)
		return
	}
	m.eng.logFn("threshold_monitor: signaled station=%s loader=%s payload=%s current=%d threshold=%d reason=%s",
		b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold, reason)
}

// OnThresholdChanges resets per-binding debounce + warm-up state for
// every (loader, payload) whose threshold value moved, and rebuilds
// the in-memory threshold cache for affected payloads. Called by
// CoreDataService.handleClaimSync after SyncDemandRegistry returns its
// change list.
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

	m.engagePayloads(affectedPayloads)
}

// Resync re-engages the monitor's bindings for one station from demand_registry,
// firing any binding already below threshold. Called when an Edge (re)connects.
//
// The startup sweep reads demand_registry once, ~3s after Core boot. But the
// registry is populated out-of-band: seeddev and migrateloaders write it directly
// (separate processes that can't notify a running monitor), and the Edge sends
// no ClaimSync (retired), so the usual runtime trigger
// (handleClaimSync → OnThresholdChanges) never fires for loaders. Without a
// re-engage on (re)connect, a binding seeded after the startup sweep stays dark
// until Core restarts — exactly the dev-sim symptom (seed populates the registry,
// edge restarts, but C-push never fires).
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

		m.checkBindings(tes, total, "below_threshold")
	}
}
