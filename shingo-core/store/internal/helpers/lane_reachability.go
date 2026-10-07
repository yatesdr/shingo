package helpers

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/reservations"
)

// ── Reachability: one definition ──────────────────────────────────────────
//
// A slot is REACHABLE iff no occupied slot sits strictly shallower in the same
// lane. That one sentence used to be spelled seven different ways across the
// tree — a Go loop, a COUNT, four correlated sub-queries, and a sort key — and
// the spellings did not agree. Everything now routes through
// LaneBlockerPredicate: ReachableSQL / BuriedSQL for the query sites whose
// ORDER BY is part of their semantics, nodes.BlockersInFrontOf for the Go sites.
//
// IT LIVES HERE, not beside nodes.IsSlotAccessible where it reads most
// naturally, because two store aggregates need it: store/nodes (the lane and
// slot readers) and store/bins (AccessibleEmptyOrder, which ranks by it). The
// store-sub-pkg-isolation depguard rule forbids one store sub-package importing
// a sibling — cross-aggregate orchestration belongs at the outer store/ level —
// and store/internal is the one allow-listed exception. A shared definition that
// two aggregates must agree on is exactly what that exception is for; the
// alternative was two copies, which is the thing being deleted.
//
// Three choices the old spellings made inconsistently, settled here:
//
//   - THE SIBLING SCOPE IS CORRELATED, not the lane parameter:
//     `blocker.parent_id = target.parent_id`, never `= $laneID`. The two are
//     equivalent today only because every caller's outer clause already pins
//     the target's parent to that lane. The correlated form stays correct if an
//     outer clause ever stops doing that.
//
//   - SELF-EXCLUSION IS EXPLICIT. `blocker.id != target.id` is redundant under
//     strict `<`, which is why three of the old spellings omitted it. It is
//     written anyway, so the predicate survives someone widening `<` to `<=`.
//
//   - NULL DEPTH IS IGNORED, on both sides: a sibling with no depth is not a
//     depth-ordered slot and so cannot be in front of anything, and a target
//     with no depth has nothing in front of it. This is SQL's reading and it
//     was the majority spelling. The Go loop in dispatch/reshuffle.go
//     disagreed, because nodes.GetSlotDepth reports 0 for NULL and 0 is
//     shallower than everything — so a depth-less occupied child made every
//     slot in its lane reachable to the SQL side and blocked to the Go side.
//     Reachable was the ruling; nodes.AuditLaneDepths makes the geometry that
//     provokes it loud at boot rather than letting the two sides differ
//     quietly again.
//
// Occupancy is occupancy: the predicate consults `bins` and nothing else. A
// shallower slot that some other order has merely RESERVED or is inbound to
// does not make a deeper slot unreachable — that case belongs to the mouth gate
// and dispatch.classifyLaneEntry, and folding it in here would change admission
// behaviour plant-wide while looking like a tidy-up.

// ShallowerInSameLane is the GEOMETRY, alone: `shallow` sits strictly in front
// of `deep` in the same lane. No occupancy, no claim, no order — just which slot
// the mouth reaches first. Both arguments name `nodes` rows already in scope: a
// table alias, or a derived table exposing id/parent_id/depth (see
// nodes.BlockersInFrontOf).
//
// ── WHY THE GEOMETRY IS SEPARATE FROM LaneBlockerPredicate ────────────────
//
// It was extracted, not invented, and the extraction is what keeps the
// one-spelling rule true rather than merely asserted. Reachability asks "is an
// OCCUPIED slot in front of my target"; the store-side burial guard
// (nodes.FindStoreSlotInLaneExcluding) asks the mirror — "would MY EMPTY
// candidate be in front of a bin somebody has claimed". Same geometry, opposite
// occupancy: the guard's candidate is empty by definition, so it cannot reuse
// LaneBlockerPredicate, whose EXISTS(bins) term would be false for exactly the
// slot it is asking about.
//
// Written as a second literal, that would have been a second definition of
// in-front-of — the thing TestReachabilityHasExactlyOneSpelling exists to catch,
// and it does catch it. So the comparison stays in ONE string literal here and
// both predicates compose from it. The guard's rule survives the extraction: the
// drift test still finds exactly one depth comparison, and now two readers agree
// by construction instead of by review.
func ShallowerInSameLane(shallow, deep string) string {
	return fmt.Sprintf(`%[1]s.parent_id = %[2]s.parent_id
			  AND %[1]s.id != %[2]s.id
			  AND %[1]s.depth IS NOT NULL
			  AND %[2]s.depth IS NOT NULL
			  AND %[1]s.depth < %[2]s.depth`, shallow, deep)
}

