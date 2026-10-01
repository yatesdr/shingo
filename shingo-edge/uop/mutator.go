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

// Mutator is the verb surface for the slot-lifecycle writes — the
// active-bin pointer, the cached count and the stamp — and for the delta
// accumulator (bin deltas and pile levels). It wraps that private
// accumulator plus narrow store interfaces (runtimeWriter for runtime-row
// writes, bucketStore for the lineside pile writes and reads its verbs make).
//
// IT IS NOT THE ONLY WRITER OF THE RUNTIME ROW, and this used to call itself
// the engine's chokepoint for UOP state. The engine still writes directly: the
// order pointers, active_pull, the lineside identity (through its own doorway,
// recordLinesideCarrier), the per-tick count (UpdateProcessNodeUOP and the
// clear-pending write in wiring_counter_delta.go), the manifest count at
// release, the record-count fence, and the stamp-only epoch refresh. What IS
// pinned is the pointer/count/stamp setters: TestArch_ActiveBinPointerWritersAreKnown
// names every file allowed to call them raw.
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
// orchestration step changes attribution context. Today's caller is the
// release trunk's commitFlip (fact-owners Lane G; the operator flip
// door FlipABNode was deleted with its last caller), which calls this
// before writePullSide swaps which side is active-pull. Engine owns the
// orchestration; UOP owns the flush.
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

// BindFromCore writes claim + active_bin_id + epoch + count as Core states
// them: Core's word on which carrier is at the slot, its count and its
// generation. Callers:
//
//   - handler_uop_adjustment.go Bound arm (admin Move put this carrier here);
//   - handler_uop_adjustment.go count arm (a count for the carrier already
//     bound here — binID is the bound one, the count and stamp are Core's;
//     the stamp only moves forward, see the store's epochAssignOnBind);
//   - operator_changeover_cancel.go reconcile (Core's physical view after a
//     cancelled changeover rebinds the slot).
//
// Same statement as ManualLoad; a different plant event, so a different name
// at the call site. The identity is not written here: a Core announcement
// carries no payload, and the callers record UNKNOWN through the doorway
// where a new carrier arrived.
func (m *Mutator) BindFromCore(nodeID int64, activeClaimID *int64, binID *int64, deltaEpoch int64, uop int) error {
	return m.rw.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, activeClaimID, binID, deltaEpoch, uop)
}

// BindStagedUnlessDeparted binds a staged carrier into a slot read as empty,
// from a person's count correction (handler_uop_adjustment.go), unless it is
// the carrier that last left this slot arriving at an older stamp — a late
// correction for a carrier that has moved on. Reports whether it bound. See
// processes.BindEmptySlotUnlessDeparted for the WHERE-clause guard.
func (m *Mutator) BindStagedUnlessDeparted(nodeID int64, activeClaimID *int64, binID, deltaEpoch int64, uop int) (bool, error) {
	return m.rw.BindEmptySlotUnlessDeparted(nodeID, activeClaimID, binID, deltaEpoch, uop)
}

// ClearActiveBin clears the active bin pointer AND zeroes the cached count on
// a process node's runtime row, in one statement: the pointer/count half of
// "the carrier left". Its one production caller is Engine.carrierLeft
// (engine/carrier_left.go), which records the identity half through the
// lineside doorway right after — the doorway is an engine method, and uop
// cannot call back into the engine (engine imports uop), so the verb that
// composes both halves lives there. Every door that nulls active_bin_id goes
// through that verb (TestArch_CarrierLeavesThroughOneVerb).
//
// THE COUNT GOES WITH THE BIN. The count on the row is the departed carrier's;
// held over, every reader in the pickup→delivery window sees material that is
// no longer there — flipTargetReady's changeover arm reads it as "holds no
// material to feed the line" only by the accident of the value's sign, and the
// demand reconciler reads it as stock.
//
// THE CLAIM IS LEFT ALONE. A sibling verb (ClearActiveAndReset) used to write
// active_claim_id in the same statement; every caller passed the value it had
// just read from the same row, so it was a no-op write, and the two verbs
// collapsed into this one.
//
// ActiveOrderID IS DELIBERATELY LEFT ALONE — it is the cell-busy pointer the
// admission guards read, released by orderWorksTheCell on terminal/departure,
// not by the pickup.
func (m *Mutator) ClearActiveBin(nodeID int64) error {
	return m.rw.ClearProcessNodeActiveBinAndCount(nodeID)
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
// auto-clear on delivery). Each then records the carrier as known-empty,
// said by the operator, through the engine's lineside doorway — the carrier
// stays, so this is not a departure.
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
