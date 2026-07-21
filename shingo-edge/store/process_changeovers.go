package store

// Phase 5b delegate file: process_changeover CRUD lives in
// store/processes/. (Phase 6.0c folded the changeovers/ sub-package
// into processes/ — changeovers transition processes between styles.)
// This file preserves the *store.DB method surface so external callers
// do not need to change.
//
// CreateChangeover stays at the top-level store package because it
// runs as a single transaction that also updates the processes table
// (set target_style_id, production_state) and inserts into
// process_nodes / process_node_runtime_states; that orchestration
// would otherwise have to thread *sql.Tx through several files.

import (
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// CreateChangeover body lives in
// shingoedge/service/changeover_service.go::ChangeoverService.Create
// (Phase 6.4a). The CRUD methods below remain as outer-shim delegates
// over the processes/ sub-package functions.

// ListProcessChangeovers returns every process_changeover for a
// process, newest first.
func (db *DB) ListProcessChangeovers(processID int64) ([]processes.Changeover, error) {
	return processes.ListChangeovers(db.DB, processID)
}

// GetActiveProcessChangeover returns the active (non-completed,
// non-cancelled) changeover for a process, if any.
func (db *DB) GetActiveProcessChangeover(processID int64) (*processes.Changeover, error) {
	return processes.GetActiveChangeover(db.DB, processID)
}

// UpdateProcessChangeoverState changes the state on a
// process_changeover.
func (db *DB) UpdateProcessChangeoverState(id int64, state domain.ChangeoverState) error {
	return processes.UpdateChangeoverState(db.DB, id, state)
}

// UpdateProcessChangeoverStateWithTrigger changes the state and records
// the trigger source ("operator-hmi" | "plc-auto" | "auto-task-terminal")
// on a process_changeover. Empty triggeredBy preserves the existing
// audit value, so multi-step finalize paths don't blank it out.
func (db *DB) UpdateProcessChangeoverStateWithTrigger(id int64, state domain.ChangeoverState, triggeredBy string) error {
	return processes.UpdateChangeoverStateWithTrigger(db.DB, id, state, triggeredBy)
}

// ListChangeoverStationTasks returns every changeover_station_task
// for one changeover.
func (db *DB) ListChangeoverStationTasks(changeoverID int64) ([]processes.StationTask, error) {
	return processes.ListChangeoverStationTasks(db.DB, changeoverID)
}

// UpdateChangeoverStationTaskState writes the state on a station task.
func (db *DB) UpdateChangeoverStationTaskState(id int64, state domain.StationTaskState) error {
	return processes.UpdateChangeoverStationTaskState(db.DB, id, state)
}

// GetChangeoverStationTaskByStation returns the station task for one
// (changeover, station) pair.
func (db *DB) GetChangeoverStationTaskByStation(changeoverID, stationID int64) (*processes.StationTask, error) {
	return processes.GetChangeoverStationTaskByStation(db.DB, changeoverID, stationID)
}

// ListChangeoverNodeTasks returns every changeover_node_task for a
// changeover.
func (db *DB) ListChangeoverNodeTasks(changeoverID int64) ([]processes.NodeTask, error) {
	return processes.ListChangeoverNodeTasks(db.DB, changeoverID)
}

// ListChangeoverNodeTasksByStation filters node tasks to those whose
// process node belongs to the given operator_station.
func (db *DB) ListChangeoverNodeTasksByStation(changeoverID, stationID int64) ([]processes.NodeTask, error) {
	return processes.ListChangeoverNodeTasksByStation(db.DB, changeoverID, stationID)
}

// GetChangeoverNodeTaskByNode returns the node task for one
// (changeover, node) pair.
func (db *DB) GetChangeoverNodeTaskByNode(changeoverID, processNodeID int64) (*processes.NodeTask, error) {
	return processes.GetChangeoverNodeTaskByNode(db.DB, changeoverID, processNodeID)
}

// FindChangeoverNodeTaskByOrderID returns the node task that references
// orderID and its parent changeover's state. Direct order-ID match
// without changeover-state filtering — see processes.FindChangeoverNodeTaskByOrderID.
func (db *DB) FindChangeoverNodeTaskByOrderID(orderID int64) (*processes.NodeTask, domain.ChangeoverState, error) {
	return processes.FindChangeoverNodeTaskByOrderID(db.DB, orderID)
}

// GetChangeoverNodeTaskByEvacOrderID returns the node task that
// references orderID via OldMaterialReleaseOrderID (evac leg only).
// See processes.GetChangeoverNodeTaskByEvacOrderID.
func (db *DB) GetChangeoverNodeTaskByEvacOrderID(orderID int64) (*processes.NodeTask, error) {
	return processes.GetChangeoverNodeTaskByEvacOrderID(db.DB, orderID)
}

// UpdateChangeoverNodeTaskState writes the state on a node task.
func (db *DB) UpdateChangeoverNodeTaskState(id int64, state domain.NodeTaskState) error {
	return processes.UpdateChangeoverNodeTaskState(db.DB, id, state)
}

// SetChangeoverNodeTaskSkipNote writes the operator-facing skip message
// on a node task. Pass "" to clear.
func (db *DB) SetChangeoverNodeTaskSkipNote(id int64, note string) error {
	return processes.SetChangeoverNodeTaskSkipNote(db.DB, id, note)
}

// LinkChangeoverNodeOrders associates the next/old material order ids
// with a node task.
func (db *DB) LinkChangeoverNodeOrders(id int64, nextOrderID, oldOrderID *int64) error {
	return processes.LinkChangeoverNodeOrders(db.DB, id, nextOrderID, oldOrderID)
}

// ListChangeoverParticipants returns the participant set for a changeover
// (with the legacy derive-from-tasks fallback).
func (db *DB) ListChangeoverParticipants(changeoverID int64) ([]domain.Participant, error) {
	return processes.ListChangeoverParticipants(db.DB, changeoverID)
}

// IsChangeoverParticipant is the hot-path point query behind intake gating.
func (db *DB) IsChangeoverParticipant(processID int64, coreNodeName string) (bool, string, error) {
	return processes.IsChangeoverParticipant(db.DB, processID, coreNodeName)
}

// ListParticipantsWithStation resolves each participant's release station.
func (db *DB) ListParticipantsWithStation(changeoverID int64) ([]processes.ParticipantWithStation, error) {
	return processes.ListParticipantsWithStation(db.DB, changeoverID)
}
