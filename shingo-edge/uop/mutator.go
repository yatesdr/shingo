// mutator.go — public surface of the uop package.
//
// Phase 1 scope: a thin shell over the package-private accumulator
// satisfying the engine's InventoryDeltaSink interface
// (RecordBin/RecordBucket/Flush/FlushFailures). Later phases grow this
// surface into segregated interfaces (Ticker, SlotWriter, Capturer,
// Pickup, Boundary, Backfiller) without further changes to the
// composition root.
package uop

import (
	"time"

	"shingo/protocol"
	"shingo/protocol/types"
	"shingoedge/store"
)

// DebugLogFunc is a nil-safe debug logging function. Mirrors the
// messaging-package alias so callers don't need a new import path
// once they switch to uop.
type DebugLogFunc = types.DebugLogFunc

// Mutator is the engine's chokepoint for UOP state mutations. Wraps a
// private accumulator (for delta emission) plus narrow store
// interfaces (runtimeWriter for runtime-row writes, bucketStore for
// lineside-bucket reads/writes, nodeStore for process_node reads).
// Phase 3a grows this type with intent verbs that own the per-call-
// site decisions the engine makes today.
type Mutator struct {
	acc     *accumulator
	rw      runtimeWriter
	buckets bucketStore
	nodes   nodeStore
}

// New constructs a Mutator for the given Edge identity. Caller wires
// DebugLog / interval (or leaves them defaulted) before calling Start.
//
// rw, buckets, and nodes are the narrow store surfaces. *store.DB
// satisfies all three. Pass nil only in tests that don't exercise
// the corresponding verbs — verbs that need a nil dependency will
// panic with a nil dereference rather than silently misbehave.
func New(db *store.DB, stationID string, rw runtimeWriter, buckets bucketStore, nodes nodeStore) *Mutator {
	return &Mutator{
		acc:     newAccumulator(db, stationID),
		rw:      rw,
		buckets: buckets,
		nodes:   nodes,
	}
}

// SetDebugLog installs a debug logging function. Safe to call before
// Start; not safe to call after.
func (m *Mutator) SetDebugLog(fn DebugLogFunc) {
	m.acc.debugLog = fn
}

// SetInterval overrides the periodic flush cadence. Intended for the
// composition root reading YAML; unsafe to call after Start.
func (m *Mutator) SetInterval(d time.Duration) {
	m.acc.setInterval(d)
}

// Start begins the periodic flush loop.
func (m *Mutator) Start() { m.acc.start() }

// Stop halts the periodic loop and runs one final flush. Idempotent.
func (m *Mutator) Stop() { m.acc.stop() }

// RecordBin accumulates a signed delta against a specific bin under
// the given reason. Satisfies engine.InventoryDeltaSink. epoch is the
// bin's load-lifecycle epoch — caller resolves it from the runtime
// bin-state cache so Core's epoch-aware dedup accepts the delta.
func (m *Mutator) RecordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason, epoch int64) {
	m.acc.recordBin(binID, payloadCode, delta, reason, epoch)
}

// RecordBucket accumulates a signed delta against a specific lineside
// bucket. Satisfies engine.InventoryDeltaSink.
//
// coreNodeName is the cross-system identifier that goes on the wire
// (Round-3 Obs 8). Empty values are dropped at flush rather than
// emitted into the (Core-rejected) cross-namespace translation hole.
//
// payloadCode (UOP-threshold replenishment) is the payload these parts
// belong to so Core's SystemUOPForPayload can sum bins + buckets per
// payload. Empty string is valid for callers that don't have the
// payload handy (e.g. uop_backfill startup, where the local row
// doesn't carry it) — the accumulator preserves any previously-latched
// non-empty value and Core's UPSERT preserves the existing row value.
func (m *Mutator) RecordBucket(nodeID int64, coreNodeName, pairKey string, styleID int64, partNumber, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason) {
	m.acc.recordBucket(nodeID, coreNodeName, pairKey, styleID, partNumber, payloadCode, delta, reason)
}

// Flush performs one synchronous flush pass. Boundary triggers
// (operator release, A/B flip, bin pickup, loader confirm) call this.
// Satisfies engine.InventoryDeltaSink.
func (m *Mutator) Flush() { m.acc.flush() }

// FlushFailures returns the cumulative count of EnqueueOutbox failures
// across the bin and bucket flush paths since process start. Surfaces
// via engine metrics for outbox-health dashboards.
func (m *Mutator) FlushFailures() int64 { return m.acc.flushFailures.Load() }

