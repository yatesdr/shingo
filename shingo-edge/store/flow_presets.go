package store

// Delegate file: flow presets live in store/processes/. This preserves the
// *store.DB method surface the services and tests use.

import "shingoedge/store/processes"

// ListFlowPresets returns a process's presets, newest version of each name
// first; archived versions only when includeArchived.
func (db *DB) ListFlowPresets(processID int64, includeArchived bool) ([]processes.FlowPreset, error) {
	return processes.ListFlowPresets(db.DB, processID, includeArchived)
}

// GetFlowPreset returns one preset by id, archived or not.
func (db *DB) GetFlowPreset(id int64) (*processes.FlowPreset, error) {
	return processes.GetFlowPreset(db.DB, id)
}

// CreateFlowPreset validates a preset against the process's positions and
// every member of its routing set, and saves it as the next version of its
// name.
func (db *DB) CreateFlowPreset(in processes.FlowPresetInput) (int64, error) {
	return processes.CreateFlowPreset(db.DB, in)
}

// RenameFlowPreset renames every version of a preset's name, and nothing else.
func (db *DB) RenameFlowPreset(id int64, name string) error {
	return processes.RenameFlowPreset(db.DB, id, name)
}

// ArchiveFlowPreset hides a preset version from the chooser.
func (db *DB) ArchiveFlowPreset(id int64) error {
	return processes.ArchiveFlowPreset(db.DB, id)
}

// StampClaimPresetProvenance records which preset version a style's flow came
// from, on the two provenance columns and nothing else.
func (db *DB) StampClaimPresetProvenance(styleID, presetID int64, version int) (int64, error) {
	return processes.StampClaimPresetProvenance(db.DB, styleID, presetID, version)
}
