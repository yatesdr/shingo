package dispatch

import (
	"fmt"

	"shingocore/dispatch/binsource"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// loader_source.go — the store-aware bridge to the pure binsource ranker. The
// ranker is store-free; this file adapts bins rows to Cand and assembles a
// dedicated home loader's pool (its member nodes) to source from.

// candFromBin adapts one store bin to a binsource.Cand.
//   - Payload "" marks an empty (the store normalizes a NULL/empty payload to "").
//   - Claimed is derived from ClaimedBy.
//   - LoadedAt stays a pointer so an empty's nil falls back to CreatedAt in the FIFO key.
func candFromBin(b *bins.Bin) binsource.Cand {
	return binsource.Cand{
		BinID:             b.ID,
		Payload:           b.PayloadCode,
		UOP:               b.UOPRemaining,
		Cap:               b.UOPCapacity,
		LoadedAt:          b.LoadedAt,
		CreatedAt:         b.CreatedAt,
		Claimed:           b.ClaimedBy != nil,
		Locked:            b.Locked,
		ManifestConfirmed: b.ManifestConfirmed,
		Status:            b.Status,
	}
}

// sourceFromDedicatedLoader is the dedicated-home-loader source path. If
// sourceNodeName is a loader member position, it ranks the loader's WHOLE pool —
// every member node: the payload-pinned home positions AND the unmarked buffer
// nodes (members with a blank payload) — with binsource.Source, and returns the
// chosen bin plus the node it sits at. That is what lets a cell bound to one home
// position consume a partial of X parked in the buffer: sourcing is over the
// loader's pool, not one slot.
//
//   - isLoaderPos=false → not a loader position; the caller falls back to its
//     normal (supermarket / global) sourcing, unchanged.
//   - isLoaderPos=true, bin=nil → a loader position but no eligible bin of X in
//     the pool; the caller QUEUES (must not fall through to the global scan, which
//     would pull plant-wide).
//   - isLoaderPos=true, bin!=nil → the chosen bin and the node it is parked at.
func (s *PlanningService) sourceFromDedicatedLoader(sourceNodeName, payloadCode string, intent binsource.Intent) (bin *bins.Bin, binNode *nodes.Node, isLoaderPos bool, err error) {
	srcNode, err := s.db.GetNodeByDotName(sourceNodeName)
	if err != nil || srcNode == nil {
		return nil, nil, false, nil //nolint:nilerr // unresolvable name → not a loader position
	}
	home, err := s.db.GetLoaderHomeByPositionNode(srcNode.ID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader for node %s: %w", sourceNodeName, err)
	}
	if home == nil {
		return nil, nil, false, nil // not a dedicated-loader position
	}

	// Pool = every member of the loader: pinned home positions + blank-payload
	// buffer nodes. bin_loader_homes holds both, so one read gives the whole pool.
	members, err := s.db.ListLoaderHomes(home.LoaderID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list loader %d members: %w", home.LoaderID, err)
	}
	cands := make([]binsource.Cand, 0, len(members))
	byID := make(map[int64]*bins.Bin)
	for _, m := range members {
		slotBins, berr := s.db.ListBinsByNode(m.PositionNodeID)
		if berr != nil {
			continue // skip a stale member rather than fail the whole source
		}
		for _, b := range slotBins {
			cands = append(cands, candFromBin(b))
			byID[b.ID] = b
		}
	}

	best, ok := binsource.Source(cands, binsource.Want{Payload: payloadCode, Intent: intent})
	if !ok {
		return nil, nil, true, nil // loader position, no eligible bin of X → caller queues
	}
	chosen := byID[best.BinID]
	node, err := s.db.GetNode(*chosen.NodeID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("resolve node for bin %d: %w", chosen.ID, err)
	}
	return chosen, node, true, nil
}