// OnBinPickedUp flushes pending deltas at the bin-pickup boundary.
// Today's caller is HandleBinPickedUp at handler_bin_picked_up.go:108,
// called before the runtime row's order pointer + active_bin_id are
// cleared. The flush MUST happen before the slot pointers clear so
// any in-flight ticks still attribute to the bin that was physically
// at the slot (the one that just got picked up). After the flush
// returns, the engine's race-guarded ClearActiveBin call clears the
// pointer.
//
// Semantically distinct from MarkAttributionBoundary (which fires
// before a node-routing flip): OnBinPickedUp fires when a specific
// bin has physically left the slot. Implementation today is the
// same (flush the accumulator) but the call sites are different
// plant events.
func (m *Mutator) OnBinPickedUp(nodeID *int64) error {
	_ = nodeID
	m.acc.flush()
	return nil
}

// MarkAttributionBoundary flushes pending deltas before a non-UOP
// orchestration step changes attribution context. Today's caller is
// FlipABNode, which calls this before SetActivePull swaps which side
// is active-pull. Engine owns the orchestration; UOP owns the flush.
//
// MUST flush synchronously. The error signature is the forward-shape
// for when the accumulator flush gains error propagation; the current
// implementation returns nil unconditionally (the underlying flush
// logs failures and increments FlushFailures rather than returning
// them). Callers should treat a returned error as "flush failed — do
// not proceed with the downstream attribution change."
//
// nodeID identifies the boundary the caller is about to cross.
// Reserved for future per-node flush optimization; the current
// implementation flushes globally.
func (m *Mutator) MarkAttributionBoundary(nodeID int64) error {
	_ = nodeID
	m.acc.flush()
	return nil
}

// BindActiveBin writes the active bin pointer on a process node's
// runtime row. Today's caller is operator_bin_ops.go:100 (L1 retrieve
// confirm — operator confirmed the empty bin physically arrived at
// the loader). Does NOT touch count, claim, or cached bin pointer —
// the loader's bin arrival is the only state change at this moment.
func (m *Mutator) BindActiveBin(nodeID, binID int64) error {
	return m.rw.SetProcessNodeActiveBinID(nodeID, &binID)
}

// ClearActiveBin clears the active bin pointer on a process node's
// runtime row. Today's caller is handler_bin_picked_up.go:126 (Core
// BinPickedUp arrival — the bin has physically left the slot, so any
// subsequent ticks attribute to nothing rather than to the now-gone
// bin). Does NOT touch count, claim, or cached bin pointer.
func (m *Mutator) ClearActiveBin(nodeID int64) error {
	return m.rw.SetProcessNodeActiveBinID(nodeID, nil)
}

// PrepareIncoming writes the cached bin pointer + count to the
// incoming supply bin's identity. Today's caller is
// operator_release.go:358 (operator release click — operator declares
// the old bin is leaving; cached bin pointer flips to whichever
// supply bin is en route to this slot, with whatever UOP the order
// manager resolved). Does NOT touch active_bin_id (still the outgoing
// bin until pickup) or claim. This is what opens the gap window:
// after this write, active_bin_id != cached_bin_id until delivery.
//
// uop is passed explicitly — do NOT derive from claim.UOPCapacity.
// SEND PARTIAL BACK preserves the runtime cache value rather than
// resetting to capacity, and capture/lineside-only paths can pass 0.
// The caller knows what value belongs on the runtime row.
func (m *Mutator) PrepareIncoming(nodeID int64, incomingBinID int64, uop int) error {
	return m.rw.SetProcessNodeCachedBin(nodeID, &incomingBinID, uop)
}

// ClearCache nulls the cached bin pointer and zeros the count. Today's
// caller is operator_produce.go:151 (produce-side reset — the produce
// node finalized a bin and there's no incoming bin identity yet).
// Does NOT touch active_bin_id or claim.
func (m *Mutator) ClearCache(nodeID int64) error {
	return m.rw.SetProcessNodeCachedBin(nodeID, nil, 0)
}

// ClearActiveAndReset atomically clears active_bin_id and zeros the
// count while preserving the claim. Today's caller is
// wiring_completion.go:181 (Order B completion at supermarket — the
// evac bin has been delivered to the supermarket, the slot it left
// from has no bin until the next supply arrives, but the claim
// continues for the next bin).
//
// activeClaimID is passed by the caller (a pointer so the existing
// runtime.ActiveClaimID can be threaded through unchanged). Atomic
// because a tick firing between two separate writes (clear active +
// set count) could attribute to a stale active_bin_id with the new
// count, or vice versa.
func (m *Mutator) ClearActiveAndReset(nodeID int64, activeClaimID *int64) error {
	return m.rw.SetProcessNodeRuntimeWithBin(nodeID, activeClaimID, nil, 0)
}

