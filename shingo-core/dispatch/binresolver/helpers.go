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

// IsAvailableAtConcreteNode checks if a bin can be claimed at a concrete
// (non-synthetic) node for lineside pickup.
//
// Two relaxations vs isBinAvailableForRetrieve:
//
//  1. ManifestConfirmed is not required. A cleared bin (post-completion
//     state where ClearAndClaim zeroed payload_code, manifest, and
//     manifest_confirmed) is a valid pickup target at a lineside slot.
//
//  2. Status "staged" is accepted (not just "available"). Lineside bins
//     are always staged — ApplyBinArrival sets staged for non-storage slots.
//
// The payload filter only rejects a mismatch when both sides are non-empty:
//
//	payloadCode != "" && bin.PayloadCode != "" && bin.PayloadCode != payloadCode
//
// This catches "wrong part parked at wrong station" while allowing the normal
// post-completion state (cleared bin with empty payload_code) to pass through.
func IsAvailableAtConcreteNode(b *store.Bin, payloadCode string) bool {
	if b.ClaimedBy != nil {
		return false
	}
	switch b.Status {
	case "maintenance", "flagged", "retired", "quality_hold":
		return false
	}
	if payloadCode != "" && b.PayloadCode != "" && b.PayloadCode != payloadCode {
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
