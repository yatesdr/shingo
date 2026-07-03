package dispatch

import (
	"shingocore/dispatch/binsource"
	"shingocore/store/bins"
)

// loader_source.go — the store→binsource adapter. The ranker (binsource) is
// store-free; candFromBin adapts a bins row to a binsource.Cand. The store-aware
// pool gather that used to live here (sourceFromDedicatedLoader) moved onto
// dispatch.SourceFinder (source_finder.go) so the dedicated-loader tier is
// reachable only through the one shared finder.

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
