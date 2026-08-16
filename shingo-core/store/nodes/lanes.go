package nodes

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth (ascending).
func ListLaneSlots(db *sql.DB, laneID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id=$1
		ORDER BY COALESCE(n.depth, 0) ASC`, SelectCols, FromClause), laneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanNodes(rows)
}

// GetSlotDepth returns the depth for a node, or 0 if not set.
func GetSlotDepth(db *sql.DB, nodeID int64) (int, error) {
	var depth *int
	err := db.QueryRow(`SELECT depth FROM nodes WHERE id=$1`, nodeID).Scan(&depth)
	if err != nil {
		return 0, err
	}
	if depth == nil {
		return 0, nil
	}
	return *depth, nil
}

// The reachability predicate itself lives in store/internal/helpers, not here
// beside its readers where it would read most naturally. store/bins needs it too
// (AccessibleEmptyOrder ranks by it) and the store-sub-pkg-isolation depguard
// rule forbids one store aggregate importing a sibling — store/internal is the
// allow-listed exception, and a definition two aggregates must agree on is what
// that exception is for. helpers.LaneBlockerPredicate carries the argument for
// the sibling scope, the self-exclusion, and the NULL-depth ruling.

// BlockersInFrontOf returns every occupied slot strictly shallower than
// slotNodeID in the same lane, shallowest first. It is the set whose emptiness
// IS reachability: IsSlotAccessible is len()==0 over this, and
// dispatch.findBuriedBlockers is this list — so the two stopped being two
// implementations of the same sentence.
//
// A slot with no parent (not in a lane) or no depth (not a depth-ordered slot)
// has nothing in front of it: (nil, nil). A slot that cannot be READ is an
// error, never an empty list. Callers must fail closed — an unreadable lane is
// treated as blocked, because refusing to move is recoverable and moving into a
// lane whose state you could not read is not.
func BlockersInFrontOf(db *sql.DB, slotNodeID int64) ([]*Node, error) {
	slot, err := Get(db, slotNodeID)
	if err != nil {
		return nil, err
	}
	if slot.ParentID == nil || slot.Depth == nil {
		return nil, nil
	}

	rows, err := db.Query(fmt.Sprintf(`WITH lane_target AS (
			SELECT id, parent_id, depth FROM nodes WHERE id = $1
		)
		SELECT %s %s
		CROSS JOIN lane_target
		WHERE %s
		ORDER BY n.depth ASC`, SelectCols, FromClause, helpers.LaneBlockerPredicate("n", "lane_target")), slotNodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanNodes(rows)
}

// IsSlotAccessible returns true if no occupied slots exist at a shallower depth
// in the same lane — the emptiness of BlockersInFrontOf, and nothing more. It
// stays a named question because two call sites want only the boolean; it is
// not a second implementation of one.
//
// It fails CLOSED: an unreadable lane returns false, and the error alongside it.
// Callers must not read that false as "reachable, no blockers" — the point of
// returning both is that "I could not tell" and "nothing is in the way" are
// different answers and only one of them is safe to drive a robot on.
func IsSlotAccessible(db *sql.DB, slotNodeID int64) (bool, error) {
	blockers, err := BlockersInFrontOf(db, slotNodeID)
	if err != nil {
		return false, err
	}
	return len(blockers) == 0, nil
}

// FindStoreSlotInLane finds the deepest empty, UNRESERVED slot in a lane for
// back-to-front packing. Returns *Node, but the WHERE clause checks the bins,
// reservations, and orders tables — kept here because the return type is owned by
// nodes/.
//
// Deepest-UNRESERVED: the reservations NOT EXISTS makes a slot another order
// has soft-reserved (pending) or hard-claimed-and-confirmed invisible here, so two
// stores pack into distinct tiered slots. The bin-emptiness guard stays (a store
// wants a physically empty slot). The orders.delivery_node string-proxy STAYS too,
// NOT retired: the reservation read does NOT subsume it — simple store orders set
// delivery_node but do NOT reserve their slot (that's the #115/#117 gap deferred to
// the dispatch-path unification), so the proxy is still the only guard against a
// complex store picking a slot a simple store is heading to. (Equivalence check
// result: gap found → proxy kept. Retire it when simple-store reserves its slot.)
func FindStoreSlotInLane(db *sql.DB, laneID int64) (*Node, error) {
	return FindStoreSlotInLaneExcluding(db, laneID, 0)
}

// FindStoreSlotInLaneExcluding is FindStoreSlotInLane made OWNER-AWARE: every
// "someone else holds this" guard skips holds belonging to excludeOrderID, so an
// order that ALREADY holds a slot in this lane can re-resolve and see its own
// slot as available.
//
// This exists for re-binding at release time (the lane gate). The plain resolver
// cannot be reused there, and the reason is a trap worth stating: its guards are
// owner-BLIND. A gate-staged order holds its slot's claim, its reservation, AND
// has delivery_node pointing at it — so it matches all three NOT EXISTS clauses
// against ITSELF and its own slot is invisible. Re-resolving with the blind
// version therefore never returns the slot the order already holds; it returns
// the next-best one, which is SHALLOWER. Re-binding to that would silently undo
// back-to-front packing and break the deepest-first invariant the whole lane seam
// exists to enforce — while looking like it worked.
//
// Owner-aware, the same call is well behaved in all three cases: the order's own
// slot is still the best pick (the overwhelmingly common one — resolve returns it
// and the caller writes nothing), a shallower slot filled and walled it (the
// accessibility guard excludes it, so a reachable one is returned), or a deeper
// slot freed while it dwelled (the deeper one wins and the caller re-binds).
//
// excludeOrderID = 0 disables the exemption and reproduces the blind behavior
// exactly: order ids are positive, so `o.id <> 0` and `r.order_id <> 0` hold for
// every row, and `n.claimed_by = 0` never matches (claimed_by is NULL or a real
// id). That equivalence is what lets FindStoreSlotInLane delegate here unchanged.
func FindStoreSlotInLaneExcluding(db *sql.DB, laneID, excludeOrderID int64) (*Node, error) {
	n, err := findStoreSlot(db, laneID, excludeOrderID, true)
	if err == nil {
		return n, nil
	}
	// WHY THE MISS HAPPENED, asked only on the miss. The callers dispose of a
	// full lane and a claim-closed lane differently — a full lane is the routine
	// case and stays quiet, a closed one is rare, new, and the thing the floor
	// watches — and they cannot tell them apart from an empty result. Re-asking
	// with the burial clause OFF answers it exactly: if a slot appears without
	// the clause and not with it, the clause is the reason and nothing else is.
	//
	// It costs one extra query per MISS, never per hit, and misses are already
	// the path that walks to the next lane.
	if open, oErr := findStoreSlot(db, laneID, excludeOrderID, false); oErr == nil && open != nil {
		return nil, fmt.Errorf("no empty slot in lane %d: %w", laneID, ErrLaneClosedByClaim)
	}
	return nil, err
}

// ErrLaneClosedByClaim reports that a lane had a usable slot and the burial
// guard refused it — a bin a robot is already coming for sits deeper.
//
// It exists so a caller can tell "this lane is full" (routine, quiet) from "this
// lane is closed to stores right now" (rare, watchable, and self-clearing when
// the claim clears). Both are the same disposition — try the next lane — so the
// sentinel changes reporting, never control flow.
var ErrLaneClosedByClaim = errors.New("lane closed to stores: a claimed bin sits deeper")

// findStoreSlot is the selector body. `guard` toggles THE BURIAL CLAUSE, and the
// off form exists only to attribute a miss (see the caller); nothing in
// production takes a slot with the guard off.
//
// ── THE BURIAL GUARD — HARD CLAIMS ONLY ───────────────────────────────────
//
// The clause consults `bins.claimed_by` and NOTHING ELSE. A pending (soft)
// reservation deeper in the lane does NOT refuse a placement. That asymmetry is
// deliberate, and it is the whole design:
//
//   - A SOFT hold is a plan, and plans get recalculated. An order parked
//     pre-dispatch holding a bin that gets buried re-resolves on its next tick
//     and turns the burial into a dig (the held-bin path asks reachability and
//     a buried verdict becomes PlanBuriedReshuffle, 3326c1bb). The cure already
//     runs; the cost of the burial is time, not a fault.
//   - A HARD claim is a robot in motion. `claimed_by` is written at
//     ConfirmForDispatch, immediately before the fleet call, and cleared at
//     arrival, so it means "a robot is on its way to this bin, or holding it".
//     Burying that has no cure at all: the robot arrives at a slot it cannot
//     reach, and nothing re-plans a job the fleet already owns.
//
// The soft-inclusive form was worked out and rejected; the reasoning is in
// FINDINGS-claim-lifecycle-and-burial-guard-2026-08-09.md §4. Short version:
// soft holds have no time bound (reaping keys on the holder's liveness, never on
// age), so two cross-lane moves each parked on the other's soft hold refuse each
// other forever — both holders alive, both correct, and no janitor able to break
// it because neither claim is stale.
//
// ── WHY THIS FORM CANNOT CYCLE, stated here so nobody re-derives it ───────
//
// A deadlock needs a cycle of "X waits on Y". Every edge this clause can create
// runs from a REFUSED STORE to a HOLDER IN MOTION, and there is no edge back:
//
//   - The clause only ever refuses a STORE (this is the store-slot selector).
//   - It only respects holds of parties already moving: a hard claim lives from
//     confirm to pickup, which is a robot's drive time, and it is cleared by
//     arrival or by terminalization — neither of which can be blocked by a
//     parked store.
//   - A parked store holds nothing the holder needs. Its bin is at a press or a
//     line, its slot reservation is in a lane the holder is leaving, not
//     entering.
//
// So the wait-for graph is bipartite and one-directional: stores wait on movers,
// movers wait on nothing here. No cycle can close. The rejected soft form broke
// exactly this property, because a parked order's soft hold made a WAITER into a
// HOLDER and closed the loop.
//
// ── DIGS ARE STRUCTURALLY EXEMPT, and get no arm here ─────────────────────
//
// A reshuffle picks its shuffle slots through findShuffleSlots
// (dispatch/reshuffle.go), which does not call this selector at all. So the
// moves that exist to UNBURY things can never be refused by a burial guard —
// the deadlock a self-blind guard would cause is prevented by the call graph,
// not by an exemption, and an exemption added here would be dead code that
// looked load-bearing.
func findStoreSlot(db *sql.DB, laneID, excludeOrderID int64, guard bool) (*Node, error) {
	burial := "true"
	if guard {
		burial = fmt.Sprintf(`NOT EXISTS (
			SELECT 1 FROM nodes held
			JOIN bins held_bin ON held_bin.node_id = held.id
			WHERE held_bin.claimed_by IS NOT NULL
			  AND held_bin.claimed_by <> $2
			  AND %s
		  )`, helpers.ShallowerInSameLane("n", "held"))
	}
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id = $1
		  AND n.is_synthetic = false
		  AND (n.claimed_by IS NULL OR n.claimed_by = $2)
		  AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
		  AND NOT EXISTS (
			SELECT 1 FROM reservations r
			WHERE r.node_id = n.id
			  AND r.resource_kind = 'slot'
			  AND r.state IN ('pending','confirmed')
			  AND r.order_id <> $2
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM orders o
			WHERE o.delivery_node = n.name
			  AND o.status NOT IN (%s)
			  AND o.id <> $2
		  )
		  -- Accessibility guard: a slot is only a valid pick if no OCCUPIED slot
		  -- sits shallower in the same lane. The deepest-empty slot can otherwise
		  -- be stranded behind a shallow bubble (an occupied slot with empties
		  -- behind it), and a robot entering at the mouth could never reach it.
		  --
		  -- NOT owner-aware, unlike the three guards above it, and that asymmetry
		  -- is correct: occupancy is occupancy regardless of whose order put the
		  -- bin there. excludeOrderID exempts an order from its OWN holds; it can
		  -- never exempt it from a physical bin.
		  AND %s
		  -- The burial guard. Owner-aware through the SAME excludeOrderID
		  -- convention as the three clauses above: an order may always place
		  -- relative to its OWN claim, which is what lets the gate re-bind
		  -- re-resolve a slot behind a bin it is itself coming for.
		  AND %s
		ORDER BY COALESCE(n.depth, 0) DESC
		LIMIT 1`, SelectCols, FromClause, protocol.TerminalStatusSQLList(),
		helpers.ReachableSQL("n"), burial), laneID, excludeOrderID)
	n, err := ScanNode(row)
	if err != nil {
		return nil, fmt.Errorf("no empty slot in lane %d", laneID)
	}
	return n, nil
}

