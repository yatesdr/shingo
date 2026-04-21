package store

// Phase 5b delegate file: process_node CRUD now lives in
// store/processes/. This file preserves the *store.DB method surface
// so external callers do not need to change.

import "shingoedge/store/processes"

// ProcessNode is one row of process_nodes.
type ProcessNode = processes.Node

// ProcessNodeInput is the input shape for CreateProcessNode /
// UpdateProcessNode.
type ProcessNodeInput = processes.NodeInput

// ListProcessNodes returns every process_nodes row.
func (db *DB) ListProcessNodes() ([]ProcessNode, error) {
	return processes.ListNodes(db.DB)
}

// ListProcessNodesByProcess returns process_nodes rows for one process.
func (db *DB) ListProcessNodesByProcess(processID int64) ([]ProcessNode, error) {
	return processes.ListNodesByProcess(db.DB, processID)
}

// ListProcessNodesByStation returns process_nodes rows for one
// operator_station.
func (db *DB) ListProcessNodesByStation(stationID int64) ([]ProcessNode, error) {
	return processes.ListNodesByStation(db.DB, stationID)
}

// GetProcessNode returns one process_node row by id.
func (db *DB) GetProcessNode(id int64) (*ProcessNode, error) {
	return processes.GetNode(db.DB, id)
}

// CreateProcessNode inserts a process_node row, generating the code
// and sequence number when not supplied.
func (db *DB) CreateProcessNode(in ProcessNodeInput) (int64, error) {
	return processes.CreateNode(db.DB, in)
}

// UpdateProcessNode modifies an existing process_node row.
func (db *DB) UpdateProcessNode(id int64, in ProcessNodeInput) error {
	return processes.UpdateNode(db.DB, id, in)
}

// DeleteProcessNode removes a process_node row.
func (db *DB) DeleteProcessNode(id int64) error {
	return processes.DeleteNode(db.DB, id)
}
