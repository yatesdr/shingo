package store

// Stage 2D delegate file: lane-scoped node queries live in store/nodes/.
// The bin-returning lane searches (FindSourceBinInLane, FindBuriedBin,
// FindOldestBuriedBin) stay here as cross-aggregate composition methods
// because their return type is *bins.Bin (bins aggregate) while the WHERE
// clause joins nodes via parent_id.

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth
// (ascending).
func (db *DB) ListLaneSlots(laneID int64) ([]*nodes.Node, error) {
	return nodes.ListLaneSlots(db.DB, laneID)
}

// LaneHeldLeg is one compound leg waiting on a lane, with the state the liveness
// floor compares against afterwards to tell whether its pass moved it.
type LaneHeldLeg struct {
	OrderID    int64
	LaneID     int64
	QueueCause string
	// State mirrors dispatch.waiterState's tuple — status, wait index, vendor
	// order — assembled in SQL so the floor's before-snapshot costs one query
	// rather than one per leg.
	State string
}

// ListLaneHeldLegs returns every compound leg not yet handed to the fleet whose
// source or delivery node is a lane slot, ACROSS ALL LANES.
//
// It is ListHeldLegParentsInLane turned inside out, and the two are deliberately
// kept as separate queries rather than one parameterised by lane. They answer
// different questions for different callers: the event path knows which lane
// just changed and wants the parents to re-drive, so it takes a lane and returns
// parents; the floor knows nothing and wants the whole waiting set WITH the
// per-leg state it will compare against, so it takes nothing and returns legs.
// Folding them would give the event path a snapshot it discards and the floor a
// lane it does not have.
//
// SAME POPULATION, THOUGH, and that is the part that must not drift: both use
// orders.AwaitingFleetSQL, so a leg visible to one is visible to the other. A
// floor that swept a different set from the one it re-drives would report
// releases nobody made and miss the ones it did.
func (db *DB) ListLaneHeldLegs() ([]LaneHeldLeg, error) {
	// The legs are selected FIRST and the node graph joined to that small set:
	// "a compound leg not yet with the fleet" is highly selective and the node
	// join is not, so filtering after would walk orders × slots.
	//
	// BOTH NAME FORMS, for the reason ListHeldLegParentsInLane states: node
	// references are bare ("SLN_002") or dotted ("LANE.SLN_002") depending on
	// which planner wrote them, and matching one form silently returns nothing
	// for half the plant. split_part(x, '.', 2) yields the slot half of a dotted
	// name and '' for a bare one, which matches no node.
	rows, err := db.DB.Query(fmt.Sprintf(`
		WITH legs AS (
			SELECT id, source_node, delivery_node, status, wait_index,
			       COALESCE(vendor_order_id, '') AS vendor_order_id,
			       COALESCE(queue_cause, '')     AS queue_cause
			FROM orders
			WHERE parent_order_id IS NOT NULL AND %s
		)
		SELECT DISTINCT legs.id, l.id, legs.queue_cause,
		       legs.status || '|' || legs.wait_index::text || '|' || legs.vendor_order_id
		FROM legs
		JOIN nodes n ON n.name IN (
			legs.source_node, legs.delivery_node,
			split_part(legs.source_node, '.', 2), split_part(legs.delivery_node, '.', 2)
		)
		JOIN nodes l ON l.id = n.parent_id
		JOIN node_types lt ON lt.id = l.node_type_id AND lt.code = 'LANE'`,
		orders.AwaitingFleetSQL("")))
	if err != nil {
		return nil, fmt.Errorf("list lane-held legs: %w", err)
	}
	defer rows.Close()

	var out []LaneHeldLeg
	for rows.Next() {
		var h LaneHeldLeg
		if err := rows.Scan(&h.OrderID, &h.LaneID, &h.QueueCause, &h.State); err != nil {
			return nil, fmt.Errorf("scan lane-held leg: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListHeldLegParentsInLane returns the distinct parents of compound legs that
// have not reached the fleet and whose source or delivery node is a slot of this
// lane.
//
// It exists to answer "who is waiting on this lane" for a leg that has no other
// witness. A leg held at its lane writes no status — it stays `pending` — so it
// is invisible to every query that looks for work in progress, and the only
// releaser its own dispatch path names is a SIBLING's dropoff completion. With no
// sibling running there is nothing to come back, which is the wedge. Keying on
// the lane instead means the event that actually frees the lane can find it,
// whoever caused it.
//
// THE POPULATION IS orders.AwaitingFleetSQL, NOT `status='pending'`, and the
// difference is a second leg with no witness: one whose fleet create was refused
// is rolled back to `sourcing` and parked, holding no vendor order. It is
// waiting on this lane in exactly the same sense and was invisible here for
// exactly the same reason. One spelling, shared with GetNextChild — see that
// function for why vendor_order_id is the authority and a status was the proxy.
//
// BOTH NAME FORMS ARE MATCHED. orders.source_node / delivery_node hold whatever
// the planner wrote, and node references are bare ("SLN_002") or dotted
// ("LANE.SLN_002") depending on the caller — GetByDotName splits on the dot and
// falls back to a bare lookup, so both are live. Matching only one form would
// silently return no parents for half the plant, which is the failure mode this
// query exists to prevent and would be invisible in exactly the same way.
func (db *DB) ListHeldLegParentsInLane(laneID int64) ([]int64, error) {
	rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT DISTINCT o.parent_order_id
		FROM orders o
		JOIN nodes n ON n.parent_id = $1
		LEFT JOIN nodes p ON p.id = n.parent_id
		WHERE o.parent_order_id IS NOT NULL
		  AND %s
		  AND (
		        o.source_node   = n.name
		     OR o.delivery_node = n.name
		     OR o.source_node   = p.name || '.' || n.name
		     OR o.delivery_node = p.name || '.' || n.name
		  )`, orders.AwaitingFleetSQL("o")), laneID)
	if err != nil {
		return nil, fmt.Errorf("list held-leg parents in lane %d: %w", laneID, err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan held-leg parent: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetSlotDepth returns the depth for a node, or 0 if not set.
func (db *DB) GetSlotDepth(nodeID int64) (int, error) {
	return nodes.GetSlotDepth(db.DB, nodeID)
}

// IsSlotAccessible returns true if no occupied slots exist at a shallower
// depth in the same lane.
func (db *DB) IsSlotAccessible(slotNodeID int64) (bool, error) {
	return nodes.IsSlotAccessible(db.DB, slotNodeID)
}

// BlockersInFrontOf returns the occupied slots shallower than slotNodeID in its
// lane, shallowest first — the set IsSlotAccessible reports the emptiness of.
// See nodes.BlockersInFrontOf for the single definition of "in front of" and
// for why an error here must be read as blocked, not as reachable.
func (db *DB) BlockersInFrontOf(slotNodeID int64) ([]*nodes.Node, error) {
	return nodes.BlockersInFrontOf(db.DB, slotNodeID)
}

// LaneForNode returns the LANE node that directly parents nodeID, or nil if
// nodeID is not a direct child slot of a lane (the mouth-gate lane resolution).
func (db *DB) LaneForNode(nodeID int64) (*nodes.Node, error) {
	return nodes.LaneForNode(db.DB, nodeID)
}

// AuditLaneGeometry returns startup warnings about single-file lane geometry the
// one-hop parent walk cannot see (§8).
func (db *DB) AuditLaneGeometry() ([]string, error) {
	return nodes.AuditLaneGeometry(db.DB)
}

// AuditLaneDepths returns startup warnings about lane children that hold a bin
// but have no depth — inventory the reachability predicate ignores by design.
// See nodes.AuditLaneDepths for why the ruling needs an audit rather than a
// guard.
func (db *DB) AuditLaneDepths() ([]string, error) {
	return nodes.AuditLaneDepths(db.DB)
}

// NodeStyleOrigins returns the (process, style) pairs that claim a node in the
// plant-claims mirror (style_claims), as canonical "process|style" strings. Empty
// when the node has no style claim — loaders/unloaders are structurally excluded
// from the mirror, so they resolve to no origin. This backs the tiered-entry
// same-origin classifier (two orders share an origin iff their demand nodes'
// pair sets are equal and non-empty).
func (db *DB) NodeStyleOrigins(nodeName string) ([]string, error) {
	rows, err := db.DB.Query(
		`SELECT DISTINCT process_id, style_id FROM style_claims WHERE core_node_name = $1`, nodeName)
	if err != nil {
		return nil, fmt.Errorf("node style origins %s: %w", nodeName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			return nil, fmt.Errorf("node style origins %s scan: %w", nodeName, err)
		}
		out = append(out, p+"|"+s)
	}
	return out, rows.Err()
}

// ListChildNodesUnlocked is ListChildNodes with dig-held lanes filtered OUT in
// the query — the candidate read the group resolver scans.
//
// It replaces five copies of
//
//	for _, child := range children {
//	    if r.LaneLock != nil && r.LaneLock.IsLocked(child.ID) { continue }
//
// and it is not an optimisation of that check, it is its deletion. The filter
// rides a query that was already happening, so it costs no extra round trip, and
// because the candidate set and the lock state come out of ONE statement there
// is no gap between fetching a lane and deciding whether it is free.
//
// The alternatives were considered and rejected. A per-scan snapshot of the dig
// holds is wrong in the direction that matters — a dig starting mid-scan stays
// invisible for the rest of it, and on a `none`-algorithm group nothing
// downstream catches the stale answer. A memory cache is a permanent obligation
// bought for an unmeasured gain on a path already doing database work.
//
// The lesson this encodes is from the bug that motivated it: the dig row dying
// at leg one was never a problem of two COPIES of a fact, it was two WRITERS for
// one fact. Judge any future shape here on that first — one writer, one reader,
// one statement.
//
// Lives at the outer store/ level because it joins nodes to reservations, and
// cross-aggregate composition is exactly what store/<sub> may not do.
//
// ── THE ASKER IS NOT OPTIONAL ─────────────────────────────────────────────
//
// This filter used to exempt nobody, and that was its one defect. It arrived as
// a consolidation of five copies of `if LaneLock.IsLocked(child) { continue }`,
// which was right about the duplication and wrong about the semantics: IsLocked
// has no asker, so consolidating onto it made "dig-held" mean "excluded from
// everyone" — INCLUDING the order the dig was run for.
//
// That is not hypothetical. In expose mode the lock is transferred to the
// complex parent to protect the bin the dig uncovered; the parent then resumes
// and re-resolves through this very query, which dropped the lane because of
// the parent's own lock. The parent could not see the bin its own dig exposed,
// re-resolved to the next buried one, and dug again. See
// store/reservations/dig_exclusion.go for the full account.
//
// Pass reservations.Anyone when there is genuinely no order behind the scan —
// it reproduces the old owner-blind behaviour exactly, because order ids are
// never zero.
func (db *DB) ListChildNodesUnlocked(parentID int64, asker reservations.DigAsker) ([]*nodes.Node, error) {
	args := append([]any{parentID, string(reservations.ModeDig)}, asker.Args()...)
	rows, err := db.DB.Query(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM reservations dig_hold
			WHERE dig_hold.resource_kind = 'mouth'
			  AND dig_hold.node_id = n.id
			  AND `+reservations.ActiveStateSQL("dig_hold.")+`
			  AND dig_hold.mode = $2
			  AND %s
		  )
		ORDER BY n.name`, nodes.SelectCols, nodes.FromClause,
		reservations.DigExclusionSQL("dig_hold.order_id", 3, 4)), args...)
	if err != nil {
		return nil, fmt.Errorf("list unlocked children of %d: %w", parentID, err)
	}
	defer rows.Close()
	return nodes.ScanNodes(rows)
}

// LanesOccupiedInGroup returns the lanes of a group that a robot is currently
// inside — Hold B, the presence row, not the dig claim.
//
// ── WHY PARKING HAS TO ASK THIS ───────────────────────────────────────────
//
// A single-file lane takes one robot at a time, and admission enforces that: a
// destination in an occupied lane is refused with CauseLaneOccupied. What was
// missing is that the CANDIDATE LIST did not know, so the release-time resolver
// offered a slot in a lane somebody was already driving into, got refused, and
// walked on — to the next candidate, which is the next slot SHALLOWER IN THE SAME
// LANE. Placing there buries the deeper slot it just skipped.
//
// That is how LS_C4 came out of the closing run as X X . X: a four-slot empty
// lane, filled by two digs taking turns, with a hole at depth 3 that nothing can
// reach. The lane was the group's entire spare capacity.
//
// So an occupied lane leaves the pool at the source. The resolver moves to a
// DIFFERENT lane rather than to a worse slot in this one, which is what "one
// robot per lane" was supposed to mean all along.
//
// NOT FOLDED INTO ListChildNodesUnlocked, deliberately: that read is shared with
// SOURCING, where a robot being inside a lane is not a reason to refuse to take a
// bin out of it. Presence excludes a lane as a PLACE TO PUT SOMETHING, which is a
// narrower question than that query answers.
func (db *DB) LanesOccupiedInGroup(groupID int64) (map[int64]bool, error) {
	rows, err := db.DB.Query(`
		SELECT DISTINCT r.node_id
		FROM reservations r
		JOIN nodes lane ON lane.id = r.node_id
		WHERE r.resource_kind = `+reservations.OccupancyKindSQL()+`
		  AND `+reservations.ActiveStateSQL("r.")+`
		  AND lane.parent_id = $1`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list occupied lanes in group %d: %w", groupID, err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan occupied lane in group %d: %w", groupID, err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ListOutstandingDigClaims names the digs in a group that still owe a blocker a
// slot: a lane held under a dig claim with at least one non-terminal leg carrying
// no destination.
//
// THE ADMISSION LEDGER THAT USED TO COUNT THESE IS GONE (§R.79). It required a
// new dig to fit on top of the room already owed, which was the wrong unit and
// the wrong idea — see planUnbury. The remaining reader is the standoff tripwire,
// which needs the IDENTITIES: an edge in the holder-of-holder graph only means
// something for a dig that is actually waiting for parking, and a dig owing
// nothing cannot be a link in a cycle whatever else it is doing.
func (db *DB) ListOutstandingDigClaims(groupID int64, asker reservations.DigAsker) ([]int64, error) {
	args := append([]any{string(reservations.ModeDig), groupID}, asker.Args()...)
	rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT DISTINCT dig_hold.order_id
		FROM reservations dig_hold
		JOIN nodes lane ON lane.id = dig_hold.node_id
		WHERE dig_hold.resource_kind = 'mouth'
		  AND dig_hold.mode = $1
		  AND `+reservations.ActiveStateSQL("dig_hold.")+`
		  AND lane.parent_id = $2
		  AND %s
		  AND EXISTS (
			SELECT 1 FROM orders leg
			WHERE leg.parent_order_id = dig_hold.order_id
			  AND leg.status NOT IN (%s)
			  AND COALESCE(leg.delivery_node, '') = ''
		  )`,
		reservations.DigExclusionSQL("dig_hold.order_id", 3, 4),
		protocol.TerminalStatusSQLList()), args...)
	if err != nil {
		return nil, fmt.Errorf("list outstanding dig claims in group %d: %w", groupID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list outstanding dig claims in group %d scan: %w", groupID, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LaneAcceptsInbound reports whether a lane currently has no mouth hold that
// would block an inbound (store) share. It is compatible when every active mouth
// row is inbound — same-mode sharing is legal (§2) — and incompatible when any
// row is outbound or a dig. An empty lane is compatible. This mirrors the mouth
// gate's own admit rule (reservations/mouth.go admitMouth) for the inbound case.
//
// This is the read behind resolve-around: the store finder prefers a lane whose
// mouth is currently free so the order need not stall there. It is advisory only
// — a hint for ranking, taken without the lane's advisory lock; the mouth gate
// still arbitrates the actual admission, so a race here only costs one less-ideal
// lane choice, never a correctness violation.
func (db *DB) LaneAcceptsInbound(laneID int64) (bool, error) {
	holds, err := reservations.ActiveMouthRows(db.DB, laneID)
	if err != nil {
		return false, err
	}
	for _, h := range holds {
		if h.Mode != reservations.ModeInbound {
			return false, nil
		}
	}
	return true, nil
}

// FindSourceBinInLane finds the shallowest accessible unclaimed bin in a
// lane matching the given payload code. Cross-aggregate composition
// (bins ↔ nodes).
//
// The is_synthetic = false guard is defense-in-depth: today _TRANSIT
// has no parent_id so it can't be a lane child, but if a future
// migration adds a synthetic ghost-slot under a lane, this filter
// prevents the lane reader from claiming an in-flight bin.
func (db *DB) FindSourceBinInLane(laneID int64, payloadCode string) (*bins.Bin, error) {
	query := fmt.Sprintf(`%s
		WHERE b.node_id IN (SELECT id FROM nodes WHERE parent_id = $1)
		  AND COALESCE(n.is_synthetic, false) = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND `+bins.SourceableStatusSQL+`
		  AND b.status <> 'staged'
		  AND ($2 = '' OR b.payload_code = $2)
		  AND NOT `+reservations.BinSpokenForSQL+`
		  AND %s
		ORDER BY COALESCE(n.depth, 0) ASC
		LIMIT 1`, bins.BinJoinQuery, helpers.ReachableSQL("n"))
	row := db.QueryRow(query, laneID, payloadCode)
	bin, err := bins.ScanBin(row)
	if err != nil {
		return nil, fmt.Errorf("no accessible bin in lane %d", laneID)
	}
	return bin, nil
}

// FindStoreSlotInLane finds the deepest empty slot in a lane for
// back-to-front packing.
func (db *DB) FindStoreSlotInLane(laneID int64) (*nodes.Node, error) {
	return nodes.FindStoreSlotInLane(db.DB, laneID)
}

// FindStoreSlotInLaneExcluding is FindStoreSlotInLane with excludeOrderID's own
// holds ignored — the owner-aware form the lane gate re-binds through, so a
// staged order can re-resolve and still see the slot it already holds. See
// nodes.FindStoreSlotInLaneExcluding for why the blind form is unusable there.
func (db *DB) FindStoreSlotInLaneExcluding(laneID, excludeOrderID int64) (*nodes.Node, error) {
	return nodes.FindStoreSlotInLaneExcluding(db.DB, laneID, excludeOrderID)
}

// CountBinsInLane counts total bins across all slots in a lane.
func (db *DB) CountBinsInLane(laneID int64) (int, error) {
	return nodes.CountBinsInLane(db.DB, laneID)
}

// FindOldestBuriedBin finds the oldest buried bin in a lane by
// loaded_at/created_at timestamp. Unlike FindBuriedBin (which returns the
// shallowest buried bin for cheapest reshuffle), this returns the oldest
// buried bin for strict FIFO correctness. Cross-aggregate composition.
func (db *DB) FindOldestBuriedBin(laneID int64, payloadCode string) (*bins.Bin, *nodes.Node, error) {
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.node_id IN (SELECT id FROM nodes WHERE parent_id = $1)
		  AND COALESCE(n.is_synthetic, false) = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND `+bins.SourceableStatusSQL+`
		  AND b.status <> 'staged'
		  AND ($2 = '' OR b.payload_code = $2)
		  AND %s
		ORDER BY COALESCE(b.loaded_at, b.created_at) ASC
		LIMIT 1`, bins.BinJoinQuery, helpers.BuriedSQL("n")), laneID, payloadCode)
	bin, err := bins.ScanBin(row)
	if err != nil {
		return nil, nil, fmt.Errorf("no buried bin in lane %d", laneID)
	}
	slot, err := nodes.Get(db.DB, *bin.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return bin, slot, nil
}

// FindBuriedBin finds a bin that exists in a lane but is blocked by
// shallower bins. Cross-aggregate composition (bins ↔ nodes).
func (db *DB) FindBuriedBin(laneID int64, payloadCode string) (*bins.Bin, *nodes.Node, error) {
	row := db.QueryRow(fmt.Sprintf(`%s
		WHERE b.node_id IN (SELECT id FROM nodes WHERE parent_id = $1)
		  AND COALESCE(n.is_synthetic, false) = false
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.manifest_confirmed = true
		  AND `+bins.SourceableStatusSQL+`
		  AND b.status <> 'staged'
		  AND ($2 = '' OR b.payload_code = $2)
		  AND %s
		ORDER BY COALESCE(n.depth, 0) ASC
		LIMIT 1`, bins.BinJoinQuery, helpers.BuriedSQL("n")), laneID, payloadCode)
	bin, err := bins.ScanBin(row)
	if err != nil {
		return nil, nil, fmt.Errorf("no buried bin in lane %d", laneID)
	}
	slot, err := nodes.Get(db.DB, *bin.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return bin, slot, nil
}

// SpokenForBin is one bin in a lane that some live order has a hold on -- the
// shadow instrument's row (service/burial_shadow.go). It is a READ for
// measurement only.
type SpokenForBin struct {
	BinID    int64
	BinLabel string
	SlotName string
	Depth    int
	// HolderID is the live order holding the bin, resolved claim-first: a hard
	// claim names its owner directly, and a bin with only a soft reservation is
	// held by whoever reserved it.
	HolderID     int64
	HolderStatus string
	// HolderIsChild says the holder is a compound leg. It keys on the ORDER SHAPE
	// rather than on the absence of a reservation row deliberately: a leg is a leg
	// whether or not it has a ledger row.
	HolderIsChild bool
	// HardClaim is bins.claimed_by. Its absence with a live reservation present is
	// the SOFT hold -- a plan, not a robot, and the class the burial guard
	// deliberately does not respect.
	HardClaim bool
	// HeldSince is when the hold started: the reservation row's created_at, or the
	// holder order's own created_at for a compound leg whose claim is stamped in
	// the same transaction that inserts it (store/orders.go CreateCompoundChildren)
	// and so shares its timestamp exactly.
	HeldSince time.Time
	// HeldFor is that hold's AGE, computed IN THE DATABASE (§R.98 stage D).
	//
	// It is not derivable in Go from HeldSince, and the burial shadow spent a
	// soak proving it: HeldSince comes from a DB-default `created_at` (wall) and
	// the caller subtracted it from `clock.Now()` (sim). Under the honest running
	// clock, sim runs a year ahead of wall, so an eight-second hold rendered as
	// `held_for=7355h32m48s` in the one line an engineer reads to judge whether a
	// burial mattered.
	//
	// The house rule is compare a column against the clock that WROTE it. Both
	// ends of this subtraction are now the database's, so the domains cannot be
	// crossed by a caller who does not know which clock stamped what — which is
	// the only durable form of the fix, since HeldSince's two sources are both
	// DB-stamped and every Go caller's `now` is not.
	//
	// The old code clamped a negative result and blamed "clock skew". That
	// clamp was the same defect wearing a guard: it caught the sign and not the
	// magnitude, so the direction that actually occurred sailed through.
	HeldFor time.Duration
}

// SpokenForBinsBehind returns every live-held bin sitting DEEPER in its lane than
// the slot `placedNodeID` -- the bins a placement there buries.
//
// ONE DEFINITION OF IN-FRONT-OF. The geometry comes from
// helpers.ShallowerInSameLane, the same expression the burial guard's clause
// composes (store/nodes/lanes.go findStoreSlot) and the same one reachability
// composes. The guard refuses on the hard-claim subset, this observes the whole
// set; spelling the depth relation twice is the drift
// TestReachabilityHasExactlyOneSpelling exists to catch, and it does.
//
// LIVE HOLDER ONLY. A hold whose order is terminal is not a hold -- the terminal
// chokepoint releases it in the same transaction as the status write. Filtering
// here means the instrument never reports a burial of a bin nobody is waiting for.
func (db *DB) SpokenForBinsBehind(placedNodeID int64) ([]SpokenForBin, error) {
	rows, err := db.Query(fmt.Sprintf(`
		WITH placed AS (SELECT id, parent_id, depth FROM nodes WHERE id = $1)
		SELECT b.id, b.label, held.name, held.depth,
		       (b.claimed_by IS NOT NULL) AS hard_claim,
		       o.id, o.status, (o.parent_order_id IS NOT NULL) AS is_child,
		       COALESCE(r.created_at, o.created_at) AS held_since,
		       EXTRACT(EPOCH FROM now() - COALESCE(r.created_at, o.created_at)) AS held_secs
		FROM nodes held
		CROSS JOIN placed
		JOIN bins b ON b.node_id = held.id AND b.status <> 'retired'
		LEFT JOIN reservations r
		       ON r.bin_id = b.id AND r.resource_kind = 'bin'
		      AND `+reservations.ActiveStateSQL("r.")+`
		JOIN orders o ON o.id = COALESCE(b.claimed_by, r.order_id)
		WHERE %s
		  AND COALESCE(held.is_synthetic, false) = false
		  AND (b.claimed_by IS NOT NULL OR r.order_id IS NOT NULL)
		  AND o.status NOT IN (%s)
		ORDER BY held.depth ASC`,
		helpers.ShallowerInSameLane("placed", "held"), protocol.TerminalStatusSQLList()), placedNodeID)
	if err != nil {
		return nil, fmt.Errorf("spoken-for bins behind node %d: %w", placedNodeID, err)
	}
	defer rows.Close()
	var out []SpokenForBin
	for rows.Next() {
		var s SpokenForBin
		var heldSecs float64
		if err := rows.Scan(&s.BinID, &s.BinLabel, &s.SlotName, &s.Depth,
			&s.HardClaim, &s.HolderID, &s.HolderStatus, &s.HolderIsChild, &s.HeldSince, &heldSecs); err != nil {
			return nil, fmt.Errorf("spoken-for bins scan: %w", err)
		}
		if heldSecs < 0 {
			heldSecs = 0 // a hold stamped in the future is not a negative age
		}
		s.HeldFor = time.Duration(heldSecs * float64(time.Second))
		out = append(out, s)
	}
	return out, rows.Err()
}

// OrderIsCompoundLeg reports whether an order is a compound (dig) child. The
// burial instrument asks it about the PLACING order: a dig leg picks its
// destination through findShuffleSlots, which does not consult the store-slot
// selector, so a dig burying a claimed bin is a known uncovered path rather than
// a guard bypass.
// OrderCommittedToFleetAt returns the last moment this order's destination was
// committed and the robot was moving toward it, from order_history.
//
// ── WHAT IT IS FOR, AND WHY THESE TWO STATUSES ────────────────────────────
//
// The burial tripwire needs to tell two populations apart: a placement that
// SKIPPED the store-slot selector (must be zero) from one the selector APPROVED
// and a later claim invalidated (churn the design accepts and heals — PLAN §R.4).
// The discriminator is time: a claim created after this order's destination was
// committed could not have been visible to the selector, and one created before
// it could.
//
// `in_transit` is the moment for a GATED order — its destination is chosen at
// release by rebindGatedDropoff, and staged→in_transit is that release.
// `dispatched` is the moment for the plain path, where a plan can go straight to
// delivered without ever reporting in_transit. MAX over the two is right for
// both: for a gated order in_transit(release) is strictly later than
// dispatched(create), and for a plain order in_transit follows dispatch.
//
// IT ERRS LATE. On the plain path the selector actually runs during sourcing,
// before dispatch, so a claim landing in the window between the two is
// classified as never-asked — the LOUD bucket. A tripwire that under-reports is
// worse than one that over-reports, which is the same rule this file's
// compound-leg lookup already follows.
//
// ── THAT TRADE-OFF WAS PRICED WRONG, AND THE BILL CAME IN ─────────────────
//
// The paragraph above used to end "AND THE DIRECTION IS DELIBERATE", on the
// reasoning that a false accusation is the safe way to be wrong. It is not, for
// THIS counter: Bypass is the instrument's only should-be-zero, and a
// should-be-zero that fires on a race nobody can fix is the cry-wolf failure the
// burial shadow's own header forbids in the paragraph about the §R.4 split.
//
// Measured on the lane-stress rig 2026-08-15. Order 53 resolved onto LSC_032 at
// 03:46:54.344 and reached here with a commit time of 03:46:54.385. The claim it
// was accused of ignoring belonged to order 54 — which did not exist until
// 03:46:54.475. The window is not always 41ms either: an order whose group is
// full at intake queues behind capacity and commits MINUTES after its
// destination was chosen (see resolveSyntheticDestination's capacity arm).
//
// So the caller now prefers OrderDestinationResolvedAt and falls back to this.
// This function is unchanged and still correct for what it measures — it was
// never the wrong answer, it was the wrong QUESTION for orders that have a
// better one available.
//
// ok=false means the order has no such history row and the caller cannot make
// the comparison; it must then take the loud arm rather than guess.
func (db *DB) OrderCommittedToFleetAt(orderID int64) (time.Time, bool, error) {
	var at sql.NullTime
	err := db.QueryRow(`SELECT MAX(created_at) FROM order_history
		WHERE order_id=$1 AND status IN ('dispatched','in_transit')`, orderID).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("order %d fleet-commit time: %w", orderID, err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return at.Time.UTC(), true, nil
}

// StampDestinationResolved records the instant the store-slot selector chose
// `orderID`'s destination at intake.
//
// WRITTEN ONCE, BY ONE CALLER (dispatch.admitOrder), and only when
// resolveSyntheticDestination actually rewrote a group into a concrete slot. The
// value is the selector's OWN clock reading, taken at the resolve and carried
// forward — not a second reading taken here, which would fold the CreateOrder
// round-trip into the number the tripwire compares against.
//
// COALESCE, NOT A BARE SET. A re-admitted order keeps its FIRST stamp: the
// question this answers is "when did a selector last look and approve", and on
// the intake path there is exactly one such moment per order. An overwrite could
// only come from a retry re-running admitOrder, which would move the timestamp
// forward past claims the original resolve genuinely did see — turning a real
// bypass quiet. Keeping the earliest keeps the loud direction.
func (db *DB) StampDestinationResolved(orderID int64, at time.Time) error {
	_, err := db.Exec(
		`UPDATE orders SET destination_resolved_at = COALESCE(destination_resolved_at, $2) WHERE id = $1`,
		orderID, at.UTC())
	if err != nil {
		return fmt.Errorf("order %d stamp destination-resolved: %w", orderID, err)
	}
	return nil
}

// OrderDestinationResolvedAt reports when intake's selector chose this order's
// destination, and whether it did at all.
//
// ok=false is NOT a failure and NOT a doubt — it is the ordinary answer for
// every order whose destination was not chosen at intake: a sender that named a
// concrete node, and a group that was full so planMove resolves at dispatch. For
// those the fleet-commit time is the right comparison and the caller falls back
// to it, which is the behaviour that predates this column.
//
// The distinction the caller must preserve: ok=false means "ask the other
// question", while an ERROR means "could not ask" and takes the loud arm.
func (db *DB) OrderDestinationResolvedAt(orderID int64) (time.Time, bool, error) {
	var at sql.NullTime
	err := db.QueryRow(
		`SELECT destination_resolved_at FROM orders WHERE id = $1`, orderID).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("order %d destination-resolved time: %w", orderID, err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return at.Time.UTC(), true, nil
}

func (db *DB) OrderIsCompoundLeg(orderID int64) (bool, error) {
	var isChild bool
	err := db.QueryRow(`SELECT parent_order_id IS NOT NULL FROM orders WHERE id=$1`, orderID).Scan(&isChild)
	if err != nil {
		return false, fmt.Errorf("order %d compound-leg check: %w", orderID, err)
	}
	return isChild, nil
}

// SlotsBlockedByHardClaims returns the ids of slots in `groupID` that sit
// STRICTLY IN FRONT of a bin some order has hard-claimed — the slots a dig may
// not park a blocker in, because doing so would wall a bin a robot is already on
// its way to.
//
// ── ONE SPELLING OF THE GEOMETRY; A NARROWER QUESTION ON TOP OF IT ────────
//
// The GEOMETRY is shared with the store selector's burial clause
// (nodes/lanes.go findStoreSlot) through helpers.ShallowerInSameLane, so "in
// front of" cannot come to mean two things — TestBurialExclusionOneSpelling
// refuses that drift.
//
// The PREDICATE is not shared, and the A batch's plan assumed it would be. Re-key
// the dig side onto "the hard-claim predicate the store side already uses" turned
// out to be wrong twice, and both times an existing test said so rather than a
// review catching it:
//
//  1. ANY claimed bin — TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace failed. A
//     lane whose own dig is RUNNING has a claimed target, and excluding every
//     slot in front of it removes that lane from the shuffle pool for the whole
//     excavation, starving a dig that had somewhere to go.
//  2. Any claimed AND REACHABLE bin — the same test failed again, for a sharper
//     reason: dig B's claim is on the BLOCKER it is about to carry OUT, which is
//     reachable and claimed and yet completely safe to park in front of.
//
// The distinction the bridge actually carried is COLLECTOR versus MOVER. A
// pending_lane_extensions row existed only for a bin a FINISHED dig had uncovered
// so its parent could come back and COLLECT IT WHERE IT LAY. A dig leg's claim
// means the opposite: that bin is leaving. Burying something on its way out is
// not burying it.
//
// `holder.parent_order_id IS NULL` is that distinction: a top-level order claims
// a bin to collect it, a compound child claims one to move it. Reachability
// stays as the second term, because a collector's bin that is itself still buried
// needs no protection — it is already behind something.
//
// The store side is deliberately WIDER (any claim, minus the asker): a store has
// no business parking in front of anything spoken for, and it is not the one
// doing the unburying. Two callers, one geometry, two policies — stated here
// because the plan expected one policy and the tests proved otherwise.
//
// IT REPLACED A DIFFERENT FACT, and the difference is why this is a re-key rather
// than a deletion. The dig side used to read pending_lane_extensions, the expose
// bridge's table: a dig that had finished in expose mode left a row naming the bin
// it had just uncovered, and that row was what protected it. The bridge is gone —
// its parenthesis used to read "the demand is no longer re-parented into its own
// dig, so nothing is left half-finished for a parent to come back to", which
// §R.91 has made false; what replaced the bridge is the self-handoff, which
// keeps the corridor as the parent's own outbound hold rather than parking the
// fact in a table. The FACT it protected is unchanged either way and is carried
// by claims: a bin somebody is coming for must not be walled in. F-19 is that scenario, and it still passes — the guard changed
// spelling, not meaning.
//
// ── THE CLAIM IS NOT THE ONLY SPELLING OF "COMING FOR IT" ─────────────────
//
// bin_id is the other one, and reading only claimed_by is how this guard went
// blind on the lane-stress rig 2026-08-13.
//
// The specimen, from the rig's own rows. Dig 8 was raised for order 1 and parked
// its first blocker — bin 6 — at LSD_010, the back of LS_D2. A blocker in a
// parking slot is an ordinary reachable bin of an ordinary style, and it was the
// most reachable one of its style in the group, so order 1 RESOLVED ONTO IT:
// bin_id 6, source_node LSD_010. The dig's next leg then parked bin 8 at LSD_007,
// two slots in front of it, and walled in the bin its own demand was now coming
// for. Order 1 has sat `staged` under lane-target-buried ever since.
//
// This guard was asked and answered NOTHING. Bin 6's claimed_by was NULL,
// because a claim is taken and released around each movement while bin_id is the
// resolve's durable record of which bin this order is for — and the burial
// happened in exactly the interval between two claims. Asked with bin_id, the
// same query returns LSD_006 through LSD_009: the four slots in front of it, one
// of which is the slot the burial used.
//
// IT IS PAIRED WITH source_node, WHICH IS WHAT MAKES IT A DIRECTION. bin_id
// alone says "this order's bin", not "this order is coming for it" — a store's
// bin_id goes on pointing at its bin after it has PLACED it, and a bare aim term
// therefore lets an order close a lane behind itself. An order coming to collect
// names the slot the bin is standing in; one that has delivered names where it
// came from. Together the two columns say the thing this guard needs and neither
// says alone.
//
// THE TERMINAL FILTER IS NOT OPTIONAL WITH THIS TERM. claimed_by is reaped when
// an order terminalizes; bin_id is not, and a finished order's bin_id would
// otherwise protect a bin forever. It is written as a status filter on the holder
// rather than as a second lookup so both spellings pass the same liveness test.
//
// THE ASKER IS NOT EXEMPT HERE, and that is one deliberate difference from the
// store side, which excludes the requesting order. A dig asking "where may I park
// this blocker" must not be allowed to bury a bin ITS OWN compound claimed a
// moment ago — the claims inside a dig are exactly the bins it is moving. Handing
// this caller the store's exemption would give it to the one caller it breaks.
//
// ── ONLY A REACHABLE BIN IS PROTECTED, WHICH IS THE OTHER DIFFERENCE ──────
//
// A claimed bin that is ITSELF still buried needs no protection: it is already
// behind something, so parking in front of it changes nothing about whether its
// robot can get to it. Protecting it anyway is not merely redundant, it starves
// the plant — a lane whose own dig is still RUNNING has a claimed, buried target,
// and excluding every slot in front of it removes that lane from the shuffle pool
// for the whole excavation. TestCrossFlow_TwoDigsOneLane_BsLegWinsTheRace is that
// case exactly, and it caught this: the first version of this query protected any
// claimed bin, and dig A was refused with nowhere to park while lane B's mouth
// slot sat free and reachable.
//
// So the term is helpers.ReachableSQL, which is the same "is anything occupied in
// front of it" predicate the rest of the system asks. That also makes the
// re-key faithful to what the deleted bridge actually protected: a
// pending_lane_extensions row existed only for a bin a FINISHED dig had already
// UNCOVERED — i.e. a reachable one — never for a target still under a wall.
// LiveServiceDigInEpisode returns the id of a service dig already running for
// this episode, or 0 when there is none.
//
// ── IT ASKS THE LIVE QUESTION (§R.104) ────────────────────────────────────
//
// A non-terminal order with a non-terminal child IS an excavation running. Keying
// this on a column instead is how it goes blind: arm 3's one-dig-per-episode gate
// then admits a second dig every time, cheerfully.
//
// It now asks the live question instead of a proxy for it: is there a
// non-terminal order in this episode with a non-terminal CHILD. That is the
// recognition predicate ("a parent is anything with children") scoped to an
// episode, it needs no column, and it is true of every dig shape there is —
// including the staged dweller digging from its mark, which no column-based
// spelling could have described.
//
// The old sentence, kept because its second half is the reason the predicate has
// to be about children rather than about the parent: "a terminal dig is finished
// work and a plain buried retrieve carries no target, because it IS the demand
// and cannot compete with itself." Under §R.91 every dig is that shape, so a
// target-based spelling would now exclude all of them.
//
// It answers "has this demand already got an excavation going", and the answer
// gates raising a second one. See laneClearEpisodeAlreadyDigging for the
// standoff that made this necessary and for why the ORIGIN is the key.
//
// An empty origin is not answerable and must not be guessed at: an order with no
// episode gets no limit, which is the same disposition every other episode-keyed
// reader takes and the honest one — the alternative is to serialise every
// unattributed dig in the plant against every other.
//
// ── IT NOW SAYS WHETHER IT ASKED, AND THAT IS THE WHOLE CHANGE ────────────
//
// asked=false means the question was never put to the database. It used to
// return (0, nil) for that, which is the same pair it returns for "I asked and
// there is no dig running" — so the caller's gate read a refusal-to-answer as a
// clean bill of health and let the dig through. The GATE'S BEHAVIOUR IS
// UNCHANGED and deliberately so: an unattributed demand still gets no limit,
// because serialising every originless dig in the plant against every other is
// the worse failure. What changes is that the caller can now RECORD that its
// admission control did not run, instead of being unable to tell.
//
// This is the house rule about a check knowing whether it had its input, applied
// to a reader rather than to a check: a bare zero cannot distinguish absence of
// data from absence of a finding, so the reader must return both facts.
func (db *DB) LiveServiceDigInEpisode(originID string) (digID int64, asked bool, err error) {
	if originID == "" {
		return 0, false, nil
	}
	var id int64
	qErr := db.QueryRow(fmt.Sprintf(
		`SELECT p.id FROM orders p
		  WHERE p.origin_id = $1 AND p.status NOT IN (%[1]s)
		    AND EXISTS (SELECT 1 FROM orders c
		                 WHERE c.parent_order_id = p.id AND c.status NOT IN (%[1]s))
		  ORDER BY p.id LIMIT 1`, protocol.TerminalStatusSQLList()), originID).Scan(&id)
	if qErr == sql.ErrNoRows {
		return 0, true, nil
	}
	if qErr != nil {
		return 0, false, fmt.Errorf("live service dig in episode %s: %w", originID, qErr)
	}
	return id, true, nil
}

// SlotsThatWouldEntombASpokenForSlot returns the slots in a group that a dig
// must not park a blocker in, because doing so would seal an EMPTY slot deeper
// in the same lane that a live order is coming to fill.
//
// It is the dig's half of EntombsASpokenForSlotSQL; the store selector asks the
// same question inline, of one candidate, which is exactly how the two halves of
// the burial guard beside it are already arranged. Group-scoped and read once
// per pass, for the same reason the burial set is: it is asked per candidate and
// the answer cannot change mid-pass without the pass being wrong anyway.
//
// OWNER-BLIND, and that is the load-bearing half. A dig is exempt from its own
// dig lock everywhere else, and the exemption here would let a compound entomb a
// slot its own next leg is driving to — which costs the plant the same slot as
// anybody else entombing it would. The burial exclusion makes the identical
// argument in the identical words: not even your own.
func (db *DB) SlotsThatWouldEntombASpokenForSlot(groupID int64) (map[int64]bool, error) {
	rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT DISTINCT n.id
		FROM nodes n
		JOIN nodes lane ON lane.id = n.parent_id
		WHERE lane.parent_id = $1
		  AND %s`, helpers.EntombsASpokenForSlotSQL("n", "0", protocol.TerminalStatusSQLList())), groupID)
	if err != nil {
		return nil, fmt.Errorf("slots that would entomb a spoken-for slot in group %d: %w", groupID, err)
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan entombing slot: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (db *DB) SlotsBlockedByHardClaims(groupID int64) (map[int64]bool, error) {
	rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT DISTINCT n.id
		FROM nodes n
		JOIN nodes lane ON lane.id = n.parent_id
		WHERE lane.parent_id = $1
		  AND EXISTS (
			SELECT 1 FROM nodes held
			JOIN bins held_bin ON held_bin.node_id = held.id
			JOIN orders holder ON holder.id = held_bin.claimed_by
			                   OR (holder.bin_id = held_bin.id AND holder.source_node = held.name)
			WHERE held_bin.status <> 'retired'
			  AND holder.parent_order_id IS NULL
			  AND holder.status NOT IN (%s)
			  AND %s
			  AND %s
		  )`, protocol.TerminalStatusSQLList(),
		helpers.ShallowerInSameLane("n", "held"), helpers.ReachableSQL("held")), groupID)
	if err != nil {
		return nil, fmt.Errorf("slots blocked by hard claims in group %d: %w", groupID, err)
	}
	defer rows.Close()

	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan blocked slot: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
