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
