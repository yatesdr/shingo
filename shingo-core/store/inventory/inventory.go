// Package inventory holds the cross-aggregate inventory listing query
// for shingo-core.
//
// Phase 5 of the architecture plan moved this query out of the flat
// store/ package and into this sub-package. The outer store/ keeps a
// type alias (`store.InventoryRow = inventory.Row`) and one-line
// delegate methods on *store.DB so external callers see no API change.
//
// It also held the corrections CRUD until the correction path was
// removed wholesale — see the commit that deleted engine/corrections.go.
//
// The inventory listing query reads bins + nodes + bin_types + orders
// in one CTE; it is grouped here (rather than at the outer store/ level)
// because the result is conceptually one denormalized inventory view —
// it is the "inventory" aggregate from the user's perspective even
// though it joins three persistence aggregates.
package inventory

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
)

// Row is the denormalized inventory listing row.
type Row struct {
	GroupName string `json:"group_name"`
	LaneName  string `json:"lane_name"`
	NodeName  string `json:"node_name"`
	Zone      string `json:"zone"`

	BinID    int64  `json:"bin_id"`
	BinLabel string `json:"bin_label"`
	BinType  string `json:"bin_type"`
	Status   string `json:"status"`

	InTransit   bool   `json:"in_transit"`
	Destination string `json:"destination,omitempty"`

	PayloadCode string `json:"payload_code"`
	CatID       string `json:"cat_id"`
	// Qty is derived at query time as UOPRemaining x the payload template's
	// parts_per_cycle for this CatID, not read off the bin manifest. Zero
	// when the template carries no line for the part.
	Qty          int64 `json:"qty"`
	UOPRemaining int   `json:"uop_remaining"`
	Confirmed    bool  `json:"confirmed"`
}

// inventorySQL is computed once at package init so the terminal-status
// list is injected from protocol.TerminalStatusSQLList() rather than
// being hand-rolled here.
var inventorySQL = fmt.Sprintf(`
WITH bin_items AS (
    -- Bins with manifest items
    -- qty is DERIVED: uop_remaining x the template's parts_per_cycle. The
    -- manifest line carries no count of its own — it used to, and nothing
    -- rewrote that number as the bin was drawn down, so this column reported
    -- a stale count for every partially-consumed bin.
    -- The LEFT JOIN keeps a bin listed when its template has no line for
    -- the part; the count is then 0, which is the honest answer when there
    -- is no ratio to count by.
    SELECT b.id AS bin_id, b.label AS bin_label, bt.code AS bin_type,
           b.node_id, b.status, b.payload_code, b.uop_remaining,
           b.manifest_confirmed AS confirmed, b.claimed_by,
           (item->>'catid') AS cat_id,
           COALESCE(b.uop_remaining * pm.parts_per_cycle, 0)::bigint AS qty
    FROM bins b
    JOIN bin_types bt ON bt.id = b.bin_type_id
    LEFT JOIN payloads p ON p.code = b.payload_code
    LEFT JOIN LATERAL jsonb_array_elements(
        CASE WHEN b.manifest IS NOT NULL AND b.manifest != 'null'
             AND jsonb_typeof(b.manifest->'items') = 'array'
             AND jsonb_array_length(b.manifest->'items') > 0
             THEN b.manifest->'items'
             ELSE NULL
        END
    ) AS item ON true
    LEFT JOIN payload_manifest pm
           ON pm.payload_id = p.id AND pm.part_number = (item->>'catid')
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
    AND o.status NOT IN (%s)
ORDER BY group_name, lane_name, COALESCE(n.depth, 0), node_name, bi.bin_label, bi.cat_id
`, protocol.TerminalStatusSQLList())

