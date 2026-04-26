package store

// Phase 5b delegate file: process CRUD now lives in store/processes/.
// This file preserves the *store.DB method surface so external callers
// do not need to change.

import "shingoedge/store/processes"

// ListProcesses returns every process row sorted by name.
func (db *DB) ListProcesses() ([]processes.Process, error) {
	return processes.List(db.DB)
}

// GetProcess returns one process by id.
func (db *DB) GetProcess(id int64) (*processes.Process, error) {
	return processes.Get(db.DB, id)
}

// CreateProcess inserts a process and returns the new row id.
func (db *DB) CreateProcess(name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	return processes.Create(db.DB, name, description, productionState, counterPLC, counterTag, counterEnabled)
}

// UpdateProcess modifies a process row.
func (db *DB) UpdateProcess(id int64, name, description, productionState string, counterPLC, counterTag string, counterEnabled bool) error {
	return processes.Update(db.DB, id, name, description, productionState, counterPLC, counterTag, counterEnabled)
}

// DeleteProcess removes a process row.
func (db *DB) DeleteProcess(id int64) error {
	return processes.Delete(db.DB, id)
}

// SetActiveStyle changes the active_style_id on a process.
func (db *DB) SetActiveStyle(processID int64, styleID *int64) error {
	return processes.SetActiveStyle(db.DB, processID, styleID)
}

// SetTargetStyle changes the target_style_id on a process.
func (db *DB) SetTargetStyle(processID int64, styleID *int64) error {
	return processes.SetTargetStyle(db.DB, processID, styleID)
}

// GetActiveStyleID returns just the active_style_id pointer for a
// process.
func (db *DB) GetActiveStyleID(processID int64) (*int64, error) {
	return processes.GetActiveStyleID(db.DB, processID)
}

// SetProcessProductionState writes the production_state on a process.
func (db *DB) SetProcessProductionState(processID int64, state string) error {
	return processes.SetProductionState(db.DB, processID, state)
}