// LaneForNode returns the LANE node that directly parents nodeID, or (nil, nil)
// if nodeID is not a direct child slot of a lane. A one-hop parent walk (§8): a
// lane is modeled as a LANE-class node whose direct children are its
// depth-ordered slots, so a node's lane is its parent exactly when that parent is
// a LANE. Group-direct nodes, staging, and a lane node itself return (nil, nil) —
// they take no mouth exclusion. AuditLaneGeometry flags single-file geometry this
// walk cannot see.
func LaneForNode(db *sql.DB, nodeID int64) (*Node, error) {
	n, err := Get(db, nodeID)
	if err != nil {
		return nil, err
	}
	if n.ParentID == nil {
		return nil, nil
	}
	parent, err := Get(db, *n.ParentID)
	if err != nil {
		return nil, err
	}
	if parent.NodeTypeCode != protocol.NodeClassLANE {
		return nil, nil
	}
	return parent, nil
}

// AuditLaneGeometry returns human-readable warnings about single-file lane
// geometry that LaneForNode's one-hop parent walk cannot see, so a misconfigured
// scene is loud at startup rather than silently ungated (§8). Two smells:
//
//  1. A real (non-synthetic) node with a depth set whose parent is NOT a LANE —
//     it is tiered like a lane slot but hangs off a group directly, so it never
//     gets a mouth exclusion (the "NGRP-direct single-file" case).
//  2. A LANE nested directly under another LANE — the one-hop walk stops at the
//     inner lane and never reaches the outer, so the deeper slots stay ungated
//     (the "deep-nested" case).
//
// It is a diagnostic only: it changes nothing, and an empty result means the
// scene is fully walkable. Callers log each line at boot.
func AuditLaneGeometry(db *sql.DB) ([]string, error) {
	var warnings []string

	tiered, err := db.Query(`
		SELECT n.name, COALESCE(pnt.code, ''), pn.name
		FROM nodes n
		JOIN nodes pn ON pn.id = n.parent_id
		LEFT JOIN node_types pnt ON pnt.id = pn.node_type_id
		WHERE n.depth IS NOT NULL
		  AND n.is_synthetic = false
		  AND COALESCE(pnt.code, '') <> $1
		ORDER BY n.name`, protocol.NodeClassLANE)
	if err != nil {
		return nil, fmt.Errorf("audit lane geometry (tiered non-lane children): %w", err)
	}
	defer tiered.Close()
	for tiered.Next() {
		var name, parentClass, parentName string
		if err := tiered.Scan(&name, &parentClass, &parentName); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf(
			"node %q has a depth but its parent %q is %s, not a LANE — single-file geometry the mouth gate cannot see",
			name, parentName, classOrUntyped(parentClass)))
	}
	if err := tiered.Err(); err != nil {
		return nil, err
	}

	nested, err := db.Query(`
		SELECT n.name, pn.name
		FROM nodes n
		JOIN node_types nt ON nt.id = n.node_type_id
		JOIN nodes pn ON pn.id = n.parent_id
		JOIN node_types pnt ON pnt.id = pn.node_type_id
		WHERE nt.code = $1 AND pnt.code = $1
		ORDER BY n.name`, protocol.NodeClassLANE)
	if err != nil {
		return nil, fmt.Errorf("audit lane geometry (nested lanes): %w", err)
	}
	defer nested.Close()
	for nested.Next() {
		var name, parentName string
		if err := nested.Scan(&name, &parentName); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf(
			"lane %q is nested under lane %q — the one-hop walk cannot reach its slots",
			name, parentName))
	}
	return warnings, nested.Err()
}

