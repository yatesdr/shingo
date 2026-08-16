package store

// Stage 2D delegate file: lane-scoped node queries live in store/nodes/.
// The bin-returning lane searches (FindSourceBinInLane, FindBuriedBin,
// FindOldestBuriedBin) stay here as cross-aggregate composition methods
// because their return type is *bins.Bin (bins aggregate) while the WHERE
// clause joins nodes via parent_id.

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth
// (ascending).
func (db *DB) ListLaneSlots(laneID int64) ([]*nodes.Node, error) {
	return nodes.ListLaneSlots(db.DB, laneID)
}

// ListHeldLegParentsInLane returns the distinct parents of PENDING compound legs
// whose source or delivery node is a slot of this lane.
//
// It exists to answer "who is waiting on this lane" for a leg that has no other
// witness. A leg held at its lane writes no status — it stays `pending` — so it
// is invisible to every query that looks for work in progress, and the only
// releaser its own dispatch path names is a SIBLING's dropoff completion. With no
// sibling running there is nothing to come back, which is the wedge. Keying on
// the lane instead means the event that actually frees the lane can find it,
// whoever caused it.
//
// BOTH NAME FORMS ARE MATCHED. orders.source_node / delivery_node hold whatever
// the planner wrote, and node references are bare ("SLN_002") or dotted
// ("LANE.SLN_002") depending on the caller — GetByDotName splits on the dot and
// falls back to a bare lookup, so both are live. Matching only one form would
// silently return no parents for half the plant, which is the failure mode this
// query exists to prevent and would be invisible in exactly the same way.
func (db *DB) ListHeldLegParentsInLane(laneID int64) ([]int64, error) {
	rows, err := db.DB.Query(`
		SELECT DISTINCT o.parent_order_id
		FROM orders o
		JOIN nodes n ON n.parent_id = $1
		LEFT JOIN nodes p ON p.id = n.parent_id
		WHERE o.parent_order_id IS NOT NULL
		  AND o.status = 'pending'
		  AND (
		        o.source_node   = n.name
		     OR o.delivery_node = n.name
		     OR o.source_node   = p.name || '.' || n.name
		     OR o.delivery_node = p.name || '.' || n.name
		  )`, laneID)
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
func (db *DB) ListChildNodesUnlocked(parentID int64) ([]*nodes.Node, error) {
	rows, err := db.DB.Query(fmt.Sprintf(`SELECT %s %s
		WHERE n.parent_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM reservations dig_hold
			WHERE dig_hold.resource_kind = 'mouth'
			  AND dig_hold.node_id = n.id
			  AND dig_hold.state IN ('pending','confirmed')
			  AND dig_hold.mode = $2
		  )
		ORDER BY n.name`, nodes.SelectCols, nodes.FromClause), parentID, string(reservations.ModeDig))
	if err != nil {
		return nil, fmt.Errorf("list unlocked children of %d: %w", parentID, err)
	}
	defer rows.Close()
	return nodes.ScanNodes(rows)
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
		  AND b.status = 'available'
		  AND ($2 = '' OR b.payload_code = $2)
		  AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.bin_id = b.id AND r.state = 'pending')
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
		  AND b.status = 'available'
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
		  AND b.status = 'available'
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
		       COALESCE(r.created_at, o.created_at) AS held_since
		FROM nodes held
		CROSS JOIN placed
		JOIN bins b ON b.node_id = held.id AND b.status <> 'retired'
		LEFT JOIN reservations r
		       ON r.bin_id = b.id AND r.resource_kind = 'bin'
		      AND r.state IN ('pending','confirmed')
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
		if err := rows.Scan(&s.BinID, &s.BinLabel, &s.SlotName, &s.Depth,
			&s.HardClaim, &s.HolderID, &s.HolderStatus, &s.HolderIsChild, &s.HeldSince); err != nil {
			return nil, fmt.Errorf("spoken-for bins scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// OrderIsCompoundLeg reports whether an order is a compound (dig) child. The
// burial instrument asks it about the PLACING order: a dig leg picks its
// destination through findShuffleSlots, which does not consult the store-slot
// selector, so a dig burying a claimed bin is a known uncovered path rather than
// a guard bypass.
func (db *DB) OrderIsCompoundLeg(orderID int64) (bool, error) {
	var isChild bool
	err := db.QueryRow(`SELECT parent_order_id IS NOT NULL FROM orders WHERE id=$1`, orderID).Scan(&isChild)
	if err != nil {
		return false, fmt.Errorf("order %d compound-leg check: %w", orderID, err)
	}
	return isChild, nil
}
