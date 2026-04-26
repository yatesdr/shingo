package store

// Phase 5b delegate file: style CRUD lives in store/processes/.
// (Phase 6.0c folded the styles/ sub-package into processes/ — styles
// are part of the process domain cluster.) This file preserves the
// *store.DB method surface so external callers do not need to change.

import "shingoedge/store/processes"

// ListStyles returns all styles ordered by name.
func (db *DB) ListStyles() ([]processes.Style, error) {
	return processes.ListStyles(db.DB)
}

// ListStylesByProcess returns styles for a single process_id.
func (db *DB) ListStylesByProcess(processID int64) ([]processes.Style, error) {
	return processes.ListStylesByProcess(db.DB, processID)
}

// GetStyleByName looks up a single style by name.
func (db *DB) GetStyleByName(name string) (*processes.Style, error) {
	return processes.GetStyleByName(db.DB, name)
}

// GetStyle looks up a single style by id.
func (db *DB) GetStyle(id int64) (*processes.Style, error) {
	return processes.GetStyle(db.DB, id)
}

// CreateStyle inserts a new style and returns the new row id.
func (db *DB) CreateStyle(name, description string, processID int64) (int64, error) {
	return processes.CreateStyle(db.DB, name, description, processID)
}

// UpdateStyle modifies an existing style.
func (db *DB) UpdateStyle(id int64, name, description string, processID int64) error {
	return processes.UpdateStyle(db.DB, id, name, description, processID)
}

// DeleteStyle removes a style row by id.
func (db *DB) DeleteStyle(id int64) error {
	return processes.DeleteStyle(db.DB, id)
}
