package binresolver

import (
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// isBinAvailableForRetrieve checks if a bin can be claimed for retrieval.
func isBinAvailableForRetrieve(b *bins.Bin, payloadCode string) bool {
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
func IsAvailableAtConcreteNode(b *bins.Bin, payloadCode string) bool {
	return BinUnavailableReason(b, payloadCode) == ""
}

// BinUnavailableReason is the reason-returning sibling of IsAvailableAtConcreteNode.
// Returns "" when the bin is available; otherwise a short, log-friendly string
// describing why the bin was rejected.
//
// Exists so callers (claimComplexBins, planning_service) can tell operators
// WHY a bin at the right node was skipped — the previous d.dbg only logged
// payload mismatches, leaving claimed_by / status rejections silent. That
// silence is what made the ALN_002 → SMN_003 incident (2026-04-23) hard to
// root-cause: the line bin was visibly there with a matching payload, but no
// log explained why the claim silently failed.
//
// Keep this in lockstep with IsAvailableAtConcreteNode — adding a new reject
// rule there means adding a new branch here.
func BinUnavailableReason(b *bins.Bin, payloadCode string) string {
	if b.ClaimedBy != nil {
		return fmt.Sprintf("already claimed by order %d", *b.ClaimedBy)
	}
	switch b.Status {
	case "maintenance", "flagged", "retired", "quality_hold":
		return fmt.Sprintf("status=%q rejects pickup", b.Status)
	}
	if payloadCode != "" && b.PayloadCode != "" && b.PayloadCode != payloadCode {
		return fmt.Sprintf("payload %q does not match order payload %q", b.PayloadCode, payloadCode)
	}
	return ""
}

// storageCandidate represents a potential storage slot for ranking.
type storageCandidate struct {
	node     *nodes.Node
	hasMatch bool
	count    int
}

// bestStorageCandidate picks the best slot: prefer consolidation, then emptiest.
func bestStorageCandidate(candidates []storageCandidate) *nodes.Node {
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
