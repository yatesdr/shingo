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
	"shingoedge/store/processes"
)

// processClaimToInput maps a persisted NodeClaim back to its
// NodeClaimInput shape — needed because the admin replenishment edits
// touch only a subset of claim fields but UpsertStyleNodeClaim writes
// the whole row.
func processClaimToInput(c *processes.NodeClaim) domain.NodeClaimInput {
	return domain.NodeClaimInput{
		StyleID:               c.StyleID,
		CoreNodeName:          c.CoreNodeName,
		Role:                  c.Role,
		SwapMode:              c.SwapMode,
		PayloadCode:           c.PayloadCode,
		UOPCapacity:           c.UOPCapacity,
		ReorderPoint:          c.ReorderPoint,
		ReorderPointSource:    c.ReorderPointSource,
		AutoReorder:           c.AutoReorder,
		InboundStaging:        c.InboundStaging,
		OutboundStaging:       c.OutboundStaging,
		InboundSource:         c.InboundSource,
		OutboundDestination:   c.OutboundDestination,
		AllowedPayloadCodes:   c.AllowedPayloadCodes,
		AutoRequestPayload:    c.AutoRequestPayload,
		KeepStaged:            c.KeepStaged,
		EvacuateOnChangeover:  c.EvacuateOnChangeover,
		PairedCoreNode:        c.PairedCoreNode,
		SecondPairedCoreNode:  c.SecondPairedCoreNode,
		AutoConfirm:           c.AutoConfirm,
		Sequence:              c.Sequence,
		LinesideSoftThreshold: c.LinesideSoftThreshold,
		ReuseCompatibleBins:   c.ReuseCompatibleBins,
		AutoPush:              c.AutoPush,
	}
}

// CellReorderInput is the write shape for the cell-side reorder_point
// + reorder_point_source pair.
type CellReorderInput struct {
	ClaimID      int64
	ReorderPoint int
	Source       string
	AutoReorder  bool
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
	// Build NodeClaimInput from existing claim so we don't clobber
	// fields outside this admin path's concern.
	upd := processClaimToInput(current)
	upd.ReorderPoint = in.ReorderPoint
	upd.ReorderPointSource = source
	upd.AutoReorder = in.AutoReorder
	if _, err := e.db.UpsertStyleNodeClaim(upd); err != nil {
		return fmt.Errorf("cell reorder: upsert: %w", err)
	}
	return nil
}