// ComingForBinSQL is the burial guard's HOLDER arm, alone: the order at alias
// `holder` is coming for the bin at alias `bin`, which stands in the slot at
// alias `slot`. Both burial guards (nodes.FindStoreSlotInLaneExcluding and
// store.SlotsBlockedByHardClaims) join their holder through it, so "coming for
// it" means one thing to the store selector and the shuffle walk, as "in front
// of" does through ShallowerInSameLane. Liveness is the caller's: each filters
// the holder on non-terminal status.
//
// THREE SPELLINGS OF ONE FACT:
//   - the hard claim (claimed_by): a robot is committed to the bin;
//   - the aim (bin_id with source_node on the bin's slot): the order resolved
//     onto the bin and is coming to collect it where it stands;
//   - a pending bin reservation held by a demand that dug for it and is not
//     yet with the fleet. A complex demand has no aim to read for its whole
//     wait — its bin_id is written only at its confirm, and source_node names
//     its pickup's group until the pickup resolves — so its dig soft-reserves
//     the bin it uncovers at the dig's birth (C2b A.1b; dispatch
//     digLaneTake.aim), and this arm is what makes that row keep the slots the
//     dig just emptied from re-burying the bin before the demand's next pass.
//
// THE THIRD ARM IS SCOPED TWICE, and both scopes are load-bearing.
//
// "NOT YET SENT" (AwaitingFleetSQL) is its direction, as source_node is the
// aim's. A sent order takes its bins by claim in its hand, so a pending row an
// order still carries after it was sent is not a robot coming: a store that has
// placed its bin in the lane and not yet ended is that shape, and reading its row
// as "coming for it" closes the lane behind the store
// (TestStoreBurst_DivertedOntoAMarkedLane_StagesInsteadOfParking).
//
// "A DEMAND THAT DUG FOR IT" (the holder has dig legs) keeps the arm off soft
// holds in general, which TestBurialGuard_SoftHoldIsBuriable pins and whose
// reason stands: a soft hold has no time bound, so two cross-lane movers parked
// on each other's holds would refuse each other for ever, both alive and both
// correct. A reserve-holding complex order keeps its partial holds for as long as
// it waits; read as burials, those would close every slot in front of them, and
// a plant's tiered slots under a group are lanes to this geometry. The dug-for
// demand is the population the window exists for (the naked target), and its
// wait has a releaser: its own next pass, one pass after the dig ends.
//
// The reservation arm is a scalar subquery, not a join: uq_reservations_bin_active
// admits one active row per bin.
func ComingForBinSQL(holder, bin, slot string) string {
	return fmt.Sprintf(`(%[1]s.id = %[2]s.claimed_by
			   OR (%[1]s.bin_id = %[2]s.id AND %[1]s.source_node = %[3]s.name)
			   OR (%[1]s.id = (SELECT coming_r.order_id FROM reservations coming_r
			                    WHERE coming_r.bin_id = %[2]s.id AND coming_r.resource_kind = 'bin'
			                      AND %[4]s)
			       AND %[5]s
			       AND EXISTS (SELECT 1 FROM orders coming_dig WHERE coming_dig.parent_order_id = %[1]s.id)))`,
		holder, bin, slot, reservations.BlockingStateSQL("coming_r."), AwaitingFleetSQL(holder))
}

// AwaitingFleetSQL is orders.AwaitingFleetSQL's one spelling — see it for why
// vendor_order_id is the authority and the status set is derived. It lives here
// because the burial guard's holder arm reads it, and this package cannot import
// store/orders. alias is the orders table alias ("" for an unaliased `orders`).
func AwaitingFleetSQL(alias string) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	return fmt.Sprintf("%sstatus IN (%s) AND COALESCE(%svendor_order_id, '') = ''",
		q, protocol.PreDispatchStatusSQLList(), q)
}

