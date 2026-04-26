package store

// Phase 5b delegate file: process_node CRUD now lives in
// store/processes/. This file preserves the *store.DB method surface
// so external callers do not need to change.

import "shingoedge/store/processes"

// ListProcessNodes returns every process_nodes row.
func (db *DB) ListProcessNodes() ([]processes.Node, error) {
	return processes.ListNodes(db.DB)
}

// ListProcessNodesByProcess returns process_nodes rows for one process.
func (db *DB) ListProcessNodesByProcess(processID int64) ([]processes.Node, error) {
	return processes.ListNodesByProcess(db.DB, processID)
}

// ListProcessNodesByStation returns process_nodes rows for one
// operator_station.
func (db *DB) ListProcessNodesByStation(stationID int64) ([]processes.Node, error) {
	return processes.ListNodesByStation(db.DB, stationID)
}

// GetProcessNode returns one process_node row by id.
func (db *DB) GetProcessNode(id int64) (*processes.Node, error) {
	return processes.GetNode(db.DB, id)
}

// CreateProcessNode inserts a process_node row, generating the code
// and sequence number when not supplied.
func (db *DB) CreateProcessNode(in processes.NodeInput) (int64, error) {
	return processes.CreateNode(db.DB, in)
}

// UpdateProcessNode modifies an existing process_node row.
func (db *DB) UpdateProcessNode(id int64, in processes.NodeInput) error {
	return processes.UpdateNode(db.DB, id, in)
}

// DeleteProcessNode removes a process_node row.
func (db *DB) DeleteProcessNode(id int64) error {
	return processes.DeleteNode(db.DB, id)
}
