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

// SetProcessNodeRuntimeWithBin updates active_claim_id, active_bin_id,
// and remaining_uop_cached atomically. Used by completion handlers so
// the bin pointer turns over at the same instant as the runtime reset.
func (db *DB) SetProcessNodeRuntimeWithBin(processNodeID int64, activeClaimID, activeBinID *int64, remainingUOP int) error {
	return processes.SetRuntimeWithBin(db.DB, processNodeID, activeClaimID, activeBinID, remainingUOP)
}

// SetProcessNodeActiveBinID writes only the active bin pointer on a
// runtime row. Used by the bin-pickup handler to clear ownership
// without disturbing the claim or count.
func (db *DB) SetProcessNodeActiveBinID(processNodeID int64, activeBinID *int64) error {
	return processes.SetActiveBinID(db.DB, processNodeID, activeBinID)
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

// UpdateProcessNodeRuntimeOrders writes the active and staged order
// pointers on a runtime row.
func (db *DB) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return processes.UpdateRuntimeOrders(db.DB, processNodeID, activeOrderID, stagedOrderID)
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