// List returns one denormalized inventory row per (bin, manifest item).
func List(db *sql.DB) ([]Row, error) {
	rows, err := db.Query(inventorySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Row
	for rows.Next() {
		var r Row
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

// BucketRow is the denormalized lineside_buckets listing row used by
// the Core inventory page. Mirrors the field naming on Row so the JS
// renderer can reuse the existing cell/lane/node columns alongside
// the bucket-specific fields (Station, StyleID, PartNumber, Qty,
// State).
//
// State ("active" | "stranded") is derived at query time from the
// plant-claims active-style mirror (process_styles.is_active +
// style_claims), NOT stored: a bucket is "stranded" when its node runs
// an active style whose payload set no longer covers the bucket's
// payload — real inventory the running style won't consume, surfaced so
// the operator can recall it. Mirrors Edge's admin Lineside Buckets table.
type BucketRow struct {
	// ID is the lineside_buckets.id primary key. Surfaced on the wire
	// so the Core admin "Lineside Buckets" page can drive the Round-3
	// Obs 10 delete action against a specific row without ambiguity.
	ID        int64  `json:"id"`
	GroupName string `json:"group_name"`
	LaneName  string `json:"lane_name"`
	NodeName  string `json:"node_name"`
	Zone      string `json:"zone"`

	Station     string `json:"station"`
	StyleID     int64  `json:"style_id"`
	PartNumber  string `json:"part_number"`
	PayloadCode string `json:"payload_code"`
	Qty         int    `json:"qty"`
	State       string `json:"state"`
	// UpdatedAt is the last time this bucket row's qty changed. Surfaced so the
	// inventory page can colour stale rows (amber >7d, coral >30d) — a bucket
	// untouched for a month is a ghost candidate that inflates the in-loop total.
	UpdatedAt time.Time `json:"updated_at"`
}

// linesideBucketsSQL mirrors the bin-side inventory join (cell → lane →
// node) so the rendered listing groups consistently. Storage nodes
// without a lane/group parent surface with empty group_name / lane_name
// — same as the bin listing.
// Round-3 Obs 8: join lineside_buckets on core_node_name → nodes.name
// instead of node_id → nodes.id. The pre-fix LEFT JOIN treated Edge's
// int64 namespace as if it were Core's, so cross-plant or post-rename
// rows joined to the wrong node and rendered under the wrong group.
// COALESCE(n.name, b.core_node_name) preserves the bucket's
// self-attribution even when Core hasn't seen the node yet (e.g. a
// rename in progress) so the admin page can show what Edge sent.
const linesideBucketsSQL = `
SELECT
    b.id,
    COALESCE(grp.name, '') AS group_name,
    CASE WHEN lane_type.code = 'LANE' THEN COALESCE(lane.name, '') ELSE '' END AS lane_name,
    COALESCE(n.name, b.core_node_name) AS node_name,
    COALESCE(n.zone, '') AS zone,
    b.station, b.style_id, b.part_number,
    COALESCE(b.payload_code, '') AS payload_code,
    b.qty, b.updated_at,
    (
      EXISTS (
        SELECT 1 FROM style_claims sc
        JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
        WHERE sc.core_node_name = b.core_node_name AND ps.is_active
      )
      AND NOT EXISTS (
        SELECT 1 FROM style_claims sc
        JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
        WHERE sc.core_node_name = b.core_node_name AND ps.is_active
          AND (sc.payload_code = b.payload_code
               OR jsonb_exists(sc.allowed_payload_codes::jsonb, b.payload_code))
      )
    ) AS stranded
FROM lineside_buckets b
LEFT JOIN nodes n ON n.name = b.core_node_name
LEFT JOIN nodes lane ON lane.id = n.parent_id
LEFT JOIN node_types lane_type ON lane_type.id = lane.node_type_id
LEFT JOIN nodes grp ON grp.id = COALESCE(
    CASE WHEN lane_type.code = 'LANE' THEN lane.parent_id ELSE lane.id END,
    n.parent_id
)
LEFT JOIN node_types grp_type ON grp_type.id = grp.node_type_id AND grp_type.code = 'NGRP'
ORDER BY group_name, b.station, COALESCE(n.depth, 0), node_name, b.part_number
`

// ListLinesideBuckets returns every lineside_buckets row joined to the
// node hierarchy so the Core inventory page can render them alongside
// the existing bins table. Rows are ordered by cell → station → node
// → part for stable on-screen grouping. Empty table returns nil.
func ListLinesideBuckets(db *sql.DB) ([]BucketRow, error) {
	rows, err := db.Query(linesideBucketsSQL)
	if err != nil {
		return nil, fmt.Errorf("query lineside_buckets: %w", err)
	}
	defer rows.Close()

	var out []BucketRow
	for rows.Next() {
		var r BucketRow
		var stranded bool
		if err := rows.Scan(
			&r.ID,
			&r.GroupName, &r.LaneName, &r.NodeName, &r.Zone,
			&r.Station, &r.StyleID, &r.PartNumber, &r.PayloadCode, &r.Qty, &r.UpdatedAt,
			&stranded,
		); err != nil {
			return nil, fmt.Errorf("scan lineside_buckets row: %w", err)
		}
		// State is derived from the plant-claims active-style mirror (same rule as
		// SystemUOPForPayload's stranded exclusion): "stranded" when the bucket's node
		// runs an active style that no longer covers the bucket's payload — real
		// inventory the running style won't consume, which the operator should recall.
		r.State = "active"
		if stranded {
			r.State = "stranded"
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DistinctStockedPayloads returns every payload_code with current stock — a
// non-empty payload on at least one bin (any lifecycle) or one lineside bucket.
// Used to build the Replenishment Health rollup so payloads that are stocked but
// have no threshold configured still appear (as "no threshold set") next to the
// monitored ones.
func DistinctStockedPayloads(db *sql.DB) ([]string, error) {
	const q = `
SELECT payload_code FROM (
    SELECT DISTINCT payload_code FROM bins WHERE COALESCE(payload_code, '') <> ''
    UNION
    SELECT DISTINCT payload_code FROM lineside_buckets WHERE COALESCE(payload_code, '') <> ''
) s
ORDER BY payload_code`
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("distinct stocked payloads: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan stocked payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteLinesideBucket removes one lineside_buckets row by primary
// key, atomically with the matching inventory_delta_dedup row so the
// dedup table doesn't shadow future deltas for the same scope.
//
// Round-3 Obs 10: powers the operator-driven "Clear" button on the
// Core admin "Lineside Buckets" table — the path for clearing the
// Core-only orphan rows that pre-Obs-8 cross-namespace bugs left
// behind. After Obs 8's CoreNodeName validation lands, orphans
// shouldn't be createable; this remains as the recovery hatch for
// the existing wedge plus any future operator-corrected drift.
//
// Returns the number of rows deleted from lineside_buckets (0 or 1)
// so callers can surface "no such row" without needing a separate
// lookup.
func DeleteLinesideBucket(db *sql.DB, id int64) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var (
		station      string
		coreNodeName string
		pairKey      string
		styleID      int64
		partNumber   string
	)
	if err := tx.QueryRow(`SELECT station, core_node_name, pair_key, style_id, part_number
		FROM lineside_buckets WHERE id=$1`, id).Scan(&station, &coreNodeName, &pairKey, &styleID, &partNumber); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("lookup bucket %d: %w", id, err)
	}

	res, err := tx.Exec(`DELETE FROM lineside_buckets WHERE id=$1`, id)
	if err != nil {
		return 0, fmt.Errorf("delete lineside_buckets %d: %w", id, err)
	}
	n, _ := res.RowsAffected()

	// Matching dedup row uses bucketScopeKey's pipe-delimited shape:
	// <CoreNodeName>|<PairKey>|<StyleID>|<PartNumber>. Inline here so
	// store/inventory/ doesn't depend on shingocore/uop just for the
	// helper.
	scopeKey := fmt.Sprintf("%s|%s|%d|%s", coreNodeName, pairKey, styleID, partNumber)
	if _, err := tx.Exec(`DELETE FROM inventory_delta_dedup
		WHERE station=$1 AND scope_kind='bucket' AND scope_key=$2`, station, scopeKey); err != nil {
		return 0, fmt.Errorf("delete dedup row for bucket %d (scope_key=%s): %w", id, scopeKey, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit bucket delete %d: %w", id, err)
	}
	return int(n), nil
}