// AuditLaneDepths returns startup warnings about lane children that HOLD A BIN
// but have no depth. Nothing in the runtime path forbids one: plantspec rejects
// Depth <= 0 for a spec'd slot (plantspec.validate), but ListLaneSlots filters
// nothing and a scene can be edited outside the spec.
//
// This exists because of the ruling in laneBlockerPredicate. A depth-less
// sibling is IGNORED — it is not a depth-ordered slot, so it cannot be in front
// of anything — which was the majority reading and is now the only one. That
// ruling is almost certainly right, and it is also unfalsifiable from inside the
// code: if the geometry never occurs, the ruling costs nothing and this audit
// never fires; if it does occur, a bin is sitting in a lane and NOTHING in the
// system will ever treat it as being in the way. The two spellings used to
// disagree about that case silently. Turning the silence into a boot warning is
// the cheap half of the fix.
//
// It filters neither is_synthetic nor enabled, deliberately: the reachability
// predicate filters neither, and this audit's whole job is to have exactly the
// same reach as the thing it is auditing.
//
// A diagnostic only — it changes nothing, and an empty result means no lane is
// holding inventory the reachability reader cannot see. Callers log each line at
// boot.
func AuditLaneDepths(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT n.name, ln.name, COUNT(b.id)
		FROM nodes n
		JOIN nodes ln ON ln.id = n.parent_id
		JOIN node_types lnt ON lnt.id = ln.node_type_id
		JOIN bins b ON b.node_id = n.id
		WHERE lnt.code = $1
		  AND n.depth IS NULL
		GROUP BY n.name, ln.name
		ORDER BY n.name`, protocol.NodeClassLANE)
	if err != nil {
		return nil, fmt.Errorf("audit lane depths: %w", err)
	}
	defer rows.Close()

	var warnings []string
	for rows.Next() {
		var name, laneName string
		var count int
		if err := rows.Scan(&name, &laneName, &count); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf(
			"node %q in lane %q holds %d bin(s) but has NO depth — reachability ignores depth-less siblings, "+
				"so nothing in that lane will ever treat those bins as being in the way",
			name, laneName, count))
	}
	return warnings, rows.Err()
}

func classOrUntyped(code string) string {
	if code == "" {
		return "untyped"
	}
	return code
}

// CountBinsInLane counts total bins across all slots in a lane.
// Lives here for convenience (single-table-coupled lane query) even though the
// COUNT runs over the bins table.
func CountBinsInLane(db *sql.DB, laneID int64) (int, error) {
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM bins b
		JOIN nodes slot ON slot.id = b.node_id
		WHERE slot.parent_id = $1
	`, laneID).Scan(&count)
	return count, err
}
