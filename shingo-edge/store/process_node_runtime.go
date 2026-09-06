package store

// Phase 5b delegate file: process_node_runtime CRUD now lives in
// store/processes/. This file preserves the *store.DB method surface
// so external callers do not need to change.

import "shingoedge/store/processes"

// EnsureProcessNodeRuntime returns the runtime row for a process_node,
// inserting a fresh row when none exists yet.
func (db *DB) EnsureProcessNodeRuntime(processNodeID int64) (*processes.RuntimeState, error) {
	return processes.EnsureRuntime(db.DB, processNodeID)
}

// GetProcessNodeRuntime returns the runtime row for a process_node.
func (db *DB) GetProcessNodeRuntime(processNodeID int64) (*processes.RuntimeState, error) {
	return processes.GetRuntime(db.DB, processNodeID)
}

// SetProcessNodeRuntime updates the active claim and remaining UOP on
// a runtime row. Does not touch active_bin_id — callers that need
// atomic bin-pointer turnover should use SetProcessNodeRuntimeWithBin.
func (db *DB) SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error {
	return processes.SetRuntime(db.DB, processNodeID, activeClaimID, remainingUOP)
}

// SetProcessNodeRuntimeClaimCountAndEpoch writes the claim and the count
// and advances the stamp for the carrier the write names, leaving the bin
// pointer alone. Used by the clear routes, where Core starts the carrier's
// next life and returns the new stamp but the carrier stays put.
func (db *DB) SetProcessNodeRuntimeClaimCountAndEpoch(processNodeID int64, activeClaimID *int64, remainingUOP int, binID, deltaEpoch int64) error {
	return processes.SetRuntimeClaimCountAndEpoch(db.DB, processNodeID, activeClaimID, remainingUOP, binID, deltaEpoch)
}

// SetProcessNodeRuntimeWithBin updates active_claim_id, active_bin_id,
// and remaining_uop_cached atomically. Used by the CLEAR-shaped writes —
// ClearActiveAndReset and the changeover-cancel reconcile — where the slot ends
// up empty and no epoch is in hand. Deliveries use
// SetProcessNodeRuntimeForDeliveredBin; the completion-handler callers this
// once named were removed with the old delivery handler.
func (db *DB) SetProcessNodeRuntimeWithBin(processNodeID int64, activeClaimID, activeBinID *int64, remainingUOP int) error {
	return processes.SetRuntimeWithBin(db.DB, processNodeID, activeClaimID, activeBinID, remainingUOP)
}

// SetProcessNodeActiveBinID writes only the active bin pointer on a
// runtime row. Used by the bin-pickup handler to clear ownership
// without disturbing the claim or count.
func (db *DB) SetProcessNodeActiveBinID(processNodeID int64, activeBinID *int64) error {
	return processes.SetActiveBinID(db.DB, processNodeID, activeBinID)
}

// SetProcessNodeActiveBinIDAndEpoch writes the active bin pointer and
// epoch together. Used by BindActiveBin (loader L1 confirm) where
// Core's LoadBin response provides the epoch.
func (db *DB) SetProcessNodeActiveBinIDAndEpoch(processNodeID int64, activeBinID *int64, deltaEpoch int64) error {
	return processes.SetActiveBinIDAndEpoch(db.DB, processNodeID, activeBinID, deltaEpoch)
}

