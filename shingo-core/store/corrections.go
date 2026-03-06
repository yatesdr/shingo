package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Correction struct {
	ID             int64     `json:"id"`
	CorrectionType string    `json:"correction_type"`
	NodeID         int64     `json:"node_id"`
	PayloadID      *int64    `json:"payload_id,omitempty"`
	ManifestItemID *int64    `json:"manifest_item_id,omitempty"`
	CatID          string    `json:"cat_id"`
	Description    string    `json:"description"`
	Quantity       int64     `json:"quantity"`
	Reason         string    `json:"reason"`
	Actor          string    `json:"actor"`
	CreatedAt      time.Time `json:"created_at"`
}

func (db *DB) CreateCorrection(c *Correction) error {
	result, err := db.Exec(db.Q(`INSERT INTO corrections (correction_type, node_id, payload_id, manifest_item_id, cat_id, description, quantity, reason, actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		c.CorrectionType, c.NodeID, nullableInt64(c.PayloadID), nullableInt64(c.ManifestItemID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	c.ID = id
	return nil
}

func (db *DB) ListCorrections(limit int) ([]*Correction, error) {
	rows, err := db.Query(db.Q(`SELECT id, correction_type, node_id, payload_id, manifest_item_id, cat_id, description, quantity, reason, actor, created_at FROM corrections ORDER BY id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

func (db *DB) ListCorrectionsByNode(nodeID int64, limit int) ([]*Correction, error) {
	rows, err := db.Query(db.Q(`SELECT id, correction_type, node_id, payload_id, manifest_item_id, cat_id, description, quantity, reason, actor, created_at FROM corrections WHERE node_id = ? ORDER BY id DESC LIMIT ?`), nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

// ApplyBatchManifestChanges applies adds, updates, and deletes to manifest_items
// and records correction rows, all within a single transaction.
func (db *DB) ApplyBatchManifestChanges(payloadID int64, adds []*ManifestItem, updates []*ManifestItem, deleteIDs []int64, corrections []*Correction) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range adds {
		result, err := tx.Exec(db.Q(`INSERT INTO manifest_items (payload_id, part_number, quantity, notes) VALUES (?, ?, ?, ?)`),
			payloadID, m.PartNumber, m.Quantity, m.Notes)
		if err != nil {
			return fmt.Errorf("insert manifest item: %w", err)
		}
		m.ID, _ = result.LastInsertId()
	}

	for _, m := range updates {
		_, err := tx.Exec(db.Q(`UPDATE manifest_items SET part_number=?, quantity=? WHERE id=?`),
			m.PartNumber, m.Quantity, m.ID)
		if err != nil {
			return fmt.Errorf("update manifest item %d: %w", m.ID, err)
		}
	}

	for _, id := range deleteIDs {
		_, err := tx.Exec(db.Q(`DELETE FROM manifest_items WHERE id=?`), id)
		if err != nil {
			return fmt.Errorf("delete manifest item %d: %w", id, err)
		}
	}

	for _, c := range corrections {
		_, err := tx.Exec(db.Q(`INSERT INTO corrections (correction_type, node_id, payload_id, manifest_item_id, cat_id, description, quantity, reason, actor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			c.CorrectionType, c.NodeID, nullableInt64(c.PayloadID), nullableInt64(c.ManifestItemID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
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
		var createdAt any
		var payloadID, manifestItemID sql.NullInt64
		if err := rows.Scan(&c.ID, &c.CorrectionType, &c.NodeID, &payloadID, &manifestItemID, &c.CatID, &c.Description, &c.Quantity, &c.Reason, &c.Actor, &createdAt); err != nil {
			return nil, err
		}
		if payloadID.Valid {
			c.PayloadID = &payloadID.Int64
		}
		if manifestItemID.Valid {
			c.ManifestItemID = &manifestItemID.Int64
		}
		c.CreatedAt = parseTime(createdAt)
		corrections = append(corrections, &c)
	}
	return corrections, rows.Err()
}