// SetClaimAndCount writes claim + count without touching either bin
// pointer. Today's callers:
//
//   - operator_bin_ops.go:200 (ClearBin manual-swap unloader empty-out)
//   - operator_node_changeover.go:260 (switch-node UOP reset during changeover)
//   - changeover_restore.go:75 (engine-startup safety net for in-progress changeovers)
//   - wiring_completion.go:149 (count carry-forward on staged-delivery)
//
// All three field-shape-identical: claim stays meaningful, count
// goes to a known value, bin pointers untouched. Caller decides the
// uop value; verb does not derive.
func (m *Mutator) SetClaimAndCount(nodeID int64, activeClaimID *int64, uop int) error {
	return m.rw.SetProcessNodeRuntime(nodeID, activeClaimID, uop)
}

// OnDelivered atomically writes claim + active_bin_id + cached_bin_id
// + count when a bin physically arrives at the slot. Today's caller
// is wiring_delivered.go:82 (delivery completion handler). Brings
// active_bin_id = cached_bin_id = binID so the gap window closes and
// the PLC tick gate resumes cache decrements.
//
// uop is passed explicitly — caller resolves from Core's BinByID
// lookup (or a configured fallback when Core is unreachable;
// wiring_delivered.go owns the resolution decision today).
//
// activeClaimID is a pointer so the caller can thread the existing
// runtime.ActiveClaimID through (or set a fresh claim for the
// to-style on changeover).
func (m *Mutator) OnDelivered(nodeID int64, activeClaimID *int64, binID int64, uop int) error {
	return m.rw.SetProcessNodeRuntimeForDeliveredBin(nodeID, activeClaimID, binID, uop)
}

// AdjustBucket sets a lineside bucket to an exact quantity (not a
// delta), emitting the signed-delta envelope so Core's mirror stays
// in step, and flushing immediately so the audit timeline reflects
// the operator action without waiting for the periodic flush window.
//
// Today's caller is admin_lineside.go (engineer/team-leader override
// for the "Lineside Buckets" admin page). currentQty is passed in so
// the verb doesn't need a separate read against the bucket store;
// the caller already has the bucket row in hand for validation.
// reason is required and explicit per Dev A's review — the
// admin/correction reason is different from capture_fill /
// consume_drain and must not be mixed up.
//
// Skips delta emission when newQty == currentQty (no-op write).
// Still writes the row to update updated_at and refresh the audit
// row, matching pre-refactor behaviour.
func (m *Mutator) AdjustBucket(nodeID int64, coreNodeName, pairKey string, styleID int64, partNumber string, currentQty, newQty int, reason protocol.LinesideBucketDeltaReason) error {
	delta := newQty - currentQty
	if delta != 0 {
		// Admin adjustments don't carry a payload code (the operator UI
		// works in part-number terms); pass empty so any previously-
		// latched payload_code on the bucket is preserved by both the
		// accumulator and Core's UPSERT.
		m.acc.recordBucket(nodeID, coreNodeName, pairKey, styleID, partNumber, "", delta, reason)
	}
	if err := m.buckets.SetLinesideBucketForReconcile(nodeID, pairKey, styleID, partNumber, newQty); err != nil {
		return err
	}
	m.acc.flush()
	return nil
}

// ManualLoad atomically writes claim + active_bin_id + count when
// an operator imprints a bin via the loader fallback path. Today's
// caller is operator_bin_ops.go:128 (the fallback path that takes a
// uop count from the load form rather than from Core's response).
//
// binID is *int64 because Core's LoadBin response may not include a
// bin identity (multi-bin order, pre-fix Core build); in that case
// active_bin_id is nulled to make the absence explicit rather than
// leaving a stale pointer behind.
//
// Note this does NOT also set cached_bin_id; the wrap of
// SetProcessNodeRuntimeWithBin (claim + active + count, cached
// untouched) means a follow-up cache write may be needed depending
// on whether the runtime row's cached pointer was already in
// agreement. The current call site doesn't write cached separately.
func (m *Mutator) ManualLoad(nodeID int64, activeClaimID *int64, binID *int64, uop int) error {
	return m.rw.SetProcessNodeRuntimeWithBin(nodeID, activeClaimID, binID, uop)
}
