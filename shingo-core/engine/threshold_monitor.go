// threshold_monitor.go — UOP-threshold replenishment, Core side.
// See shingo/docs/uop-threshold-replenishment.md for the design
// overview.
//
// The C-push architecture in one paragraph:
//
//   Edge owns claim config and ships per-(loader, payload) thresholds
//   to Core via ClaimSync. Core tracks combined in-loop UOP (bins +
//   buckets) per payload incrementally — the delta handlers apply each
//   BinUOPDelta and LinesideBucketDelta directly to an in-memory total.
//   When the total drops below the configured threshold for a (loader,
//   payload) pair, Core emits a LoopBelowThresholdSignal on subject
//   demand.loop_below_threshold. Edge responds by firing L1 retrieve_empty
//   after its in-flight guard.
//
// No DB queries on the hot path. The monitor is notified directly by
// the Kafka delta handlers (HandleBinUOPDelta, HandleLinesideBucketDelta)
// which already have the payload code and delta. The EventBinUpdated bus
// is only used for rare non-delta mutations (status changes, manual bin
// moves) which reconcile from the DB since the event doesn't carry a
// UOP delta.
//
// Debounce policy: 15-second window per (loader_node, payload).
// In-memory state (lost on Core restart — that's intentional; the
// startup sweep handles the restart case). The debounce timer is
// reset on SyncRegistry when the threshold value for the pair changes,
// so a newly-applied threshold engages immediately.
//
// Startup sweep: on Run() the monitor walks every binding with
// threshold > 0 once, seeding the UOP cache from the DB. After the
// sweep, all further UOP updates are incremental — no more DB reads.
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
	// OnThresholdChanges. Never queried from the DB on the hot path.
	thresholdsByPayload map[string][]thresholdEntry
	// uopCache is the combined (bin + bucket) UOP total per payload,
	// maintained incrementally. Seeded by the startup sweep from the DB,
	// then updated by OnBinUOPDelta and OnBucketApplied on each Kafka
	// delta. No DB queries after startup.
	uopCache map[string]int
}

// NewThresholdMonitor constructs the monitor. Call Run() to perform
// the startup sweep.
func NewThresholdMonitor(e *Engine) *ThresholdMonitor {
	return &ThresholdMonitor{
		eng:                 e,
		debounce:            make(map[string]time.Time),
		warmUp:              make(map[string]int),
		thresholdsByPayload: make(map[string][]thresholdEntry),
		uopCache:            make(map[string]int),
	}
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

// startupSweep iterates every (loader, payload) with threshold > 0,
// seeds the UOP cache from the DB, and fires signals for any binding
// already below threshold. After the sweep, all UOP updates are
// incremental — no more DB reads.
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

	// Seed UOP cache from DB (only time we query UOP after startup).
	for payload, bindings := range byPayload {
		if ctx.Err() != nil {
			return
		}
		uop, err := m.eng.inventoryService.SystemUOPForPayload(ctx, []string{payload})
		if err != nil {
			m.eng.logFn("threshold_monitor: startup sweep SystemUOPForPayload(%s): %v", payload, err)
			continue
		}
		var total int
		if len(uop.Counts) > 0 {
			total = uop.Counts[0].TotalUOP
		}
		m.mu.Lock()
		m.uopCache[payload] = total
		m.mu.Unlock()
		for _, b := range bindings {
			if total < b.ReplenishUOPThreshold {
				m.mu.Lock()
				m.warmUp[bindingKey(b.StationID, b.CoreNodeName, b.PayloadCode)] = warmUpFloor
				m.mu.Unlock()
				m.fireSignalCached(thresholdEntry{
					stationID:    b.StationID,
					coreNodeName: b.CoreNodeName,
					payloadCode:  b.PayloadCode,
					threshold:    b.ReplenishUOPThreshold,
					loaderID:     b.LoaderID,
				}, total, "warm_up_startup_sweep")
			}
		}
	}

	m.mu.Lock()
	m.sweepDone = true
	m.mu.Unlock()
	m.eng.logFn("threshold_monitor: startup sweep complete — switching to incremental mode")
}

// OnBinUOPDelta applies a bin UOP delta to the cached total and checks
// thresholds. Called by HandleBinUOPDelta after the delta is applied to
// the DB. The delta is known (from the Kafka message) so we apply it
// directly — no DB query needed. Short-circuits for unmonitored
// payloads so the cache doesn't grow entries for payloads no binding
// is watching.
func (m *ThresholdMonitor) OnBinUOPDelta(payloadCode string, delta int) {
	if payloadCode == "" {
		return
	}
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payloadCode]
	if !monitored {
		m.mu.Unlock()
		return
	}
	m.uopCache[payloadCode] += delta
	total := m.uopCache[payloadCode]
	m.mu.Unlock()

	m.checkBindings(bindings, total)
}