// LaneBlockerPredicate is the definition: `blocker` is an occupied slot sitting
// strictly shallower than `target` in the same lane. Both arguments name `nodes`
// rows already in scope — a table alias, or a derived table exposing
// id/parent_id/depth (see nodes.BlockersInFrontOf). Returns a bare boolean
// expression so callers can put it wherever they need it.
//
// It is now the OCCUPANCY term plus ShallowerInSameLane, which is what it always
// meant; the three choices settled above (correlated sibling scope, explicit
// self-exclusion, NULL depth ignored) live in the geometry half and are
// unchanged.
func LaneBlockerPredicate(blocker, target string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM bins lane_blocker_bin WHERE lane_blocker_bin.node_id = %s.id)
			  AND %s`, blocker, ShallowerInSameLane(blocker, target))
}

// laneBlockerScan is the sibling scan wrapped by ReachableSQL and BuriedSQL:
// one row per occupied slot in front of `target`.
func laneBlockerScan(target string) string {
	return fmt.Sprintf(`SELECT 1 FROM nodes lane_blocker
			WHERE %s`, LaneBlockerPredicate("lane_blocker", target))
}

// ReachableSQL returns a boolean SQL expression, true iff the slot at alias
// `target` is reachable. Interpolate it into a WHERE clause (or, as
// bins.AccessibleEmptyOrder does, into an ORDER BY key). It references only the
// target's own node columns, so it is uncorrelated to query parameters and
// never shifts a caller's placeholder numbering.
//
// There is no mouth argument, deliberately. The expression is already
// mouth-agnostic — it derives "in front of" from `depth`, not from a direction
// — and there is exactly one mouth to derive it for. The seam that a second
// door plugs into is this single definition, not a parameter every caller would
// pass the same value to.
func ReachableSQL(target string) string {
	return "NOT " + BuriedSQL(target)
}

// BuriedSQL is the negation of ReachableSQL: true iff something occupied sits
// in front of `target`. The buried-bin readers want it in this direction.
func BuriedSQL(target string) string {
	return fmt.Sprintf("EXISTS (\n\t\t\t%s\n\t\t  )", laneBlockerScan(target))
}

// EntombsASpokenForSlotSQL is true iff placing a bin at `candidate` would seal
// an EMPTY slot deeper in the same lane that a live order is coming to fill.
//
// ── THE BUBBLE, AND WHY THE EXISTING GUARDS CANNOT SEE IT ─────────────────
//
// The selectors refuse to PICK a slot somebody else holds, each by its own
// reads. The store-slot finder (nodes.findStoreSlot) reads all three: the slot
// claim, a stranger's slot reservation, and another live order's
// delivery_node. The dig's shuffle pool (dispatch shuffleSlotFree) reads only
// the slot claim and the dropoff gate — bins at the node, and orders holding a
// claimed bin bound for it — so a waiting order's pending reservation or bare
// delivery_node does not remove a slot from that pool; the claim at the moment
// of choice is what refuses it (dig-starvation SYNTH S16). Whatever a selector
// reads removes the deeper slot from its own pool and says nothing about the
// shallower ones. So the selector, offered a lane whose deep slot is spoken
// for, happily fills a shallow one, and the deep slot is now behind a bin.
//
// WHETHER THE HOLE IS PERMANENT DEPENDS ON WHAT SEALS IT, and the difference
// decides whether anything ever recovers. If the sealing bin is one NOBODY WILL
// COME FOR — a store parked in a slot no demand names — the deep slot is lost
// for the life of the plant: no robot can reach it, and no dig will ever be
// raised against it, because the bin in front is not in anybody's way. That is
// both of the measured cases below, and it is the shape this predicate exists
// for.
//
// If instead the sealing bin carries live payload that FIFO will eventually
// pull, the hole is TEMPORARY: the seal leaves when the material is consumed,
// and the deep slot comes back on its own. It still costs capacity and it still
// distorts every lane-depth read while it lasts — it is simply not permanent,
// and a scar that says "for the life of the plant" flat sends somebody hunting
// for a leak that is going to close by itself.
//
// Measured on the lane-stress rig 2026-08-13, both of the run's new bubbles:
//
//	LSD_010 (d5) was order 7's delivery node. Stores took d2 and d3 while it
//	waited, order 7 was cancelled, and d5 was walled behind them forever.
//	LSD_003 (d3) was order 57's delivery node. A dig leg parked its blocker at
//	d2 while order 57 was still driving, and d3 emptied later behind it.
//
// Both are lawful placements by every rule that existed. Neither is recoverable.
//
// ── IT IS THE MIRROR OF THE BURIAL GUARD ──────────────────────────────────
//
// That one protects a deeper BIN somebody is coming to collect. This protects a
// deeper EMPTY SLOT somebody is coming to fill. Same geometry — composed from
// ShallowerInSameLane, so there is still exactly one definition of "in front of"
// — opposite occupancy, and the same three ownership spellings the pickers
// already use.
//
// ownerParam is the SQL placeholder holding the asking order's id; pass a
// literal 0 for an owner-blind reader. A dig is deliberately owner-blind here,
// for the reason the burial exclusion states about itself: "not even your own"
// is the load-bearing half, and a compound that entombs a slot its own next leg
// is driving to has cost itself the same slot as anybody else would.
func EntombsASpokenForSlotSQL(candidate, ownerParam, terminalStatusList string) string {
	return fmt.Sprintf(`EXISTS (
			SELECT 1 FROM nodes spoken
			 WHERE %s
			   AND NOT EXISTS (SELECT 1 FROM bins spoken_bin WHERE spoken_bin.node_id = spoken.id)
			   AND (
			        (spoken.claimed_by IS NOT NULL AND spoken.claimed_by <> %[2]s)
			     OR %[4]s
			     OR EXISTS (SELECT 1 FROM orders spoken_ord
			                 WHERE spoken_ord.delivery_node = spoken.name
			                   AND spoken_ord.status NOT IN (%[3]s)
			                   AND spoken_ord.id <> %[2]s)
			   )
		  )`, ShallowerInSameLane(candidate, "spoken"), ownerParam, terminalStatusList,
		reservations.SlotSpokenForByStrangerSQL("spoken_res", "spoken.id", ownerParam))
}

// ── ONE SPELLING FOR "DOES THIS ORDER MOVE A BIN OF ITS OWN?" ─────────────
//
// A compound PARENT is a folder. It owns legs, it never touches a bin, and its
// bin_id is NULL for that reason and permanently. A defective single-bin order
// also has a NULL bin_id — because planMove failed to persist one, which is a
// real fault worth a diagnostic.
//
// `order.BinID == nil` is TRUE OF BOTH AND CANNOT TELL THEM APART. That is the
// whole defect: it is a true answer to a narrower question, and it reads exactly
// like the answer the caller wanted.
//
// Measured at the pin commit, on the lane-stress rig 2026-08-13: the bin-state
// reconciliation strip reported TWELVE anomalies — ten service digs and two
// buried retrieves, every one of them a compound parent whose legs had delivered
// correctly. Zero true positives. The strip read "Core degraded" for the whole
// run because a coordinator and a broken order are indistinguishable under the
// bin-id spelling.
//
// THE EXEMPTION IS THE CHILD ROWS, not the order type and not a flag — the
// tree's own ruling, at store/reconciliation/reconciliation.go, which is the one
// site that already had this right. Child rows are written in the SAME
// TRANSACTION as the parent's transition (dispatch.CreateCompoundOrder writes
// children BEFORE BeginReshuffle, deliberately), so the fact cannot drift. A
// label beside it can.
//
// SPELLED ONCE, HERE, beside ShallowerInSameLane and for the same reason: one
// geometry, one spelling. Two copies of a predicate is how the Go loop in
// dispatch/reshuffle.go came to disagree with the SQL about lane reachability,
// and that disagreement was silent for months.

// OwnsNoCargoSQL is the SQL half: `alias` names an `orders` row already in
// scope, and the fragment is TRUE when that order owns legs — i.e. it is a
// coordinator and moves no bin of its own.
//
// Returns a bare boolean expression so a caller can put it wherever it needs it,
// matching LaneBlockerPredicate's contract.
func OwnsNoCargoSQL(alias string) string {
	return fmt.Sprintf(
		`EXISTS (SELECT 1 FROM orders leg WHERE leg.parent_order_id = %s.id)`, alias)
}
