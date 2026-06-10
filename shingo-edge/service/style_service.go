package service

import (
	"shingo/protocol"
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

// Delete removes a style row by id.
func (s *StyleService) Delete(id int64) error {
	return s.db.DeleteStyle(id)
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

// ListClaims returns every claim for a style. Produce manual_swap (bin
// loader) claims are enriched with the loader-wide transitional flag (the
// Edge-only transitional_loaders set, keyed by core_node_name) so the Edge
// processes claim editor can reflect and toggle it; other claims carry the
// zero value.
func (s *StyleService) ListClaims(styleID int64) ([]processes.NodeClaim, error) {
	claims, err := s.db.ListStyleNodeClaims(styleID)
	if err != nil {
		return nil, err
	}
	for i := range claims {
		if claims[i].Role != protocol.ClaimRoleProduce || claims[i].SwapMode != protocol.SwapModeManualSwap {
			continue
		}
		// Fail-open: a lookup error leaves TransitionalLoader false rather
		// than failing the whole claim list (mirrors isTransitionalLoader).
		if on, lerr := s.db.IsTransitionalLoader(claims[i].CoreNodeName); lerr == nil {
			claims[i].TransitionalLoader = on
		}
		if on, lerr := s.db.IsHomeLocationLoader(claims[i].CoreNodeName); lerr == nil {
			claims[i].HomeLocationLoader = on
		}
	}
	return claims, nil
}

// SetTransitionalLoader marks (on) or clears the loader-wide transitional
// flag for a bin loader, keyed by core_node_name. updatedBy is recorded on
// the audit column. The caller is responsible for restricting this to a
// produce manual_swap claim — the set itself is loader-wide and untyped.
func (s *StyleService) SetTransitionalLoader(coreNodeName string, on bool, updatedBy string) error {
	return s.db.SetTransitionalLoader(coreNodeName, on, updatedBy)
}

func (s *StyleService) SetHomeLocationLoader(coreNodeName string, on bool, updatedBy string) error {
	return s.db.SetHomeLocationLoader(coreNodeName, on, updatedBy)
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
