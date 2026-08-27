package store

// Phase 5b delegate file: process CRUD now lives in store/processes/.
// This file preserves the *store.DB method surface so external callers
// do not need to change.

import (
	"shingoedge/store/processes"
	"shingoedge/store/process_groups"
)

// ProcessGroup is the row type for the process_groups table; aliased
// here so callers can reference it as store.ProcessGroup without
// importing the sub-package.
type ProcessGroup = process_groups.Group

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

// SetChangeoverAutoArm writes the changeover_auto_arm mode (auto|prompt|off) on a
// process; unknown/empty ⇒ auto.
func (db *DB) SetChangeoverAutoArm(processID int64, mode string) error {
	return processes.SetChangeoverAutoArm(db.DB, processID, mode)
}

// SetProcessGroupID assigns a process to a group (pass nil to ungroup).
func (db *DB) SetProcessGroupID(processID int64, groupID *int64) error {
	return processes.SetGroupID(db.DB, processID, groupID)
}

// ── Process groups ──────────────────────────────────────────────────

// ListProcessGroups returns every process_groups row, ordered by name.
func (db *DB) ListProcessGroups() ([]ProcessGroup, error) {
	return process_groups.ListGroups(db.DB)
}

// GetProcessGroup returns one process_group by id.
func (db *DB) GetProcessGroup(id int64) (*ProcessGroup, error) {
	return process_groups.GetGroup(db.DB, id)
}

// CreateProcessGroup inserts a process_group and returns the new id.
func (db *DB) CreateProcessGroup(name, description string) (int64, error) {
	return process_groups.CreateGroup(db.DB, name, description)
}

// UpdateProcessGroup modifies a process_group's name and description.
func (db *DB) UpdateProcessGroup(id int64, name, description string) error {
	return process_groups.UpdateGroup(db.DB, id, name, description)
}

// DeleteProcessGroup removes a process_group. Member processes revert to
// Ungrouped via the explicit transactional UPDATE inside DeleteGroup —
// foreign_keys is OFF, so the ON DELETE SET NULL FK never fires.
func (db *DB) DeleteProcessGroup(id int64) error {
	return process_groups.DeleteGroup(db.DB, id)
}

// CountProcessGroupMembers returns how many processes are in a group.
func (db *DB) CountProcessGroupMembers(id int64) (int, error) {
	return process_groups.CountGroupMembers(db.DB, id)
}
