package binresolver

import (
	"fmt"

	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// isBinAvailableForRetrieve checks if a bin can be claimed for retrieval.
func isBinAvailableForRetrieve(b *bins.Bin, payloadCode string) bool {
	if b.ClaimedBy != nil || !b.ManifestConfirmed || b.Status != domain.BinStatusAvailable {
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
// Exists so callers (the complex claim path, planning_service) can tell operators
// WHY a bin at the right node was skipped — the previous d.dbg only logged
// payload mismatches, leaving claimed_by / status rejections silent. That
// silence is what made the ALN_002 → SMN_003 incident (2026-04-23) hard to
// root-cause: the line bin was visibly there with a matching payload, but no
// log explained why the claim silently failed.
//
// The status reject-set is domain.BinStatus.BlocksPickup — the SAME predicate the
// pure loader ranker (binsource.eligible) uses, so the concrete path and the ranker
// can no longer drift. IsAvailableAtConcreteNode derives from this.
//
// The locked check matches the ranker too: a locked bin was already unclaimable
// (the claim guards locked=false, service/bin_manifest.go), so surfacing it here is
// zero behaviour change — it just explains the skip instead of letting the claim
// silently fail.
func BinUnavailableReason(b *bins.Bin, payloadCode string) string {
	if b.ClaimedBy != nil {
		return fmt.Sprintf("already claimed by order %d", *b.ClaimedBy)
	}
	if b.HasPendingReservation {
		// Owner-blind: HasPendingReservation is EXISTS(pending) on the bin, which
		// includes THIS order's own hold, so don't claim "another order".
		return "pending reservation held"
	}
	if b.Locked {
		return "locked for active handling"
	}
	if b.Status.BlocksPickup() {
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
	depth    int // lane/slot depth; higher = further back. Packs deepest-first.
}

// bestStorageCandidate picks the best slot. Precedence:
//  1. consolidate with a matching payload (hasMatch),
//  2. pack to the back — prefer the deeper lane/slot (higher depth),
//  3. then the emptiest lane (lowest count) as a final tiebreak.
//
// Depth packing applies under LKND too: LKND vs DPTH differ only in which lane
// wins, never in whether the deepest slot is preferred. Before, LKND dropped
// bins in the emptiest lane regardless of depth, which read on the floor as
// "picks the most-open spot instead of packing to the back."
func bestStorageCandidate(candidates []storageCandidate) *nodes.Node {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if candidateBetter(c, best) {
			best = c
		}
	}
	return best.node
}

func candidateBetter(c, best storageCandidate) bool {
	if c.hasMatch != best.hasMatch {
		return c.hasMatch // consolidation wins
	}
	if c.depth != best.depth {
		return c.depth > best.depth // deeper (further back) wins — pack to the back
	}
	return c.count < best.count // emptiest as a final tiebreak
}

// nodeDepth returns a node's configured depth, treating unset (nil) as 0
// (front-most), so depth-ordered lanes pack ahead of undepthed ones.
func nodeDepth(n *nodes.Node) int {
	if n != nil && n.Depth != nil {
		return *n.Depth
	}
	return 0
}