// OnBucketApplied is invoked by the messaging layer after a successful
// LinesideBucketDelta apply. Applies the delta to the cached total,
// emits an engine event for other subscribers, and checks thresholds.
// Short-circuits for unmonitored payloads (after the event emit) so
// the cache doesn't grow for payloads no binding is watching.
func (m *ThresholdMonitor) OnBucketApplied(station, coreNodeName, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason) {
	m.eng.Events.Emit(Event{Type: EventLinesideBucketApplied, Payload: LinesideBucketAppliedEvent{
		Station:      station,
		CoreNodeName: coreNodeName,
		PayloadCode:  payloadCode,
		Delta:        delta,
		Reason:       reason,
	}})
	if payloadCode == "" {
		return
	}
	m.mu.Lock()
	bindings, monitored := m.thresholdsByPayload[payloadCode]
	if !monitored {
		m.mu.Unlock()
		return
	}
	m.uopCache[payloadCode] += delta
	total := m.uopCache[payloadCode]
	m.mu.Unlock()

	m.checkBindings(bindings, total)
}

// handleBinUpdated is the EventBinUpdated subscriber for rare non-delta
// bin mutations (status changes, manual moves, corrections). These
// events don't carry a UOP delta, so we reconcile from the DB. This
// path fires infrequently — the primary consumption path goes through
// OnBinUOPDelta instead.
func (m *ThresholdMonitor) handleBinUpdated(ev BinUpdatedEvent) {
	if ev.PayloadCode == "" {
		return
	}
	// Reconcile UOP from DB for this payload.
	if m.eng == nil || m.eng.inventoryService == nil {
		return
	}
	uop, err := m.eng.inventoryService.SystemUOPForPayload(context.Background(), []string{ev.PayloadCode})
	if err != nil {
		m.eng.logFn("threshold_monitor: reconcile SystemUOPForPayload(%s): %v", ev.PayloadCode, err)
		return
	}
	var total int
	if len(uop.Counts) > 0 {
		total = uop.Counts[0].TotalUOP
	}
	m.mu.Lock()
	m.uopCache[ev.PayloadCode] = total
	bindings := m.thresholdsByPayload[ev.PayloadCode]
	m.mu.Unlock()

	m.checkBindings(bindings, total)
}

// checkBindings evaluates all threshold bindings for a given total and
// fires signals for any that are below threshold and past debounce.
func (m *ThresholdMonitor) checkBindings(bindings []thresholdEntry, total int) {
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
		if m.eng != nil { // nil in the pure unit harness (newTestMonitor)
			for _, b := range bindings {
				if b.threshold <= 0 {
					continue
				}
				m.eng.logFn("threshold_monitor: REFUSING to signal station=%s loader=%s payload=%s — in-loop total is negative (total=%d threshold=%d); the bins ledger for this payload is broken, reconcile it before trusting replenishment",
					b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold)
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
		m.fireSignalCached(b, total, "below_threshold")
	}
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

		// RE-BASELINE FROM THE DB, UNCONDITIONALLY.
		//
		// This used to reuse the cached total whenever one existed and only
		// query for a payload that wasn't already monitored. That made a
		// threshold EDIT re-evaluate against whatever the incremental cache
		// had drifted to, which is the one moment an engineer is actively
		// trying to correct the system's belief. On 2026-07-21 that cost a
		// diagnostic loop at Springfield: nudging a threshold 120→121→120
		// fired nothing because the cache sat at ~139 while the DB truth was
		// 31. Resync ((re)connect) shared the same stale path.
		//
		// This does NOT violate the file-header "no DB queries on the hot
		// path" invariant: engagePayloads runs only on a config edit
		// (OnThresholdChanges) or an Edge (re)connect (Resync). The hot path
		// is OnBinUOPDelta / OnBucketApplied, which stay purely incremental.
		total := 0
		if m.eng.inventoryService != nil {
			uop, err := m.eng.inventoryService.SystemUOPForPayload(context.Background(), []string{payload})
			if err != nil {
				m.eng.logFn("threshold_monitor: engagePayloads re-baseline for %s: %v", payload, err)
				// Best-effort: fall through with total=0 so a zero-stock
				// payload still fires. The DB error is logged loudly so
				// the operator can see it.
			} else if len(uop.Counts) > 0 {
				total = uop.Counts[0].TotalUOP
			}
			m.mu.Lock()
			m.uopCache[payload] = total
			m.mu.Unlock()
		}

		m.checkBindings(tes, total)
	}
}
