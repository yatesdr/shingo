// threshold_monitor.go — UOP-threshold replenishment, Core side.
// See shingo/docs/uop-threshold-replenishment.md for the design
// overview.
//
// The C-push architecture in one paragraph:
//
//   Edge owns claim config and ships per-(loader, payload) thresholds
//   to Core via ClaimSync. Core observes combined in-loop UOP (bins +
//   buckets) per payload on every bin update / bucket delta apply.
//   When the total drops below the configured threshold for a (loader,
//   payload) pair, Core emits a LoopBelowThresholdSignal on subject
//   demand.loop_below_threshold. Edge responds by firing L1 retrieve_empty
//   after its in-flight guard.
//
// Debounce policy: 15-second window per (loader_node, payload).
// In-memory state (lost on Core restart — that's intentional; the
// startup sweep handles the restart case). The debounce timer is
// reset on SyncRegistry when the threshold value for the pair changes,
// so a newly-applied threshold engages immediately.
//
// Startup sweep: on Run() the monitor walks every binding with
// threshold > 0 once, bypassing debounce. After the sweep, normal
// debounced operation begins. This handles the case where Core was
// down during a threshold-crossing event.
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

// ThresholdMonitor watches combined bin + bucket UOP per payload and
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
}

// NewThresholdMonitor constructs the monitor. Call Run() to perform
// the startup sweep and then have it react to BinUpdated /
// LinesideBucketApplied events.
func NewThresholdMonitor(e *Engine) *ThresholdMonitor {
	return &ThresholdMonitor{
		eng:      e,
		debounce: make(map[string]time.Time),
		warmUp:   make(map[string]int),
	}
}

// bindingKey composes the (station, core_node_name, payload) tuple
// used to key per-binding state in the threshold monitor's debounce
// and warm-up maps. core_node_name is the canonical cross-system
// identifier — matches loader_payload_thresholds, demand_registry,
// and the LoopBelowThresholdSignal wire field.
func bindingKey(station, coreNodeName, payload string) string {
	return station + "|" + coreNodeName + "|" + payload
}

// Run performs the startup sweep then subscribes to the engine event
// bus for ongoing monitoring. Idempotent — calling twice is harmless;
// the second call is a no-op because the sweep flag stays set.
//
// Sweep runs in a goroutine so it doesn't block Engine startup; ordering
// vs. uop_backfill is handled at the cmd/shingocore wiring layer
// (sweep runs after a backfill-completion gate). For Phase 1 the
// monitor itself just waits a short grace period before sweeping; the
// brief flags persistent sweep ordering as a deploy-checklist item.
func (m *ThresholdMonitor) Run(ctx context.Context) {
	// One-shot startup sweep on a goroutine. Subscriptions are wired
	// from wireEventHandlers so the subscription side is up before the
	// sweep fires its first signal.
	go func() {
		// Brief grace period to let uop_backfill from any reconnecting
		// Edge clear the inventory_delta_dedup pipeline before we read
		// SystemUOPForPayload. Phase 1 deploy-checklist documents the
		// strict ordering for plants where this matters; the grace
		// period is a safety belt for the typical case.
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		m.startupSweep(ctx)
	}()
}

// startupSweep iterates every (loader, payload) with threshold > 0
// once, bypassing the debounce check. After the sweep, normal
// debounced operation begins. Errors are logged and the sweep
// continues — a single failed binding shouldn't stop the others from
// being evaluated.
func (m *ThresholdMonitor) startupSweep(ctx context.Context) {
	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		m.eng.logFn("threshold_monitor: startup sweep ListDemandThresholds: %v", err)
		// Even on lookup failure, mark sweep done so steady-state
		// monitoring isn't stuck in cold-start mode forever.
		m.mu.Lock()
		m.sweepDone = true
		m.mu.Unlock()
		return
	}
	m.eng.logFn("threshold_monitor: startup sweep — evaluating %d monitored bindings", len(entries))

	// Group by payload so SystemUOPForPayload is called once per
	// distinct payload rather than once per (station, loader)
	// binding — multiple loaders that monitor the same payload share
	// the lookup.
	byPayload := map[string][]demands.RegistryEntry{}
	for _, e := range entries {
		byPayload[e.PayloadCode] = append(byPayload[e.PayloadCode], e)
	}

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
		for _, b := range bindings {
			if b.ReplenishUOPThreshold <= 0 {
				continue
			}
			if total < b.ReplenishUOPThreshold {
				// Seed the warm-up cap so subsequent signals on the
				// same binding continue firing during the cold-start
				// catch-up window.
				cap := warmUpFloor
				m.mu.Lock()
				m.warmUp[bindingKey(b.StationID, b.CoreNodeName, b.PayloadCode)] = cap
				m.mu.Unlock()
				m.fireSignal(b, total, "warm_up_startup_sweep")
			}
		}
	}

	m.mu.Lock()
	m.sweepDone = true
	m.mu.Unlock()
	m.eng.logFn("threshold_monitor: startup sweep complete — switching to debounced mode")
}

