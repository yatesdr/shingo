// Package payloads holds payload-aggregate persistence for shingo-core.
//
// Stage 2D of the architecture plan moved payload CRUD + manifest
// templates out of the flat store/ package and into this sub-package.
// The outer store/ keeps type aliases (`store.Payload = payloads.Payload`,
// etc.) and one-line delegate methods on *store.DB so callers see no
// public API change. Cross-aggregate methods (those that span payloads
// and bins/nodes) stay at the outer store/ level.
package payloads

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// Payload is the payload-template domain entity. The struct lives in
// shingocore/domain (Stage 2A); this alias keeps the payloads.Payload
// name used by ScanPayload, Create/Update, and the outer store/
// payloads.go re-export (store.Payload).
type Payload = domain.Payload

// SelectCols is exported so cross-aggregate readers (e.g. ListPayloadsForNode
// at the outer store/ level) can reuse the column list.
const SelectCols = `id, code, description, uop_capacity, created_at, updated_at`

// ScanPayload reads a single payloads row. Exported for cross-aggregate
// readers at the outer store/ level.
func ScanPayload(row interface{ Scan(...any) error }) (*Payload, error) {
	var p Payload
	err := row.Scan(&p.ID, &p.Code, &p.Description,
		&p.UOPCapacity, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ScanPayloads reads all payloads rows from a *sql.Rows.
func ScanPayloads(rows *sql.Rows) ([]*Payload, error) {
	var payloads []*Payload
	for rows.Next() {
		p, err := ScanPayload(rows)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

// Create inserts a new payload template and sets p.ID on success.
func Create(db *sql.DB, p *Payload) error {
	id, err := helpers.InsertID(db, `INSERT INTO payloads (code, description, uop_capacity) VALUES ($1, $2, $3) RETURNING id`,
		p.Code, p.Description, p.UOPCapacity)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	p.ID = id
	return nil
}

// Update writes all payload columns by primary key.
func Update(db *sql.DB, p *Payload) error {
	_, err := db.Exec(`UPDATE payloads SET code=$1, description=$2, uop_capacity=$3, updated_at=NOW() WHERE id=$4`,
		p.Code, p.Description, p.UOPCapacity, p.ID)
	return err
}

// Delete removes a payload template.
func Delete(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM payloads WHERE id=$1`, id)
	return err
}

// Get fetches a payload by ID.
func Get(db *sql.DB, id int64) (*Payload, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM payloads WHERE id=$1`, SelectCols), id)
	return ScanPayload(row)
}

// GetByCode fetches a payload by its unique code.
func GetByCode(db *sql.DB, code string) (*Payload, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM payloads WHERE code=$1`, SelectCols), code)
	return ScanPayload(row)
}

// List returns every payload ordered by code.
func List(db *sql.DB) ([]*Payload, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM payloads ORDER BY code`, SelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return ScanPayloads(rows)
}

// SetBinTypes replaces all bin type associations for a payload template.
// Runs as a single transaction.
func SetBinTypes(db *sql.DB, payloadID int64, binTypeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM payload_bin_types WHERE payload_id=$1`, payloadID); err != nil {
		return err
	}
	for _, btID := range binTypeIDs {
		if _, err := tx.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1, $2)`, payloadID, btID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
