package store

// Phase 5b delegate file: style CRUD now lives in store/styles/.
// This file preserves the *store.DB method surface so external callers
// do not need to change.

import "shingoedge/store/styles"

// Style represents a product/recipe style that maps to a BOM.
type Style = styles.Style

// ListStyles returns all styles ordered by name.
func (db *DB) ListStyles() ([]Style, error) {
	return styles.List(db.DB)
}

// ListStylesByProcess returns styles for a single process_id.
func (db *DB) ListStylesByProcess(processID int64) ([]Style, error) {
	return styles.ListByProcess(db.DB, processID)
}

// GetStyleByName looks up a single style by name.
func (db *DB) GetStyleByName(name string) (*Style, error) {
	return styles.GetByName(db.DB, name)
}

// GetStyle looks up a single style by id.
func (db *DB) GetStyle(id int64) (*Style, error) {
	return styles.Get(db.DB, id)
}

// CreateStyle inserts a new style and returns the new row id.
func (db *DB) CreateStyle(name, description string, processID int64) (int64, error) {
	return styles.Create(db.DB, name, description, processID)
}

// UpdateStyle modifies an existing style.
func (db *DB) UpdateStyle(id int64, name, description string, processID int64) error {
	return styles.Update(db.DB, id, name, description, processID)
}

// DeleteStyle removes a style row by id.
func (db *DB) DeleteStyle(id int64) error {
	return styles.Delete(db.DB, id)
}
