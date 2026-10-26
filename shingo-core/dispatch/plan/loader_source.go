package plan

import (
	"shingocore/dispatch/binsource"
	"shingocore/store/bins"
)

// loader_source.go — the store→binsource adapter. The ranker (binsource) is
// store-free; candFromBin adapts a bins row to a binsource.Cand. The store-aware
// pool gather that used to live here (sourceFromDedicatedLoader) moved onto
// the Finder (finder.go) so the dedicated-loader tier is reachable only
// through the one shared finder.

// reservedBy is the pending reservation's holder, 0 when there is none.
func reservedBy(b *bins.Bin) int64 {
	if b.PendingReservedBy == nil {
		return 0
	}
	return *b.PendingReservedBy
}

// candFromBin adapts one store bin to a binsource.Cand.
//   - Payload "" marks an empty (the store normalizes a NULL/empty payload to "").
//   - Claimed is derived from ClaimedBy.
//   - LoadedAt stays a pointer so an empty's nil falls back to CreatedAt in the FIFO key.
//   - BinTypeID rides along so the ranker can judge the carrier against the
//     part's payload_bin_types rule, which arrives separately on the Want.
func candFromBin(b *bins.Bin) binsource.Cand {
	return binsource.Cand{
		BinID:             b.ID,
		BinTypeID:         b.BinTypeID,
		Payload:           b.PayloadCode,
		UOP:               b.UOPRemaining,
		Cap:               b.UOPCapacity,
		LoadedAt:          b.LoadedAt,
		CreatedAt:         b.CreatedAt,
		Claimed:           b.ClaimedBy != nil,
		Locked:            b.Locked,
		ReservedBy:        reservedBy(b),
		Bare:              b.BinTypeBare,
		ManifestConfirmed: b.ManifestConfirmed,
		Status:            b.Status,
	}
}

// liftWhereTheReturnLands narrows a pool pick to the bins standing on liftFrom,
// the spots a swap's returning carrier can land on once this leg lifts from one
// of them (Need.LiftFrom). The ranker still decides among them, so it is FIFO
// over the spots that work. It answers fifo itself when fifo already stands on
// such a spot, and when no eligible bin does: the return then waits, as it did
// before.
//
// A changeover at a two-buffer loader cell is the case it exists for. The
// incoming part's oldest cart stood in its pinned home, so FIFO lifted it there;
// that home will not take the outgoing part, the outgoing part's home and both
// buffers were full, and the pair waited until it was cancelled. The incoming
// part's other cart stood in a buffer, and lifting that one gives the returning
// cart its spot (Hopkinsville 4x2, 2026-10-26).
func liftWhereTheReturnLands(cands []binsource.Cand, byID map[int64]*bins.Bin, liftFrom []int64, want binsource.Want, fifo binsource.Cand) binsource.Cand {
	on := make(map[int64]bool, len(liftFrom))
	for _, id := range liftFrom {
		on[id] = true
	}
	standsOn := func(binID int64) bool {
		b := byID[binID]
		return b != nil && b.NodeID != nil && on[*b.NodeID]
	}
	if standsOn(fifo.BinID) {
		return fifo
	}
	var narrowed []binsource.Cand
	for _, c := range cands {
		if standsOn(c.BinID) {
			narrowed = append(narrowed, c)
		}
	}
	if pick, ok := binsource.Source(narrowed, want); ok {
		return pick
	}
	return fifo
}
