package service

import (
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

// Delete removes a style row by id.
func (s *StyleService) Delete(id int64) error {
	return s.db.DeleteStyle(id)
}

// ── Style/node claims ─────────────────────────────────────────────

// ListClaims returns every claim for a style.
func (s *StyleService) ListClaims(styleID int64) ([]processes.NodeClaim, error) {
	return s.db.ListStyleNodeClaims(styleID)
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
