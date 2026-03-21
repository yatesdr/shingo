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
	BinID          *int64    `json:"bin_id,omitempty"`
	CatID          string    `json:"cat_id"`
	Description    string    `json:"description"`
	Quantity       int64     `json:"quantity"`
	Reason         string    `json:"reason"`
	Actor          string    `json:"actor"`
	CreatedAt      time.Time `json:"created_at"`
}

func (db *DB) CreateCorrection(c *Correction) error {
	id, err := db.insertID(`INSERT INTO corrections (correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		c.CorrectionType, c.NodeID, nullableInt64(c.BinID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

func (db *DB) ListCorrections(limit int) ([]*Correction, error) {
	rows, err := db.Query(`SELECT id, correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor, created_at FROM corrections ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

func (db *DB) ListCorrectionsByNode(nodeID int64, limit int) ([]*Correction, error) {
	rows, err := db.Query(`SELECT id, correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor, created_at FROM corrections WHERE node_id = $1 ORDER BY id DESC LIMIT $2`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCorrections(rows)
}

// ApplyBinManifestChanges applies corrections to a bin's manifest and records correction rows.
func (db *DB) ApplyBinManifestChanges(binID int64, corrections []*Correction) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, c := range corrections {
		_, err := tx.Exec(`INSERT INTO corrections (correction_type, node_id, bin_id, cat_id, description, quantity, reason, actor) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			c.CorrectionType, c.NodeID, nullableInt64(c.BinID), c.CatID, c.Description, c.Quantity, c.Reason, c.Actor)
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
