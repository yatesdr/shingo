// mutator.go — public surface of the uop package.
//
// A shell over the package-private accumulator satisfying the engine's
// InventoryDeltaSink interface, organised into segregated interfaces
// (Ticker, SlotWriter, Capturer, Piles, Pickup, Boundary; see
// interfaces.go).
package uop

import (
	"time"

	"shingo/protocol/types"
	"shingoedge/store"
)

// DebugLogFunc is a nil-safe debug logging function. Mirrors the
// messaging-package alias so callers don't need a new import path
// once they switch to uop.
type DebugLogFunc = types.DebugLogFunc

// Mutator is the engine's chokepoint for UOP state mutations. Wraps a
// private accumulator (bin deltas and pile levels) plus narrow store
// interfaces (runtimeWriter for runtime-row writes, bucketStore for the
// lineside pile writes and reads its verbs make).
type Mutator struct {
	acc     *accumulator
	rw      runtimeWriter
	buckets bucketStore
}

// New constructs a Mutator for the given Edge identity. Caller wires
// DebugLog / interval (or leaves them defaulted) before calling Start.
//
// rw and buckets are the narrow store surfaces. *store.DB satisfies
// both. Pass nil only in tests that don't exercise the corresponding
// verbs — verbs that need a nil dependency will panic with a nil
// dereference rather than silently misbehave.
func New(db *store.DB, stationID string, rw runtimeWriter, buckets bucketStore) *Mutator {
	return &Mutator{
		acc:     newAccumulator(db, stationID),
		rw:      rw,
		buckets: buckets,
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

// Flush performs one synchronous flush pass. Boundary triggers
// (operator release, A/B flip, bin pickup, loader confirm) call this.
func (m *Mutator) Flush() { m.acc.flush() }

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
// logs failures and leaves the entry for the next flush rather than
// returning them). Callers should treat a returned error as "flush failed — do
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

// BindActiveBin writes the active bin pointer and epoch on a process
// node's runtime row. Today's caller is operator_bin_ops.go:100 (L1
// retrieve confirm — operator confirmed the empty bin physically
// arrived at the loader). Does NOT touch count or claim — the loader's
// bin arrival is the only state change at this moment. deltaEpoch is
// the bin's load-lifecycle epoch from Core's LoadBin response; it
// seeds the active_bin_epoch so subsequent BinUOPDeltas carry the
// right generation for Core's dedup.
func (m *Mutator) BindActiveBin(nodeID, binID int64, deltaEpoch int64) error {
	return m.rw.SetProcessNodeActiveBinIDAndEpoch(nodeID, &binID, deltaEpoch)
}

// ClearActiveBin clears the active bin pointer AND zeroes the cached count on
// a process node's runtime row, in one statement. Today's caller is
// handler_bin_picked_up.go (Core BinPickedUp arrival — the bin has physically
// left the slot, so any subsequent ticks attribute to nothing rather than to
// the now-gone bin).
//
// THE COUNT GOES WITH THE BIN. The count on the row is the departed carrier's;
// held over, every reader in the pickup→delivery window sees material that is
// no longer there — flipTargetReady's changeover arm reads it as "holds no
// material to feed the line" only by the accident of the value's sign, and the
// demand reconciler reads it as stock. Zeroing at the pickup is the same
// atomic semantics as ClearActiveAndReset (both land the slot in "empty and
// knows it"), differing only in leaving the claim and order pointers for the
// callers that own them.
//
// ActiveOrderID IS DELIBERATELY LEFT ALONE — it is the cell-busy pointer the
// admission guards read, released by orderWorksTheCell on terminal/departure,
// not by the pickup.
func (m *Mutator) ClearActiveBin(nodeID int64) error {
	return m.rw.ClearProcessNodeActiveBinAndCount(nodeID)
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

// SetClaimCountAndEpoch is SetClaimAndCount plus the carrier's new
// generation stamp: the clear shape. Clearing a carrier for reuse starts a
// new life for it on Core — Core bumps the stamp and returns it in the same
// reply — but the carrier does not move, so the bin pointer is untouched
// and only the count and the stamp change.
//
// binID is the carrier Core's reply named. The stamp lands only if that is
// the carrier bound at this slot: Core resolves which carrier to clear from
// its own view of the node, and a reply naming one the Edge is not holding
// is about a carrier that is not here.
//
// Callers: the three routes that clear a carrier through Core —
// operator_bin_ops.go (the operator's CLEAR on a manual-swap window),
// operator_home_consolidation.go (zeroing a loader home before the
// consolidation move), and wiring_delivered.go (the market-pullback
// auto-clear on delivery).
func (m *Mutator) SetClaimCountAndEpoch(nodeID int64, activeClaimID *int64, uop int, binID, deltaEpoch int64) error {
	return m.rw.SetProcessNodeRuntimeClaimCountAndEpoch(nodeID, activeClaimID, uop, binID, deltaEpoch)
}

// OnDelivered atomically writes claim + active_bin_id + active_bin_epoch
// + count when a bin physically arrives at the slot. Today's caller
// is wiring_delivered.go:82 (delivery completion handler). Binds
// active_bin_id to the delivered bin so PLC ticks attribute to it.
//
// uop and deltaEpoch are seeded from the OrderDelivered envelope (the
// bin's authoritative count + load-lifecycle epoch carried by Core at
// arrival), or a configured fallback when the envelope omits them;
// wiring_delivered.go owns the resolution decision today.
//
// activeClaimID is a pointer so the caller can thread the existing
// runtime.ActiveClaimID through (or set a fresh claim for the
// to-style on changeover).
func (m *Mutator) OnDelivered(nodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, uop int) error {
	return m.rw.SetProcessNodeRuntimeForDeliveredBin(nodeID, activeClaimID, binID, deltaEpoch, uop)
}

// ManualLoad atomically writes claim + active_bin_id + epoch + count
// when an operator imprints a bin via the loader fallback path. Today's
// caller is seatManuallyLoadedBin in operator_bin_ops.go.
//
// The count is CORE'S, off the LoadBin response — the number Core resolved
// and wrote to the ledger. It used to be the load form's, which was the same
// number whenever the operator declared one and a locally-invented fallback
// whenever they did not.
//
// binID is *int64 because Core's LoadBin response may not include a
// bin identity (multi-bin order, pre-fix Core build); in that case
// active_bin_id is nulled to make the absence explicit rather than
// leaving a stale pointer behind.
//
// deltaEpoch is the bin's load-lifecycle epoch from Core's LoadBin
// response (Core bumps it atomically in SetForProduction). It seeds
// active_bin_epoch so subsequent BinUOPDeltas carry the right
// generation for Core's epoch-aware dedup.
func (m *Mutator) ManualLoad(nodeID int64, activeClaimID *int64, binID *int64, deltaEpoch int64, uop int) error {
	return m.rw.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, activeClaimID, binID, deltaEpoch, uop)
}
