// replenishment_admin.go — engine wrapper for the CELL-side half of the
// replenishment admin page (handlers_admin_replenishment.go +
// handlers_api_replenishment.go).
//
// The engine-level wrapper exists so the www layer's narrow ServiceAccess /
// EngineOrchestration interface doesn't leak the wide *store.DB surface.
//
// SCOPE NOTE — this file used to carry a second, LOADER-side half: per-(loader,
// payload) UOP thresholds, a duplicate of Core's threshold calculator, and the
// apply/override/recalculate paths behind them. All of it was inert. Core owns
// the loader UOP threshold (bin_loader_homes.uop_threshold →
// BuildDemandRegistryFromAggregate → demand_registry → the threshold monitor),
// and the Edge write path terminated in SendClaimSync(), a no-op stub retired
// when Core took ownership of the loader aggregate. A threshold typed on the
// Edge page saved cleanly, displayed, and reached nothing. It was deleted
// rather than left as a trap.
//
// What remains is genuinely Edge-owned: reorder_point / reorder_point_source /
// auto_reorder on style_node_claims, which handleConsumeTick reads on every PLC
// tick to fire auto-reorder. That is the cell-side threshold and it is live.
package engine

import (
	"fmt"

	"shingoedge/domain"
)

// CellReorderInput is the write shape for the cell-side reorder_point
// + reorder_point_source pair.
type CellReorderInput struct {
	ClaimID      int64
	ReorderPoint int
	Source       string
	AutoReorder  bool
	// CalledBy is the desktop session's user. THE HANDLER'S, never the body's,
	// exactly as flow/save decides it (owner ruling R1): this is the third
	// door that writes a claim, and it used to stamp none — which
	// MaterializeClaim reads as ClaimSourceAdmin with no caller, so a flow
	// saved from a station read "the desktop" on the set-up card after an
	// engineer nudged a reorder point.
	CalledBy string
}

// UpdateCellReorder modifies the reorder_point + source + AutoReorder
// fields on an existing style_node_claim. Other claim fields stay
// untouched — the engineer's edit on the replenishment page should
// not change InboundSource / OutboundDestination / etc.
func (e *Engine) UpdateCellReorder(in CellReorderInput) error {
	if in.ClaimID <= 0 {
		return fmt.Errorf("cell reorder: claim_id required")
	}
	if in.ReorderPoint < 0 {
		return fmt.Errorf("cell reorder: reorder_point must be >= 0")
	}
	current, err := e.db.GetStyleNodeClaim(in.ClaimID)
	if err != nil || current == nil {
		return fmt.Errorf("cell reorder: claim %d not found: %w", in.ClaimID, err)
	}
	source := in.Source
	if source == "" {
		source = "manual"
	}
	// The stored claim as a write, with every absent-means-untouched column
	// left absent: this path edits three columns and has no opinion about the
	// rest. See domain.InputFromClaimUngated for why the echo cannot be a
	// hand-kept list.
	upd := domain.InputFromClaimUngated(*current)
	upd.ReorderPoint = in.ReorderPoint
	upd.ReorderPointSource = &source
	upd.AutoReorder = &in.AutoReorder
	// This IS a claim write from the desktop, so it says so — with the person
	// who made it, like the other two doors.
	upd.Source, upd.CalledBy = domain.ClaimSourceAdmin, in.CalledBy
	if _, err := e.db.UpsertStyleNodeClaim(upd); err != nil {
		return fmt.Errorf("cell reorder: upsert: %w", err)
	}
	return nil
}
