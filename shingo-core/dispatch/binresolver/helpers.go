package binresolver

import (
	"fmt"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// isBinAvailableForRetrieve checks if a bin can be claimed for retrieval, by the
// order asking.
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
// A PENDING RESERVATION IS ASKED OWNER-AWARE (B1, Rule 4). This used to leave
// reservations out entirely, and the reason given was half right. The owner-blind
// HasPendingReservation would refuse the order the bin IT ALREADY RESERVED
// whenever the resolver re-runs (scanner replay, a reconcile tick), so the order
// could never source it: that half stands, and reservedByStranger lets the
// holder through. The other half, that a stranger's reservation is refused one
// layer down by uq_reservations_bin_active so nothing is lost, was wrong. The
// choice was lost: the resolver handed back a bin somebody else had planned for,
// the reserve then conflicted, and the order parked as though it had lost a race
// when a free bin stood beside it. The holder rides the bin projection, so the
// question costs no read.
//
// Status composes on the shared rule instead of restating it: sourceable, and
// additionally not 'staged', because a retrieve from a group must not take a bin
// an operator is working at. Equivalent to the previous `!= available` for every
// declared status.
//
// A carrier counted to nothing is no stock, and neither is one on quality hold,
// as the stock fragment says (helpers.BinCarriesSourceableStockSQL): this is
// that fragment's Go twin for a group's slot children, which the lane mouth
// asks in SQL. AND THE CARRIER RULE, as the lane mouth asks it
// (helpers.PayloadBinTypeRuleArm): rule is the part's (domain.BinTypeRule),
// read once per scan by the caller (Store.LoadBinTypeRule), so a slot child in
// a carrier its part may not ride, a bare marker among them, is not taken. A
// part-less ask names no rule. Rule 6's table holds it
// (TestRule6_OneAvailabilityTable, group_slot_child).
func isBinAvailableForRetrieve(b *bins.Bin, payloadCode string, rule domain.BinTypeRule, asker domain.DigAsker) bool {
	if b.ClaimedBy != nil || b.Locked || !b.ManifestConfirmed || !b.Stocked() || b.QualityHold {
		return false
	}
	if reservedByStranger(b, asker) {
		return false
	}
	if !b.Status.Sourceable() || b.Status == domain.BinStatusStaged {
		return false
	}
	if payloadCode != "" && (b.PayloadCode != payloadCode || !rule.Permits(b.BinTypeID)) {
		return false
	}
	return true
}

// reservedByStranger reports whether a pending reservation on b belongs to an
// order other than the asker. The asker's own reservation is not a reason to
// refuse; domain.Anyone (order 0) holds none, so for it every pending
// reservation is a stranger's, which is exactly the owner-blind answer.
func reservedByStranger(b *bins.Bin, asker domain.DigAsker) bool {
	if !b.HasPendingReservation {
		return false
	}
	return b.PendingReservedBy == nil || *b.PendingReservedBy != asker.OrderID || asker.OrderID == 0
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
//     are always staged — BinService.ApplyArrival sets staged for non-storage slots.
//
// The payload filter only rejects a mismatch when both sides are non-empty:
//
//	payloadCode != "" && bin.PayloadCode != "" && bin.PayloadCode != payloadCode
//
// This catches "wrong part parked at wrong station" while allowing the normal
// post-completion state (cleared bin with empty payload_code) to pass through.
func IsAvailableAtConcreteNode(b *bins.Bin, payloadCode string, binTypes domain.BinTypeRule) bool {
	return BinUnavailableReason(b, payloadCode, binTypes) == ""
}

// BinUnavailableReason is BinUnavailableReasonFor asked by nobody in particular
// (domain.Anyone): every pending reservation is a stranger's, which is the
// owner-blind answer its callers without an asker have always had.
func BinUnavailableReason(b *bins.Bin, payloadCode string, binTypes domain.BinTypeRule) string {
	return BinUnavailableReasonFor(b, payloadCode, binTypes, domain.Anyone)
}

// BinUnavailableReasonFor is the PickableAt question (SYNTH F6): may the asker
// pick up this bin, which stands at the node it named? It is the reason-returning
// sibling of IsAvailableAtConcreteNode.
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
//
// WHERE IT DIVERGES FROM Sourceable (bins.BinSourceableSQL), BY DESIGN (owner,
// 2026-09-26): a STAGED bin is pickable here and not sourceable there. Sourceable
// asks whether the plant may take the bin for a demand ("staged keeps its
// guards": no line stealing); this asks what stands at a node somebody named,
// and a bin arriving at a line node is staged. Where they AGREE: a bin on a
// synthetic node is refused by both (_TRANSIT is on a robot, not at a place),
// and so is a stranger's pending reservation.
func BinUnavailableReasonFor(b *bins.Bin, payloadCode string, binTypes domain.BinTypeRule, asker domain.DigAsker) string {
	if b.ClaimedBy != nil {
		return fmt.Sprintf("already claimed by order %d", *b.ClaimedBy)
	}
	if b.AtSyntheticNode {
		return fmt.Sprintf("at synthetic node %q, not at a place", b.NodeName)
	}
	if reservedByStranger(b, asker) {
		// Another order's: the asker's own reservation never reaches here.
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
	// THE CARRIER RULE, ASKED LAST and only when a part was named. A door
	// fetching "whatever is resident" — a removal leg, an empty pickup whose
	// step names no part — passes payloadCode "" and an unrestricted rule, and
	// this is inert for it. Where a part IS named, the bin is being taken FOR
	// that part, so it must be a carrier the part may travel in whether it
	// already holds it (a part supply's stock) or is an empty about to (an
	// empty pickup naming the part it is for: the produce spare, a changeover's
	// refill).
	//
	// Asked after the payload match so the reason stays the useful one: a bin of
	// the wrong part reports the mismatch, not its type.
	if payloadCode != "" && !binTypes.Permits(b.BinTypeID) {
		// Named by CODE where the row carries one and by id otherwise. Callers
		// reach this predicate through BinJoinQuery, which joins bin_types and
		// fills the code — but not every construction of a bins.Bin does, and a
		// reason that reads `bin type ""` names nothing an operator can act on.
		what := b.BinTypeCode
		if what == "" {
			what = fmt.Sprintf("#%d", b.BinTypeID)
		}
		return fmt.Sprintf("bin type %q may not carry payload %q", what, payloadCode)
	}
	return ""
}

// EmptyBinsOnly returns the candidates that are empty carriers (no bound
// payload) — the set eligible for a produce node's empty pickup leg — and keeps
// the bare rule and nothing else. Whether tops & bottoms among them may be
// bound is BindableEmpties', asked after it.
//
// A BARE CART IS NOT AN EMPTY, though it has no payload: it has no bin on it to
// fill. This filter never composed bins.EmptyCarrierWhere, so it needs the rule
// spelled here (TestTier4_NeverOffersABareCart).
func EmptyBinsOnly(candidates []*bins.Bin) []*bins.Bin {
	out := make([]*bins.Bin, 0, len(candidates))
	for _, b := range candidates {
		if b.PayloadCode == "" && !b.BinTypeBare {
			out = append(out, b)
		}
	}
	return out
}

// StockOnly keeps the candidates that are stock of part: loaded with it, its
// manifest confirmed, counted above 0 — the stock fragment's carrier half
// (helpers.BinCarriesSourceableStockSQL), asked of what stands at the node a
// pickup names, where BinUnavailableReasonFor then judges the rest (a hold,
// the status, the carrier rule). It is the part-supply pickup's bind, ground
// rule 5's "stock of a part": a carrier counted to nothing, one with no part,
// tops & bottoms and the bare marker are no stock of it, on a produce press as
// on a consume one (TestRule5_EveryStepKindAgainstEveryCarrierState). A
// pickup that names no part, on the step or the order, keeps every candidate:
// it lifts whatever stands there.
//
// BEFORE IT, the part-supply arm judged by BinUnavailableReasonFor alone,
// which asks no count and refuses only ANOTHER part, so an unstamped empty
// passed as a full: a consume press's on-deck set a part-less carrier on the
// press, and a changeover's produce index leg lifted its empty by naming the
// part. The Edge now says Empty for a pickup that lifts an empty.
func StockOnly(candidates []*bins.Bin, part string) []*bins.Bin {
	if part == "" {
		return candidates
	}
	out := make([]*bins.Bin, 0, len(candidates))
	for _, b := range candidates {
		if b.PayloadCode == part && b.ManifestConfirmed && b.Stocked() {
			out = append(out, b)
		}
	}
	return out
}

// BindAt is what a bind knows of where an empty pickup lifts: whether the node
// is a claim's own position (its line, paired or second paired node), and the
// height the carrier is wanted at where it is next set down.
type BindAt struct {
	LinePosition bool
	SetDown      domain.Height
}

// BindableEmpties keeps the empties a pickup standing at a node may bind, after
// EmptyBinsOnly. Tops & bottoms binds where the node is a cell's own position —
// the house rule: a carrier on a cell's own position belongs to that cell, so a
// press's index lifts what SEND BACK landed on its outside whatever height the
// press would ask for — or where the set-down height admits it (a fed press's
// core wants Layers(0)). Every other empty binds where the set-down height
// admits it, which for any height but tops & bottoms alone is every one.
// Otherwise it is dropped: a refill sourced from a concrete supply node holding
// only tops & bottoms, set down on an outside that wants a stack, waits.
// Its readers: the reserve's bind (dispatch findAvailableForNeed) and the
// finder's named-source empties (Pick, tier 4's namedEmpty).
func BindableEmpties(candidates []*bins.Bin, at BindAt) []*bins.Bin {
	out := make([]*bins.Bin, 0, len(candidates))
	for _, b := range candidates {
		s := b.CarrierState()
		if at.SetDown.Admits(s) || (at.LinePosition && s == protocol.CarrierTopsAndBottoms) {
			out = append(out, b)
		}
	}
	return out
}

// AnyTopsAndBottoms reports whether tops & bottoms stands among the candidates:
// the one carrier a bind has to ask about where it stands, so the read that
// answers (IsLinePosition) is made only when it is there.
func AnyTopsAndBottoms(candidates []*bins.Bin) bool {
	for _, b := range candidates {
		if b.CarrierState() == protocol.CarrierTopsAndBottoms {
			return true
		}
	}
	return false
}

// OfTypes keeps the candidates of the wanted carrier types; nil wants any.
func OfTypes(candidates []*bins.Bin, types []string) []*bins.Bin {
	if len(types) == 0 {
		return candidates
	}
	out := make([]*bins.Bin, 0, len(candidates))
	for _, b := range candidates {
		for _, t := range types {
			if b.BinTypeCode == t {
				out = append(out, b)
				break
			}
		}
	}
	return out
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
