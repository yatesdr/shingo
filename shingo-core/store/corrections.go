package store

import (
	"database/sql"
	"time"
)

type Correction struct {
	ID             int64     `json:"id"`
	CorrectionType string    `json:"correction_type"`
	NodeID         int64     `json:"node_id"`
	InstanceID     *int64    `json:"instance_id,omitempty"`
	ManifestItemID *int64    `json:"manifest_item_id,omitempty"`
	CatID          string    `json:"cat_id"`
	Description    string    `json:"description"`
	Quantity       float64   `json:"quantity"`
	Reason         string    `json:"reason"`
	Actor          string    `json:"actor"`
	CreatedAt      time.Time `json:"created_at"`
}

func (db *DB) CreateCorrection(c *Correction) error {
	result, err := db.Exec(db.Q(`INSERT INTO corrections (correction_type, node_id, instance_id, manifest_item_id, cat_id, description, quantity, reason, actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		c.CorrectionType, c.NodeID, nullableInt64(c.InstanceID), nullableInt64(c.ManifestItemID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	c.ID = id
	return nil
}

func (db *DB) ListCorrections(limit int) ([]*Correction, error) {
	rows, err := db.Query(db.Q(`SELECT id, correction_type, node_id, instance_id, manifest_item_id, cat_id, description, quantity, reason, actor, created_at FROM corrections ORDER BY id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

func (db *DB) ListCorrectionsByNode(nodeID int64, limit int) ([]*Correction, error) {
	rows, err := db.Query(db.Q(`SELECT id, correction_type, node_id, instance_id, manifest_item_id, cat_id, description, quantity, reason, actor, created_at FROM corrections WHERE node_id = ? ORDER BY id DESC LIMIT ?`), nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

func scanCorrections(rows *sql.Rows) ([]*Correction, error) {
	var corrections []*Correction
	for rows.Next() {
		var c Correction
		var createdAt any
		var instanceID, manifestItemID sql.NullInt64
		if err := rows.Scan(&c.ID, &c.CorrectionType, &c.NodeID, &instanceID, &manifestItemID, &c.CatID, &c.Description, &c.Quantity, &c.Reason, &c.Actor, &createdAt); err != nil {
			return nil, err
		}
		if instanceID.Valid {
			c.InstanceID = &instanceID.Int64
		}
		if manifestItemID.Valid {
			c.ManifestItemID = &manifestItemID.Int64
		}
		c.CreatedAt = parseTime(createdAt)
		corrections = append(corrections, &c)
	}
	return corrections, rows.Err()
}
