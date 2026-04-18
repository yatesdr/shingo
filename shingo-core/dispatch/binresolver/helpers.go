package binresolver

import "shingocore/store"

// isBinAvailableForRetrieve checks if a bin can be claimed for retrieval.
func isBinAvailableForRetrieve(b *store.Bin, payloadCode string) bool {
	if b.ClaimedBy != nil || !b.ManifestConfirmed || b.Status != "available" {
		return false
	}
	if payloadCode != "" && b.PayloadCode != payloadCode {
		return false
	}
	return true
}

// storageCandidate represents a potential storage slot for ranking.
type storageCandidate struct {
	node     *store.Node
	hasMatch bool
	count    int
}

// bestStorageCandidate picks the best slot: prefer consolidation, then emptiest.
func bestStorageCandidate(candidates []storageCandidate) *store.Node {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.hasMatch && !best.hasMatch {
			best = c
		} else if c.hasMatch == best.hasMatch && c.count < best.count {
			best = c
		}
	}
	return best.node
}