// evaluatePayload is the steady-state path. Called from event
// handlers (BinUpdated, LinesideBucketApplied) — looks up monitored
// bindings for the affected payload, recomputes loop UOP, and dispatches
// LoopBelowThresholdSignal for any binding below threshold whose
// debounce window has elapsed.
func (m *ThresholdMonitor) evaluatePayload(payloadCode string) {
	if payloadCode == "" {
		return
	}
	entries, err := m.eng.db.LookupDemandThresholdsByPayload(payloadCode)
	if err != nil {
		m.eng.logFn("threshold_monitor: LookupDemandThresholdsByPayload(%s): %v", payloadCode, err)
		return
	}
	if len(entries) == 0 {
		return
	}
	uop, err := m.eng.inventoryService.SystemUOPForPayload(context.Background(), []string{payloadCode})
	if err != nil {
		m.eng.logFn("threshold_monitor: SystemUOPForPayload(%s): %v", payloadCode, err)
		return
	}
	var total int
	if len(uop.Counts) > 0 {
		total = uop.Counts[0].TotalUOP
	}
	for _, b := range entries {
		if b.ReplenishUOPThreshold <= 0 {
			continue
		}
		if total >= b.ReplenishUOPThreshold {
			continue
		}
		key := bindingKey(b.StationID, b.CoreNodeName, b.PayloadCode)
		if !m.allow(key) {
			m.eng.dbg("threshold_monitor: suppress station=%s loader=%s payload=%s total=%d threshold=%d (debounce)",
				b.StationID, b.CoreNodeName, b.PayloadCode, total, b.ReplenishUOPThreshold)
			continue
		}
		m.fireSignal(b, total, "below_threshold")
	}
}

// allow returns true if the binding may fire now under the debounce
// + warm-up policy. Records the firing time on success so a follow-up
// call within the window returns false.
func (m *ThresholdMonitor) allow(key string) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Warm-up override: while the per-binding warm-up counter is
	// positive, allow firing every event regardless of debounce.
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

// fireSignal builds and ships a LoopBelowThresholdSignal to the binding's
// edge station. Caller is responsible for the debounce / warm-up gate.
func (m *ThresholdMonitor) fireSignal(b demands.RegistryEntry, total int, reason string) {
	signal := &protocol.LoopBelowThresholdSignal{
		PayloadCode:  b.PayloadCode,
		CurrentUOP:   total,
		Threshold:    b.ReplenishUOPThreshold,
		CoreNodeName: b.CoreNodeName,
		Reason:       reason,
	}
	if err := m.eng.SendDataToEdge(protocol.SubjectLoopBelowThreshold, b.StationID, signal); err != nil {
		m.eng.logFn("threshold_monitor: send LoopBelowThresholdSignal to %s loader=%s payload=%s: %v",
			b.StationID, b.CoreNodeName, b.PayloadCode, err)
		return
	}
	m.eng.logFn("threshold_monitor: signaled station=%s loader=%s payload=%s current=%d threshold=%d reason=%s",
		b.StationID, b.CoreNodeName, b.PayloadCode, total, b.ReplenishUOPThreshold, reason)
}

// OnRegistryChanges resets per-binding debounce + warm-up state for
// every (loader, payload) whose threshold value moved. Called by
// CoreDataService.handleClaimSync after SyncDemandRegistry returns its
// change list. The clear ensures a freshly-configured threshold takes
// effect on the next inventory event rather than waiting out the
// debounce window from a previous firing.
func (m *ThresholdMonitor) OnRegistryChanges(changes []demands.RegistryChange) {
	if len(changes) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range changes {
		key := bindingKey(c.StationID, c.CoreNodeName, c.PayloadCode)
		delete(m.debounce, key)
		delete(m.warmUp, key)
		if m.eng != nil {
			m.eng.dbg("threshold_monitor: reset debounce station=%s loader=%s payload=%s old=%d new=%d",
				c.StationID, c.CoreNodeName, c.PayloadCode, c.OldThreshold, c.NewThreshold)
		}
	}
}

// OnBucketApplied is invoked by the messaging layer after a successful
// LinesideBucketDelta apply. Emits an engine event so the rest of the
// engine (any other subscriber, including potential future audit
// listeners) sees the bucket motion, then drives evaluation directly
// in line. We don't subscribe to the event for our own re-evaluation
// path because the messaging-layer call already carries the payload
// code we need and going through the event bus would force a redundant
// lookup.
func (m *ThresholdMonitor) OnBucketApplied(station string, nodeID int64, payloadCode string, newQty, delta int, reason protocol.LinesideBucketDeltaReason) {
	m.eng.Events.Emit(Event{Type: EventLinesideBucketApplied, Payload: LinesideBucketAppliedEvent{
		Station:     station,
		NodeID:      nodeID,
		PayloadCode: payloadCode,
		NewQty:      newQty,
		Delta:       delta,
		Reason:      reason,
	}})
	m.evaluatePayload(payloadCode)
}

// handleBinUpdatedForThreshold is the EventBinUpdated subscriber that
// re-evaluates the affected payload. Wired in wireEventHandlers.
func (m *ThresholdMonitor) handleBinUpdated(ev BinUpdatedEvent) {
	m.evaluatePayload(ev.PayloadCode)
}
