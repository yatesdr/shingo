package binresolver

import (
	"fmt"

	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// isBinAvailableForRetrieve checks if a bin can be claimed for retrieval.
//
// Locked is checked here rather than only downstream. Without it this predicate
// selected bins the claim could never take: a LOCKED bin passed selection,
// acquired a soft reservation (Acquire has no locked predicate), then failed the
// hard claim, whose CAS requires locked=false. The order requeued as
// claim-failed -- a reason implying a lost race rather than "the resolver picked
// a locked bin". Worse, that reservation is not released on confirm failure and
// carries no expiry, so the bin then disappeared from every reader that excludes
// bins with a pending reservation until the order terminalised. A locked bin
// read as a material shortage.
//
// HasPendingReservation is deliberately NOT checked, and must not be added.
// The field is OWNER-BLIND: it is true when ANY order holds a pending
// reservation, including this order's own. BinUnavailableReason can consult it
// safely because its caller (the reserve reconcile) loads its own holds first;
// this path has no such step. Refusing here would therefore refuse an order the
// bin IT ALREADY RESERVED whenever the resolver re-runs -- scanner replay or a
// reconcile tick -- and the order could never source it. Another order's
// reservation is already refused one layer down by uq_reservations_bin_active,
// so nothing is lost by leaving it out.
//
// Status composes on the shared rule instead of restating it: sourceable, and
// additionally not 'staged', because a retrieve from a group must not take a bin
// an operator is working at. Equivalent to the previous `!= available` for every
// declared status.
func isBinAvailableForRetrieve(b *bins.Bin, payloadCode string) bool {
	if b.ClaimedBy != nil || b.Locked || !b.ManifestConfirmed {
		return false
	}
	if !b.Status.Sourceable() || b.Status == domain.BinStatusStaged {
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

// PropResolveAround is the node-property key (read on a lane's GROUP) that turns
// on lane-aware resolve-around for storage. When "on", the store ranker prefers a
// lane whose mouth is currently free of a conflicting hold, so an order need not
// stall at a mode-held lane. Unset (or anything but "on") = off, and the ranking
// is byte-identical to before. Set per group in the Core web UI node editor.
//
// Note: lanes carry NO depth — only their slots do (seed_core.go seeds LANE nodes
// with depth nil), so in a lane group every lane's ranking depth is 0 and this
// preference effectively orders all of the group's lanes (prefer compatible, then
// emptiest). It still sorts BELOW depth in candidateBetter, which keeps it inert
// for the direct-slot branch where depth is real. Opportunistic and never load-
// bearing (§13.3): the mouth gate still arbitrates admission. (The store-ranker
// depth overload is tracked as tech-debt in SHINGO_TODO.md.)
const PropResolveAround = "resolve_around"

// storageCandidate represents a potential storage slot for ranking.
type storageCandidate struct {
	node     *nodes.Node
	hasMatch bool
	count    int
	depth    int // lane/slot depth; higher = further back. Packs deepest-first.
	// laneCompatible is the resolve-around hint: the lane's mouth is currently
	// free of a conflicting hold. Only ever set when the group enables the arm;
	// false otherwise, which leaves the ranking unchanged (see candidateBetter).
	laneCompatible bool
}

// bestStorageCandidate picks the best slot. Precedence:
//  1. consolidate with a matching payload (hasMatch),
//  2. pack to the back — prefer the deeper lane/slot (higher depth),
//  3. resolve-around: when a group enables it, prefer a mouth-compatible lane
//     (lanes all rank at depth 0, so this is the effective cross-lane order among
//     lanes; neutral when the arm is off),
//  4. then the emptiest lane (lowest count) as a final tiebreak.
//
// Depth packing applies under LKND too: LKND vs DPTH differ only in which lane
// wins, never in whether the deepest slot is preferred. Before, LKND dropped
// bins in the emptiest lane regardless of depth, which read on the floor as
// "picks the most-open spot instead of packing to the back."
//
// ── ARM 2 IS INERT IN EVERY PLANT SPEC IN THE REPO, AND THAT IS NOT A BUG ──
//
// It ranks whatever nodeDepth returns for the CANDIDATE this loop built, and in
// both branches that value ties today:
//
//   - LANE branch: the candidate's depth is the LANE's, and seeddev creates
//     lanes with a nil depth (seed_core.go's ensureNode call for ln.Name), so
//     every lane ranks 0. The comment three lines above already says this.
//   - FLAT branch: the candidate is the slot itself, and the only flat group in
//     any spec is demo.yaml's SYN_PRESS_EMPTIES, whose eight positions are all
//     `depth: 1`.
//
// SO THE REAL DEEPEST-FIRST IS NOT HERE. It is findStoreSlot's own
// `ORDER BY COALESCE(n.depth,0) DESC` (store/nodes/lanes.go), which ranks SLOTS
// inside the lane this function has already chosen. Two different questions —
// which lane, then which slot in it — and only the second one currently
// discriminates. Somebody debugging a store that packed to the wrong place needs
// to know which of the two they are looking at.
//
// The arm stays. It is correct, it is free, and it becomes live the day a spec
// gives its lanes depths or a flat group varies them — which is a configuration
// change, not a code change, so deleting the arm would silently change behaviour
// on a plant nobody rebuilt.
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
	// Resolve-around (opportunistic; off unless the group enables it): prefer a
	// lane whose mouth is currently compatible so the order need not stall there.
	// It sorts BELOW depth, which keeps it inert in the direct-slot branch where
	// depth is real; lanes themselves carry no depth (all rank at 0), so for a lane
	// group this is the effective cross-lane order — prefer compatible, then
	// emptiest. When the arm is off every candidate's laneCompatible is false and
	// this comparison is a no-op, leaving the ranking byte-identical.
	if c.laneCompatible != best.laneCompatible {
		return c.laneCompatible // compatible lane wins the equal-depth tie
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
