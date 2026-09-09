package service

import (
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// StyleService owns the style aggregate's CRUD — styles themselves
// (recipes a process can run) and style_node_claims (which core
// nodes a style needs material from, plus the per-claim swap mode
// configuration). These two concepts are tightly coupled: a style
// declares its claims, claims belong to exactly one style.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
type StyleService struct {
	db *store.DB
}

// NewStyleService constructs a StyleService wrapping the shared
// *store.DB.
func NewStyleService(db *store.DB) *StyleService {
	return &StyleService{db: db}
}

// ── Styles ────────────────────────────────────────────────────────

// List returns all styles ordered by name.
func (s *StyleService) List() ([]processes.Style, error) {
	return s.db.ListStyles()
}

// ListByProcess returns styles for a single process_id.
func (s *StyleService) ListByProcess(processID int64) ([]processes.Style, error) {
	return s.db.ListStylesByProcess(processID)
}

// Get returns one style by id.
func (s *StyleService) Get(id int64) (*processes.Style, error) {
	return s.db.GetStyle(id)
}

// Create inserts a new style and returns the new row id.
func (s *StyleService) Create(name, description string, processID int64) (int64, error) {
	return s.db.CreateStyle(name, description, processID)
}

// Update modifies an existing style.
func (s *StyleService) Update(id int64, name, description string, processID int64) error {
	return s.db.UpdateStyle(id, name, description, processID)
}

// SetExpectedCATID sets (or clears, when empty) the style's expected PLC
// part-identity value (WarLink CATID_01). Written by the style editor
// alongside a create/update; drives the A5 CATID guard.
func (s *StyleService) SetExpectedCATID(id int64, expectedCATID string) error {
	return s.db.SetStyleExpectedCATID(id, expectedCATID)
}

// Delete RETIRES a style by id. Nothing is destroyed; see store DeleteStyle.
func (s *StyleService) Delete(id int64) error {
	return s.db.DeleteStyle(id)
}

// Restore un-retires a style by id.
func (s *StyleService) Restore(id int64) error {
	return s.db.RestoreStyle(id)
}

// DeleteImpact reports what the style is carrying, for the confirmation the
// operator sees before retiring it.
func (s *StyleService) DeleteImpact(id int64) (*processes.StyleImpact, error) {
	return s.db.StyleDeleteImpact(id)
}

// Clone duplicates an existing style (same process) along with every
// style_node_claim row. The new style starts inactive; the caller sets it
// active separately. Operators use this to scaffold a per-payload variant of
// a style that shares robot choreography.
func (s *StyleService) Clone(srcID int64, name, description string) (int64, error) {
	return s.db.CloneStyle(srcID, name, description)
}

// GenerateVariants scaffolds a family of styles from one base style, each a
// clone of the base with its per-claim payload overrides applied, in a single
// atomic batch. Returns the new style ids in variant order.
func (s *StyleService) GenerateVariants(baseID int64, variants []domain.StyleVariant) ([]int64, error) {
	return s.db.GenerateStyles(baseID, variants)
}

// ── Style/node claims ─────────────────────────────────────────────

// ListClaims returns every claim for a style, exactly as stored. It enriches
// nothing — the body below returns the store's rows unmodified.
//
// It used to fold the loader-wide transitional flag (the Edge-only
// transitional_loaders set, keyed by core_node_name) onto produce manual_swap
// claims so the claim editor could reflect and toggle it. Replenishment type
// and dedicated-position layout both moved to the Core loader aggregate, the
// editor stopped surfacing either, and the enrichment went with them.
func (s *StyleService) ListClaims(styleID int64) ([]processes.NodeClaim, error) {
	claims, err := s.db.ListStyleNodeClaims(styleID)
	if err != nil {
		return nil, err
	}
	// Loader replenishment (operator-driven) + dedicated-position layout now live on
	// the Core aggregate, not these per-style edge flag tables, and the claim editor
	// no longer surfaces them — so nothing is populated onto the claims here.
	return claims, nil
}

// GetClaim returns one claim by id.
func (s *StyleService) GetClaim(id int64) (*processes.NodeClaim, error) {
	return s.db.GetStyleNodeClaim(id)
}

// UpsertClaim inserts or updates a claim and returns the row id.
// Validates manual_swap invariants (auto_confirm and outbound
// destination) inside the underlying sub-package.
func (s *StyleService) UpsertClaim(in processes.NodeClaimInput) (int64, error) {
	return s.db.UpsertStyleNodeClaim(in)
}

// DeleteClaim removes a claim row by id.
func (s *StyleService) DeleteClaim(id int64) error {
	return s.db.DeleteStyleNodeClaim(id)
}