// SetProcessNodeRuntimeWithBinAndEpoch updates active_claim_id,
// active_bin_id, active_bin_epoch, and remaining_uop_cached atomically.
// Used by ManualLoad (operator imprint) where Core's LoadBin response
// provides the epoch.
func (db *DB) SetProcessNodeRuntimeWithBinAndEpoch(processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error {
	return processes.SetRuntimeWithBinAndEpoch(db.DB, processNodeID, activeClaimID, activeBinID, deltaEpoch, remainingUOP)
}

// SetProcessNodeRuntimeForDeliveredBin writes active_claim_id,
// active_bin_id, active_bin_epoch, and remaining_uop_cached atomically
// when a bin physically arrives at the slot. deltaEpoch is the arrived
// bin's load-lifecycle epoch (from the OrderDelivered envelope) so
// subsequent tick deltas carry the right generation; remainingUOP is the
// bin's authoritative count from the same envelope.
func (db *DB) SetProcessNodeRuntimeForDeliveredBin(processNodeID int64, activeClaimID *int64, binID int64, deltaEpoch int64, remainingUOP int) error {
	return processes.SetRuntimeForDeliveredBin(db.DB, processNodeID, activeClaimID, binID, deltaEpoch, remainingUOP)
}

// SetProcessNodeRuntimeLinesidePayload records the identity of the carrier
// standing at this node, together with whether that identity could be
// established at all, who asserted it, and when.
//
// known is not derivable from payloadCode. A carrier known to be carrying
// nothing and a carrier nobody could read both spell themselves "", and the
// readers of this row fail open on the first and must not act on the second.
// Callers go through Engine.recordLinesideCarrier, which takes a
// domain.LinesideCarrier and cannot lose the distinction on the way here.
func (db *DB) SetProcessNodeRuntimeLinesidePayload(processNodeID int64, payloadCode string, known bool, source string) error {
	return processes.SetRuntimeLinesidePayload(db.DB, processNodeID, payloadCode, known, source)
}

// UpdateProcessNodeRuntimeOrders writes BOTH order pointers on a runtime row.
// Only for callers that genuinely decide both slots at once — see
// processes.UpdateRuntimeOrders. To set one pointer, use the partial setters
// below; passing nil here for the slot you are not changing destroys it.
func (db *DB) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return processes.UpdateRuntimeOrders(db.DB, processNodeID, activeOrderID, stagedOrderID)
}

// SetProcessNodeRuntimeActiveOrder writes the active order pointer, leaving
// staged alone.
func (db *DB) SetProcessNodeRuntimeActiveOrder(processNodeID int64, activeOrderID *int64) error {
	return processes.SetRuntimeActiveOrder(db.DB, processNodeID, activeOrderID)
}

// SetProcessNodeRuntimeStagedOrder writes the staged order pointer, leaving
// active alone.
func (db *DB) SetProcessNodeRuntimeStagedOrder(processNodeID int64, stagedOrderID *int64) error {
	return processes.SetRuntimeStagedOrder(db.DB, processNodeID, stagedOrderID)
}

// ClearProcessNodeRuntimeOrders drops both order pointers on one node.
func (db *DB) ClearProcessNodeRuntimeOrders(processNodeID int64) error {
	return processes.ClearRuntimeOrders(db.DB, processNodeID)
}

// ClearProcessNodeRuntimeOrderRefs nulls every runtime order pointer that
// references orderID, on whichever node rows hold it.
func (db *DB) ClearProcessNodeRuntimeOrderRefs(orderID int64) error {
	return processes.ClearRuntimeOrderRefs(db.DB, orderID)
}

// UpdateProcessNodeUOP writes the remaining UOP on a runtime row.
func (db *DB) UpdateProcessNodeUOP(processNodeID int64, remainingUOP int) error {
	return processes.UpdateRuntimeUOP(db.DB, processNodeID, remainingUOP)
}

// AddPendingUOPDelta accumulates a tick count held while no bin is bound
// at the slot (hold-and-replay gap handling).
func (db *DB) AddPendingUOPDelta(processNodeID int64, delta int) error {
	return processes.AddPendingUOPDelta(db.DB, processNodeID, delta)
}

// SetProcessNodeUOPClearPending writes the cached UOP and zeroes the
// pending hold-pile in one statement (used when a tick binds the held
// delta onto a now-present bin).
func (db *DB) SetProcessNodeUOPClearPending(processNodeID int64, remainingUOP int) error {
	return processes.SetRuntimeUOPClearPending(db.DB, processNodeID, remainingUOP)
}

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func (db *DB) SetActivePull(processNodeID int64, active bool) error {
	return processes.SetActivePull(db.DB, processNodeID, active)
}
