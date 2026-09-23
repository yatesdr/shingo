package store

// Phase 5b delegate file: style CRUD lives in store/processes/.
// (Phase 6.0c folded the styles/ sub-package into processes/ — styles
// are part of the process domain cluster.) This file preserves the
// *store.DB method surface so external callers do not need to change.

import (
	"shingoedge/domain"
	"shingoedge/store/processes"
)

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

// SetStyleExpectedCATID sets (or clears, when empty) the style's expected PLC
// part-identity value (WarLink CATID_01) used by the A5 CATID guard.
func (db *DB) SetStyleExpectedCATID(id int64, expectedCATID string) error {
	return processes.SetStyleExpectedCATID(db.DB, id, expectedCATID)
}

// DeleteStyle RETIRES a style by id (soft delete). See processes.DeleteStyle.
func (db *DB) DeleteStyle(id int64) error {
	return processes.DeleteStyle(db.DB, id)
}

// RestoreStyle un-retires a style by id.
func (db *DB) RestoreStyle(id int64) error {
	return processes.RestoreStyle(db.DB, id)
}

// StyleDeleteImpact counts every row a HARD delete of this style would destroy,
// so the operator confirmation can name the number instead of implying it.
func (db *DB) StyleDeleteImpact(id int64) (*processes.StyleImpact, error) {
	return processes.StyleDeleteImpact(db.DB, id)
}

// CloneStyle creates a new style in src's process, copying all of src's
// style_node_claims verbatim. Returns the new style id.
func (db *DB) CloneStyle(srcID int64, name, description, calledBy string) (int64, error) {
	return processes.CloneStyle(db.DB, srcID, name, description, calledBy)
}

// CopyStyleClaims replaces target's node claims with src's (the clone
// column list, verbatim). includePayloads=false keeps the target's own
// payloads on the nodes the two styles share; overrides is the optional
// per-claim adjust layer applied on top of the copy (see
// processes.ClaimOverride). Returns the notes the override layer produced.
// Callers own the active-style and same-process rules.
func (db *DB) CopyStyleClaims(srcID, targetID int64, includePayloads bool, overrides []processes.ClaimOverride) ([]string, error) {
	return processes.CopyStyleClaims(db.DB, srcID, targetID, includePayloads, overrides)
}

// GenerateStyles scaffolds a family of styles from one base style, each a
// clone of base with per-claim payload overrides applied, in one transaction.
func (db *DB) GenerateStyles(baseID int64, variants []domain.StyleVariant, calledBy string) ([]int64, error) {
	return processes.GenerateStyles(db.DB, baseID, variants, calledBy)
}

// ListClaimsByContainmentDest returns every live claim whose containment
// destination is dest (the containment release path's claim lookup).
func (db *DB) ListClaimsByContainmentDest(dest string) ([]processes.NodeClaim, error) {
	return processes.ListClaimsByContainmentDest(db.DB, dest)
}

// ListContainmentClaimsForPayload returns every live claim bound to a payload
// that declares a containment destination (the recall path's work list).
func (db *DB) ListContainmentClaimsForPayload(payloadCode string) ([]processes.NodeClaim, error) {
	return processes.ListContainmentClaimsForPayload(db.DB, payloadCode)
}

// ListAllContainmentClaims returns every live claim that declares a
// containment destination (the containment screen's node grouping).
func (db *DB) ListAllContainmentClaims() ([]processes.NodeClaim, error) {
	return processes.ListAllContainmentClaims(db.DB)
}

// ListProduceContainmentDestinations returns, per process id, the containment
// destination its live styles' produce claims declare (the settings toggle's
// derived read).
func (db *DB) ListProduceContainmentDestinations() (map[int64]string, error) {
	return processes.ListProduceContainmentDestinations(db.DB)
}
