package dispatch

import (
	"fmt"

	"shingocore/dispatch/binsource"
	"shingocore/store/bins"
	"shingocore/store/loaders"
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
// sourceNodeName is a position on a dedicated_positions loader, it ranks the
// loader's WHOLE pool — its payload-pinned home positions AND its buffer slots
// (home_kind=buffer) — with binsource.Source, and returns the chosen bin plus the
// node it sits at. That is what lets a cell bound to one home position consume a
// partial of X parked in the buffer: sourcing is over the loader's pool, not one
// slot. A shared_window (market) loader's windows live in the same table but are
// layout-gated out below (D5) so a window name never enters the flat-pool ranker.
//
//   - isLoaderPos=false → not a loader position; the caller falls back to its
//     normal (supermarket / global) sourcing, unchanged.
//   - isLoaderPos=true, bin=nil → a loader position but no eligible bin of X in
//     the pool; the caller QUEUES (must not fall through to the global scan, which
//     would pull plant-wide).
//   - isLoaderPos=true, bin!=nil → the chosen bin and the node it is parked at.
func (s *PlanningService) sourceFromDedicatedLoader(sourceNodeName, payloadCode string, intent binsource.Intent) (bin *bins.Bin, binNode *nodes.Node, isLoaderPos bool, err error) {
	srcNode, err := s.db.GetNodeByDotName(sourceNodeName)
	if err != nil {
		// A real lookup error must NOT be reported as "not a loader position" — that
		// would fall the caller through to the global plant-wide scan (the very bug
		// this path fixes). Propagate so the order queues instead of pulling globally.
		return nil, nil, false, fmt.Errorf("resolve source node %s: %w", sourceNodeName, err)
	}
	if srcNode == nil {
		return nil, nil, false, nil // name doesn't resolve to a node → not a loader position
	}
	home, err := s.db.GetLoaderHomeByPositionNode(srcNode.ID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader for node %s: %w", sourceNodeName, err)
	}
	if home == nil {
		return nil, nil, false, nil // not a loader position at all
	}
	// Layout gate (D5 / M3): Source ranks dedicated_positions loaders only. A
	// shared_window loader ALSO stores its windows in bin_loader_homes, so without
	// this a window node name would be ranked as a flat pool, bypassing the
	// supermarket/seam semantics that govern a market loader. A non-dedicated (or
	// vanished/archived) loader → treat as "not a loader source" and fall through.
	loader, err := s.db.GetLoader(home.LoaderID)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolve loader %d for node %s: %w", home.LoaderID, sourceNodeName, err)
	}
	if loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
		return nil, nil, false, nil
	}

	// Pool = the loader's sourceable members: pinned home positions + buffer slots
	// (kept partials). An UNPINNED home (home_kind=home, no payload yet) is inert
	// and excluded (InSourcePool) so a stray bin on a half-configured position is
	// never sourced — the D4 buffer/unpinned-home disambiguation, keyed on
	// home_kind rather than the overloaded blank payload.
	members, err := s.db.ListLoaderHomes(home.LoaderID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list loader %d members: %w", home.LoaderID, err)
	}
	// Collect the sourceable members' node ids, then read every bin across them in
	// ONE query (ListBinsByNodes) rather than N per-member reads on the hot path. A
	// read error now FAILS the source (propagates → the order queues) instead of
	// being swallowed per member — a swallowed read silently shrank the pool and
	// could mis-source (pick a newer bin, or strand a parked partial).
	poolNodes := make([]int64, 0, len(members))
	for _, m := range members {
		if !m.InSourcePool() {
			continue // unpinned home — inert, not a buffer
		}
		poolNodes = append(poolNodes, m.PositionNodeID)
	}
	slotBins, err := s.db.ListBinsByNodes(poolNodes)
	if err != nil {
		return nil, nil, true, fmt.Errorf("list bins for loader %d pool: %w", home.LoaderID, err)
	}
	cands := make([]binsource.Cand, 0, len(slotBins))
	byID := make(map[int64]*bins.Bin, len(slotBins))
	for _, b := range slotBins {
		cands = append(cands, candFromBin(b))
		byID[b.ID] = b
	}

	best, ok := binsource.Source(cands, binsource.Want{Payload: payloadCode, Intent: intent})
	if !ok {
		return nil, nil, true, nil // loader position, no eligible bin of X → caller queues
	}
	chosen := byID[best.BinID]
	if chosen == nil || chosen.NodeID == nil {
		// Defensive: Source only returns a BinID it was handed, and a pool bin always
		// carries the node it was read at — but never deref a nil node id on the hot path.
		return nil, nil, true, fmt.Errorf("loader %d chose bin %d with no resolvable node", home.LoaderID, best.BinID)
	}
	node, err := s.db.GetNode(*chosen.NodeID)
	if err != nil {
		return nil, nil, true, fmt.Errorf("resolve node for bin %d: %w", chosen.ID, err)
	}
	return chosen, node, true, nil
}
