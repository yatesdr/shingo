package store

import (
	"database/sql"
	"time"
)

type Correction struct {
	ID              int64
	CorrectionType  string
	NodeID          int64
	MaterialID      *int64
	InventoryID     *int64
	PayloadID       *int64
	ManifestItemID  *int64
	CatID           string
	Description     string
	Quantity        float64
	Reason          string
	Actor           string
	CreatedAt       time.Time
}

func (db *DB) CreateCorrection(c *Correction) error {
	var matID, invID, plID, miID any
	if c.MaterialID != nil {
		matID = *c.MaterialID
	}
	if c.InventoryID != nil {
		invID = *c.InventoryID
	}
	if c.PayloadID != nil {
		plID = *c.PayloadID
	}
	if c.ManifestItemID != nil {
		miID = *c.ManifestItemID
	}
	result, err := db.Exec(db.Q(`INSERT INTO corrections (correction_type, node_id, material_id, inventory_id, payload_id, manifest_item_id, cat_id, description, quantity, reason, actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		c.CorrectionType, c.NodeID, matID, invID, plID, miID, c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	c.ID = id
	return nil
}

func (db *DB) ListCorrections(limit int) ([]*Correction, error) {
	rows, err := db.Query(db.Q(`SELECT id, correction_type, node_id, material_id, inventory_id, payload_id, manifest_item_id, cat_id, description, quantity, reason, actor, created_at FROM corrections ORDER BY id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var corrections []*Correction
	for rows.Next() {
		var c Correction
		var createdAt string
		var materialID, inventoryID, payloadID, manifestItemID sql.NullInt64
		if err := rows.Scan(&c.ID, &c.CorrectionType, &c.NodeID, &materialID, &inventoryID, &payloadID, &manifestItemID, &c.CatID, &c.Description, &c.Quantity, &c.Reason, &c.Actor, &createdAt); err != nil {
			return nil, err
		}
		if materialID.Valid {
			c.MaterialID = &materialID.Int64
		}
		if inventoryID.Valid {
			c.InventoryID = &inventoryID.Int64
		}
		if payloadID.Valid {
			c.PayloadID = &payloadID.Int64
		}
		if manifestItemID.Valid {
			c.ManifestItemID = &manifestItemID.Int64
		}
		c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		corrections = append(corrections, &c)
	}
	return corrections, rows.Err()
}
