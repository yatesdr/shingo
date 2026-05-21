// Package inventory holds inventory listing + correction persistence
// for shingo-core.
//
// Phase 5 of the architecture plan moved the cross-aggregate inventory
// listing query and the corrections CRUD out of the flat store/ package
// and into this sub-package. The outer store/ keeps type aliases
// (`store.InventoryRow = inventory.Row`,
// `store.Correction = inventory.Correction`) and one-line delegate
// methods on *store.DB so external callers see no API change.
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
	"shingocore/store/internal/helpers"
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

	PayloadCode  string `json:"payload_code"`
	CatID        string `json:"cat_id"`
	Qty          int64  `json:"qty"`
	UOPRemaining int    `json:"uop_remaining"`
	Confirmed    bool   `json:"confirmed"`
}

// Correction is one corrections-table row.
type Correction struct {
	ID             int64     `json:"id"`
	CorrectionType string    `json:"correction_type"`
	NodeID         int64     `json:"node_id"`
	BinID          *int64    `json:"bin_id,omitempty"`
	CatID          string    `json:"cat_id"`
	Description    string    `json:"description"`
	Quantity       int64     `json:"quantity"`
	Reason         string    `json:"reason"`
	Actor          string    `json:"actor"`
	CreatedAt      time.Time `json:"created_at"`
}

// inventorySQL is computed once at package init so the terminal-status
// list is injected from protocol.TerminalStatusSQLList() rather than
// being hand-rolled here.
var inventorySQL = fmt.Sprintf(`
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
// State is derived rather than stored: rows in lineside_buckets are
// garbage-collected at qty=0, so any row that surfaces here is by
// definition "active". The field is exposed for parity with Edge's
// admin Lineside Buckets table and so a future "inactive" bucket
// shape (if it ever lands on Core) has a place to plug in.
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
    b.qty
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
		if err := rows.Scan(
			&r.ID,
			&r.GroupName, &r.LaneName, &r.NodeName, &r.Zone,
			&r.Station, &r.StyleID, &r.PartNumber, &r.PayloadCode, &r.Qty,
		); err != nil {
			return nil, fmt.Errorf("scan lineside_buckets row: %w", err)
		}
		// GC contract on lineside_buckets removes qty=0 rows; anything
		// returned here is active by definition.
		r.State = "active"
		out = append(out, r)
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

// CreateCorrection inserts one corrections row and sets c.ID on success.
func CreateCorrection(db *sql.DB, c *Correction) error {
	id, err := helpers.InsertID(db, `INSERT INTO corrections (correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		c.CorrectionType, c.NodeID, helpers.NullableInt64(c.BinID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

// ListCorrections returns recent corrections rows.
func ListCorrections(db *sql.DB, limit int) ([]*Correction, error) {
	rows, err := db.Query(`SELECT id, correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor, created_at FROM corrections ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

// ListCorrectionsByNode returns recent corrections rows for one node.
func ListCorrectionsByNode(db *sql.DB, nodeID int64, limit int) ([]*Correction, error) {
	rows, err := db.Query(`SELECT id, correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor, created_at FROM corrections WHERE node_id = $1 ORDER BY id DESC LIMIT $2`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

// ApplyBinManifestChanges records corrections rows for a bin's manifest
// changes inside a single transaction.
func ApplyBinManifestChanges(db *sql.DB, binID int64, corrections []*Correction) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, c := range corrections {
		_, err := tx.Exec(`INSERT INTO corrections (correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			c.CorrectionType, c.NodeID, helpers.NullableInt64(c.BinID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
		if err != nil {
			return fmt.Errorf("insert correction: %w", err)
		}
	}

	return tx.Commit()
}

func scanCorrections(rows *sql.Rows) ([]*Correction, error) {
	var corrections []*Correction
	for rows.Next() {
		var c Correction
		var binID sql.NullInt64
		if err := rows.Scan(&c.ID, &c.CorrectionType, &c.NodeID, &binID, &c.CatID, &c.Description, &c.Quantity, &c.Reason, &c.Actor, &c.CreatedAt); err != nil {
			return nil, err
		}
		if binID.Valid {
			c.BinID = &binID.Int64
		}
		corrections = append(corrections, &c)
	}
	return corrections, rows.Err()
}
