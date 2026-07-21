package store

// Stage 2D delegate file: lane-scoped node queries live in store/nodes/.
// The bin-returning lane searches (FindSourceBinInLane, FindBuriedBin,
// FindOldestBuriedBin) stay here as cross-aggregate composition methods
// because their return type is *bins.Bin (bins aggregate) while the WHERE
// clause joins nodes via parent_id.

import (
	"fmt"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// ListLaneSlots returns all child nodes of a lane, ordered by depth
// (ascending).
func (db *DB) ListLaneSlots(laneID int64) ([]*nodes.Node, error) {
	return nodes.ListLaneSlots(db.DB, laneID)
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
		  AND NOT EXISTS (
			SELECT 1 FROM nodes sib
			JOIN bins sb ON sb.node_id = sib.id
			WHERE sib.parent_id = $1
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(n.depth, 0) ASC
		LIMIT 1`, bins.BinJoinQuery)
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
		  AND EXISTS (
			SELECT 1 FROM nodes sib
			JOIN bins sb ON sb.node_id = sib.id
			WHERE sib.parent_id = $1
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(b.loaded_at, b.created_at) ASC
		LIMIT 1`, bins.BinJoinQuery), laneID, payloadCode)
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
		  AND EXISTS (
			SELECT 1 FROM nodes sib
			JOIN bins sb ON sb.node_id = sib.id
			WHERE sib.parent_id = $1
			  AND sib.depth IS NOT NULL
			  AND n.depth IS NOT NULL
			  AND sib.depth < n.depth
		  )
		ORDER BY COALESCE(n.depth, 0) ASC
		LIMIT 1`, bins.BinJoinQuery), laneID, payloadCode)
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
