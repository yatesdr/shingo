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
func (db *DB) UpdateProcessChangeoverState(id int64, state string) error {
	return processes.UpdateChangeoverState(db.DB, id, state)
}

// ListChangeoverStationTasks returns every changeover_station_task
// for one changeover.
func (db *DB) ListChangeoverStationTasks(changeoverID int64) ([]processes.StationTask, error) {
	return processes.ListChangeoverStationTasks(db.DB, changeoverID)
}

// UpdateChangeoverStationTaskState writes the state on a station task.
func (db *DB) UpdateChangeoverStationTaskState(id int64, state string) error {
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

// UpdateChangeoverNodeTaskState writes the state on a node task.
func (db *DB) UpdateChangeoverNodeTaskState(id int64, state string) error {
	return processes.UpdateChangeoverNodeTaskState(db.DB, id, state)
}

// LinkChangeoverNodeOrders associates the next/old material order ids
// with a node task.
func (db *DB) LinkChangeoverNodeOrders(id int64, nextOrderID, oldOrderID *int64) error {
	return processes.LinkChangeoverNodeOrders(db.DB, id, nextOrderID, oldOrderID)
}
