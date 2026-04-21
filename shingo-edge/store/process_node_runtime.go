package store

// Phase 5b delegate file: process_node_runtime CRUD now lives in
// store/processes/. This file preserves the *store.DB method surface
// so external callers do not need to change.

import "shingoedge/store/processes"

// ProcessNodeRuntimeState is one row of process_node_runtime_states.
type ProcessNodeRuntimeState = processes.RuntimeState

// EnsureProcessNodeRuntime returns the runtime row for a process_node,
// inserting a fresh row when none exists yet.
func (db *DB) EnsureProcessNodeRuntime(processNodeID int64) (*ProcessNodeRuntimeState, error) {
	return processes.EnsureRuntime(db.DB, processNodeID)
}

// GetProcessNodeRuntime returns the runtime row for a process_node.
func (db *DB) GetProcessNodeRuntime(processNodeID int64) (*ProcessNodeRuntimeState, error) {
	return processes.GetRuntime(db.DB, processNodeID)
}

// SetProcessNodeRuntime updates the active claim and remaining UOP on
// a runtime row.
func (db *DB) SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error {
	return processes.SetRuntime(db.DB, processNodeID, activeClaimID, remainingUOP)
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

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func (db *DB) SetActivePull(processNodeID int64, active bool) error {
	return processes.SetActivePull(db.DB, processNodeID, active)
}
