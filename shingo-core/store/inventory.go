package store

type InventoryRow struct {
	// Hierarchy
	GroupName string `json:"group_name"`
	LaneName  string `json:"lane_name"`
	NodeName  string `json:"node_name"`
	Zone      string `json:"zone"`

	// Bin
	BinID    int64  `json:"bin_id"`
	BinLabel string `json:"bin_label"`
	BinType  string `json:"bin_type"`
	Status   string `json:"status"`

	// In-transit: if bin is claimed by an active order, show planned destination
	InTransit   bool   `json:"in_transit"`
	Destination string `json:"destination,omitempty"`

	// Contents
	PayloadCode  string `json:"payload_code"`
	CatID        string `json:"cat_id"`
	Qty          int64  `json:"qty"`
	UOPRemaining int    `json:"uop_remaining"`
	Confirmed    bool   `json:"confirmed"`
}

const inventorySQL = `
WITH bin_items AS (
    -- Bins with manifest items
    SELECT b.id AS bin_id, b.label AS bin_label, bt.code AS bin_type,
           b.node_id, b.status, b.payload_code, b.uop_remaining,
           b.manifest_confirmed AS confirmed, b.claimed_by,
           (item->>'catid') AS cat_id,
           (item->>'qty')::bigint AS qty
    FROM bins b
    JOIN bin_types bt ON bt.id = b.bin_type_id
    LEFT JOIN LATERAL jsonb_array_elements(
        CASE WHEN b.manifest IS NOT NULL AND b.manifest != 'null'
             AND jsonb_typeof(b.manifest->'items') = 'array'
             AND jsonb_array_length(b.manifest->'items') > 0
             THEN b.manifest->'items'
             ELSE NULL
        END
    ) AS item ON true
    WHERE item IS NOT NULL

    UNION ALL

    -- Bins with no manifest items (empty or no manifest) - one row per bin
    SELECT b.id, b.label, bt.code,
           b.node_id, b.status, b.payload_code, b.uop_remaining,
           b.manifest_confirmed, b.claimed_by,
           '' AS cat_id, 0 AS qty
    FROM bins b
    JOIN bin_types bt ON bt.id = b.bin_type_id
    WHERE b.manifest IS NULL
       OR b.manifest = 'null'
       OR jsonb_typeof(b.manifest->'items') != 'array'
       OR jsonb_array_length(b.manifest->'items') = 0
)
SELECT
    COALESCE(grp.name, '') AS group_name,
    CASE WHEN lane_type.code = 'LANE' THEN COALESCE(lane.name, '') ELSE '' END AS lane_name,
    COALESCE(n.name, '') AS node_name,
    COALESCE(n.zone, '') AS zone,
    bi.bin_id, bi.bin_label, bi.bin_type, bi.status,
    (bi.claimed_by IS NOT NULL AND o.id IS NOT NULL) AS in_transit,
    COALESCE(o.delivery_node, '') AS destination,
    COALESCE(bi.payload_code, '') AS payload_code,
    COALESCE(bi.cat_id, '') AS cat_id,
    COALESCE(bi.qty, 0) AS qty,
    bi.uop_remaining,
    bi.confirmed
FROM bin_items bi
LEFT JOIN nodes n ON n.id = bi.node_id
LEFT JOIN nodes lane ON lane.id = n.parent_id
LEFT JOIN node_types lane_type ON lane_type.id = lane.node_type_id
LEFT JOIN nodes grp ON grp.id = COALESCE(
    CASE WHEN lane_type.code = 'LANE' THEN lane.parent_id ELSE lane.id END,
    n.parent_id
)
LEFT JOIN node_types grp_type ON grp_type.id = grp.node_type_id AND grp_type.code = 'NGRP'
LEFT JOIN orders o ON o.id = bi.claimed_by
    AND o.status NOT IN ('confirmed', 'failed', 'cancelled')
ORDER BY group_name, lane_name, COALESCE(n.depth, 0), node_name, bi.bin_label, bi.cat_id
`

func (db *DB) ListInventory() ([]InventoryRow, error) {
	rows, err := db.Query(inventorySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []InventoryRow
	for rows.Next() {
		var r InventoryRow
		if err := rows.Scan(
			&r.GroupName, &r.LaneName, &r.NodeName, &r.Zone,
			&r.BinID, &r.BinLabel, &r.BinType, &r.Status,
			&r.InTransit, &r.Destination,
			&r.PayloadCode, &r.CatID, &r.Qty, &r.UOPRemaining, &r.Confirmed,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
