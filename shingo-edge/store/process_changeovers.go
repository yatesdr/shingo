package store

// Phase 5b delegate file: process_changeover CRUD now lives in
// store/changeovers/. This file preserves the *store.DB method surface
// so external callers do not need to change.
//
// CreateChangeover stays at the top-level store package because it
// runs as a single transaction that also updates processes (set
// target_style_id, production_state) and inserts into process_nodes /
// process_node_runtime_states; that orchestration crosses aggregates
// and would otherwise have to thread *sql.Tx through several
// sub-packages.

import (
	"fmt"

	"shingoedge/store/changeovers"
)

// ChangeoverNodeTaskInput holds pre-computed data for a single node
// task to be created as part of a changeover transaction.
type ChangeoverNodeTaskInput = changeovers.NodeTaskInput

// ProcessChangeover is one row of process_changeovers.
type ProcessChangeover = changeovers.Changeover

// ChangeoverStationTask is one row of changeover_station_tasks.
type ChangeoverStationTask = changeovers.StationTask

// ChangeoverNodeTask is one row of changeover_node_tasks.
type ChangeoverNodeTask = changeovers.NodeTask

// CreateChangeover atomically creates a changeover with its station
// and node tasks. Cross-aggregate: it also flips the owning process
// into the changeover state and backfills process_nodes /
// process_node_runtime_states for any core nodes that didn't have a
// row yet. Returns the new changeover id.
func (db *DB) CreateChangeover(processID int64, fromStyleID *int64, toStyleID int64, calledBy, notes string,
	stationIDs []int64, nodeTasks []ChangeoverNodeTaskInput, existingNodes []ProcessNode) (int64, error) {

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by, notes)
		VALUES (?, ?, ?, 'active', ?, ?)`, processID, fromStyleID, toStyleID, calledBy, notes)
	if err != nil {
		return 0, err
	}
	changeoverID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, toStyleID, processID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE processes SET production_state='changeover_active' WHERE id=?`, processID); err != nil {
		return 0, err
	}

	for _, sid := range stationIDs {
		if _, err := tx.Exec(`INSERT INTO changeover_station_tasks (
			process_changeover_id, operator_station_id, state
		) VALUES (?, ?, 'waiting')`, changeoverID, sid); err != nil {
			return 0, err
		}
	}

	for _, nt := range nodeTasks {
		// Find existing process node by core_node_name.
		var processNodeID *int64
		for i := range existingNodes {
			if existingNodes[i].CoreNodeName == nt.CoreNodeName {
				id := existingNodes[i].ID
				processNodeID = &id
				break
			}
		}
		if processNodeID == nil {
			// Auto-create process node for this claimed core node.
			res, err := tx.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name) VALUES (?, ?, ?, ?)`,
				nt.ProcessID, nt.CoreNodeName, nt.CoreNodeName, nt.CoreNodeName)
			if err != nil {
				return 0, fmt.Errorf("auto-create process node for %s: %w", nt.CoreNodeName, err)
			}
			id, _ := res.LastInsertId()
			processNodeID = &id
		}

		if _, err := tx.Exec(`INSERT INTO changeover_node_tasks (
			process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state
		) VALUES (?, ?, ?, ?, ?, ?)`, changeoverID, *processNodeID, nt.FromClaimID, nt.ToClaimID, nt.Situation, nt.State); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO process_node_runtime_states (process_node_id) VALUES (?)`, *processNodeID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changeoverID, nil
}

// ListProcessChangeovers returns every process_changeover for a
// process, newest first.
func (db *DB) ListProcessChangeovers(processID int64) ([]ProcessChangeover, error) {
	return changeovers.List(db.DB, processID)
}

// GetActiveProcessChangeover returns the active (non-completed,
// non-cancelled) changeover for a process, if any.
func (db *DB) GetActiveProcessChangeover(processID int64) (*ProcessChangeover, error) {
	return changeovers.GetActive(db.DB, processID)
}

// UpdateProcessChangeoverState changes the state on a
// process_changeover.
func (db *DB) UpdateProcessChangeoverState(id int64, state string) error {
	return changeovers.UpdateState(db.DB, id, state)
}

// ListChangeoverStationTasks returns every changeover_station_task
// for one changeover.
func (db *DB) ListChangeoverStationTasks(changeoverID int64) ([]ChangeoverStationTask, error) {
	return changeovers.ListStationTasks(db.DB, changeoverID)
}

// UpdateChangeoverStationTaskState writes the state on a station task.
func (db *DB) UpdateChangeoverStationTaskState(id int64, state string) error {
	return changeovers.UpdateStationTaskState(db.DB, id, state)
}

// GetChangeoverStationTaskByStation returns the station task for one
// (changeover, station) pair.
func (db *DB) GetChangeoverStationTaskByStation(changeoverID, stationID int64) (*ChangeoverStationTask, error) {
	return changeovers.GetStationTaskByStation(db.DB, changeoverID, stationID)
}

// ListChangeoverNodeTasks returns every changeover_node_task for a
// changeover.
func (db *DB) ListChangeoverNodeTasks(changeoverID int64) ([]ChangeoverNodeTask, error) {
	return changeovers.ListNodeTasks(db.DB, changeoverID)
}

// ListChangeoverNodeTasksByStation filters node tasks to those whose
// process node belongs to the given operator_station.
func (db *DB) ListChangeoverNodeTasksByStation(changeoverID, stationID int64) ([]ChangeoverNodeTask, error) {
	return changeovers.ListNodeTasksByStation(db.DB, changeoverID, stationID)
}

// GetChangeoverNodeTaskByNode returns the node task for one
// (changeover, node) pair.
func (db *DB) GetChangeoverNodeTaskByNode(changeoverID, processNodeID int64) (*ChangeoverNodeTask, error) {
	return changeovers.GetNodeTaskByNode(db.DB, changeoverID, processNodeID)
}

// UpdateChangeoverNodeTaskState writes the state on a node task.
func (db *DB) UpdateChangeoverNodeTaskState(id int64, state string) error {
	return changeovers.UpdateNodeTaskState(db.DB, id, state)
}

// LinkChangeoverNodeOrders associates the next/old material order ids
// with a node task.
func (db *DB) LinkChangeoverNodeOrders(id int64, nextOrderID, oldOrderID *int64) error {
	return changeovers.LinkNodeOrders(db.DB, id, nextOrderID, oldOrderID)
}
