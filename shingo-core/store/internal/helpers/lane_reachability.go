package helpers

import "fmt"

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

// LaneBlockerPredicate is the definition: `blocker` is an occupied slot sitting
// strictly shallower than `target` in the same lane. Both arguments name `nodes`
// rows already in scope — a table alias, or a derived table exposing
// id/parent_id/depth (see nodes.BlockersInFrontOf). Returns a bare boolean
// expression so callers can put it wherever they need it.
func LaneBlockerPredicate(blocker, target string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM bins lane_blocker_bin WHERE lane_blocker_bin.node_id = %[1]s.id)
			  AND %[1]s.parent_id = %[2]s.parent_id
			  AND %[1]s.id != %[2]s.id
			  AND %[1]s.depth IS NOT NULL
			  AND %[2]s.depth IS NOT NULL
			  AND %[1]s.depth < %[2]s.depth`, blocker, target)
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
