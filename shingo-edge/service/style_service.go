package service

import (
	"strings"

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
//
// calledBy is stamped on every copied claim (source='cloned').
func (s *StyleService) Clone(srcID int64, name, description, calledBy string) (int64, error) {
	return s.db.CloneStyle(srcID, name, description, calledBy)
}

// GenerateVariants scaffolds a family of styles from one base style, each a
// clone of the base with its per-claim payload overrides applied, in a single
// atomic batch. Returns the new style ids in variant order.
//
// calledBy is stamped on every generated claim (source='generated').
func (s *StyleService) GenerateVariants(baseID int64, variants []domain.StyleVariant, calledBy string) ([]int64, error) {
	return s.db.GenerateStyles(baseID, variants, calledBy)
}

// CopyClaimsResult is one target style's outcome in a CopyClaims batch.
// Status is "copied", "skipped", or "failed" — the batch never aborts on
// the first bad target, so a 40-style copy reports every outcome.
//
// Notes carries the override layer's report for a copied target: renames it
// refused (collision or press-index distinctness), role changes it withheld
// against SwapMode requirements, pair references it auto-fixed after a
// rename. Empty unless the copy carried overrides that had something to say.
type CopyClaimsResult struct {
	StyleID int64    `json:"style_id"`
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// CopyClaims replaces each target style's node claims with the source's —
// the same verbatim copy Clone Style performs, but into EXISTING styles.
// Per-target results, never abort-on-first-error.
//
// Rules enforced here, not just in the UI:
//   - a target must live in the source's process;
//   - a target must not be the process's ACTIVE style — copying claims
//     under a style production is running right now would change live
//     behavior mid-part, so it is refused outright;
//   - the source itself is skipped, and duplicate targets collapse.
//
// includePayloads=false preserves each target's own payloads (per node,
// for nodes the two styles share) — the "copy the choreography, keep my
// payloads" mode.
//
// overrides (matched by the source claim's node name, blank field =
// inherit the copied value) ride the same transaction per target, after
// the copy. They are cleaned up once here so the store layer sees one
// well-formed row per node: blank match keys dropped, duplicates collapsed
// last-wins, all-blank rows dropped as the no-ops the modal never sends.
func (s *StyleService) CopyClaims(srcID int64, targets []int64, includePayloads bool, overrides []processes.ClaimOverride) []CopyClaimsResult {
	results := make([]CopyClaimsResult, 0, len(targets))
	if len(targets) == 0 {
		return results
	}
	// One well-formed row per node survives to the store layer: trimmed,
	// duplicates collapsed last-wins, no-ops (nothing set) dropped. Rows
	// without a match key cannot attach to anything and are refused here —
	// a 400 from the handler is the honest answer, and the handler owns it,
	// so a blank key that still arrives is dropped with the same silence a
	// blank field would get.
	clean := make([]processes.ClaimOverride, 0, len(overrides))
	ovSeen := map[string]bool{}
	for _, ov := range overrides {
		ov.Node = strings.TrimSpace(ov.Node)
		if ov.Node == "" {
			continue
		}
		ov.CoreNodeName = strings.TrimSpace(ov.CoreNodeName)
		ov.Role = strings.TrimSpace(ov.Role)
		ov.PayloadCode = strings.TrimSpace(ov.PayloadCode)
		ov.InboundSource = strings.TrimSpace(ov.InboundSource)
		ov.OutboundDestination = strings.TrimSpace(ov.OutboundDestination)
		ov.InboundStaging = strings.TrimSpace(ov.InboundStaging)
		ov.OutboundStaging = strings.TrimSpace(ov.OutboundStaging)
		ov.PairedCoreNode = strings.TrimSpace(ov.PairedCoreNode)
		ov.SecondPairedCoreNode = strings.TrimSpace(ov.SecondPairedCoreNode)
		if ov.CoreNodeName == "" && ov.Role == "" && ov.PayloadCode == "" &&
			ov.InboundSource == "" && ov.OutboundDestination == "" &&
			ov.InboundStaging == "" && ov.OutboundStaging == "" &&
			ov.PairedCoreNode == "" && ov.SecondPairedCoreNode == "" {
			continue
		}
		key := ov.Node
		if ovSeen[key] {
			for i := range clean {
				if clean[i].Node == key {
					clean[i] = ov
					break
				}
			}
			continue
		}
		ovSeen[key] = true
		clean = append(clean, ov)
	}
	overrides = clean

	src, err := s.db.GetStyle(srcID)
	if err != nil || src == nil {
		return []CopyClaimsResult{{Status: "failed", Reason: "source style not found"}}
	}
	var activeStyleID int64
	if proc, err := s.db.GetProcess(src.ProcessID); err == nil && proc != nil && proc.ActiveStyleID != nil {
		activeStyleID = *proc.ActiveStyleID
	}

	seen := map[int64]bool{}
	for _, targetID := range targets {
		if seen[targetID] {
			continue
		}
		seen[targetID] = true
		res := CopyClaimsResult{StyleID: targetID}
		switch {
		case targetID == srcID:
			res.Status, res.Reason = "skipped", "is the source style"
		default:
			tgt, err := s.db.GetStyle(targetID)
			switch {
			case err != nil || tgt == nil:
				res.Status, res.Reason = "failed", "style not found"
			case tgt.ProcessID != src.ProcessID:
				res.Status, res.Reason = "failed", "style belongs to a different process"
			case tgt.ID == activeStyleID:
				res.Status, res.Reason = "failed", "active style cannot be a copy target"
			default:
				notes, err := s.db.CopyStyleClaims(srcID, targetID, includePayloads, overrides)
				if err != nil {
					res.Status, res.Reason = "failed", err.Error()
				} else {
					res.Status = "copied"
					res.Notes = notes
				}
			}
		}
		results = append(results, res)
	}
	return results
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

// ListContainmentClaims returns every live claim that declares a containment
// destination. The containment screen groups its nodes from this: each
// distinct containment destination is a section, each claim its outbound
// release target.
func (s *StyleService) ListContainmentClaims() ([]processes.NodeClaim, error) {
	return s.db.ListAllContainmentClaims()
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
