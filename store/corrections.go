package store

import (
	"database/sql"
	"time"
)

type Correction struct {
	ID             int64
	CorrectionType string
	NodeID         int64
	MaterialID     *int64
	InventoryID    *int64
	Quantity       float64
	Reason         string
	Actor          string
	CreatedAt      time.Time
}

func (db *DB) CreateCorrection(c *Correction) error {
	var matID, invID any
	if c.MaterialID != nil {
		matID = *c.MaterialID
	}
	if c.InventoryID != nil {
		invID = *c.InventoryID
	}
	result, err := db.Exec(db.Q(`INSERT INTO corrections (correction_type, node_id, material_id, inventory_id, quantity, reason, actor) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		c.CorrectionType, c.NodeID, matID, invID, c.Quantity, c.Reason, c.Actor)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	c.ID = id
	return nil
}

func (db *DB) ListCorrections(limit int) ([]*Correction, error) {
	rows, err := db.Query(db.Q(`SELECT id, correction_type, node_id, material_id, inventory_id, quantity, reason, actor, created_at FROM corrections ORDER BY id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var corrections []*Correction
	for rows.Next() {
		var c Correction
		var createdAt string
		var materialID, inventoryID sql.NullInt64
		if err := rows.Scan(&c.ID, &c.CorrectionType, &c.NodeID, &materialID, &inventoryID, &c.Quantity, &c.Reason, &c.Actor, &createdAt); err != nil {
			return nil, err
		}
		if materialID.Valid {
			c.MaterialID = &materialID.Int64
		}
		if inventoryID.Valid {
			c.InventoryID = &inventoryID.Int64
		}
		c.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		corrections = append(corrections, &c)
	}
	return corrections, rows.Err()
}
