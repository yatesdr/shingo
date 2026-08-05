package nodes

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
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

// ── Reachability: one definition ──────────────────────────────────────────
//
// A slot is REACHABLE iff no occupied slot sits strictly shallower in the same
// lane. That one sentence used to be spelled seven different ways across the
// tree — a Go loop, a COUNT, four correlated sub-queries, and a sort key — and
// the spellings did not agree. Everything now routes through
// laneBlockerPredicate: ReachableSQL / BuriedSQL for the query sites whose
// ORDER BY is part of their semantics, BlockersInFrontOf for the Go sites.
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
//     disagreed, because GetSlotDepth below reports 0 for NULL and 0 is
//     shallower than everything — so a depth-less occupied child made every
//     slot in its lane reachable to the SQL side and blocked to the Go side.
//     Reachable was the ruling; AuditLaneDepths makes the geometry that
//     provokes it loud at boot rather than letting the two sides differ
//     quietly again.
//
// Occupancy is occupancy: the predicate consults `bins` and nothing else. A
// shallower slot that some other order has merely RESERVED or is inbound to
// does not make a deeper slot unreachable — that case belongs to the mouth gate
// and dispatch.classifyLaneEntry, and folding it in here would change admission
// behaviour plant-wide while looking like a tidy-up.

// laneBlockerPredicate is the definition: `blocker` is an occupied slot sitting
// strictly shallower than `target` in the same lane. Both arguments name
// `nodes` rows already in scope — a table alias, or a derived table with
// id/parent_id/depth (see BlockersInFrontOf). Returns a bare boolean expression
// so callers can put it wherever they need it.
func laneBlockerPredicate(blocker, target string) string {
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
			WHERE %s`, laneBlockerPredicate("lane_blocker", target))
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
		ORDER BY n.depth ASC`, SelectCols, FromClause, laneBlockerPredicate("n", "lane_target")), slotNodeID)
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
		  AND NOT EXISTS (
			-- Accessibility guard (mirrors IsSlotAccessible): a slot is only a
			-- valid pick if no OCCUPIED slot sits shallower in the same lane. The
			-- deepest-empty slot can otherwise be stranded behind a shallow bubble
			-- (an occupied slot with empties behind it), and a robot entering at
			-- the mouth could never reach it.
			SELECT 1 FROM nodes sib
			JOIN bins bb ON bb.node_id = sib.id
			WHERE sib.parent_id = n.parent_id
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(n.depth, 0) DESC
		LIMIT 1`, SelectCols, FromClause, protocol.TerminalStatusSQLList()), laneID, excludeOrderID)
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
