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
const SelectCols = `id, code, description, uop_capacity, robot_group, advanced_load_sequence, created_at, updated_at`

// ScanPayload reads a single payloads row. Exported for cross-aggregate
// readers at the outer store/ level.
func ScanPayload(row interface{ Scan(...any) error }) (*Payload, error) {
	var p Payload
	err := row.Scan(&p.ID, &p.Code, &p.Description,
		&p.UOPCapacity, &p.RobotGroup, &p.AdvancedLoadSequence, &p.CreatedAt, &p.UpdatedAt)
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

// PayloadCATIDs returns each payload's part identity (its single distinct
// payload_manifest part number) keyed by payload id — but ONLY for payloads
// whose manifest carries exactly one distinct part number. Payloads with zero
// or several distinct part numbers are omitted, so the edge auto-fill never
// guesses which part id a multi-part payload "is".
func PayloadCATIDs(db *sql.DB) (map[int64]string, error) {
	rows, err := db.Query(`SELECT payload_id, MIN(part_number), COUNT(DISTINCT part_number)
		FROM payload_manifest WHERE part_number != ''
		GROUP BY payload_id`)
	if err != nil {
		return nil, fmt.Errorf("payload catids: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var payloadID int64
		var partNumber string
		var distinct int
		if err := rows.Scan(&payloadID, &partNumber, &distinct); err != nil {
			return nil, fmt.Errorf("scan payload catid: %w", err)
		}
		if distinct == 1 {
			out[payloadID] = partNumber
		}
	}
	return out, rows.Err()
}

// DescriptionsByCode returns a payload_code → description map for every payload
// template. Used by the inventory Replenishment Health rollup to show a catalog
// description beside each payload code.
func DescriptionsByCode(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT code, COALESCE(description, '') FROM payloads`)
	if err != nil {
		return nil, fmt.Errorf("payload descriptions: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var code, desc string
		if err := rows.Scan(&code, &desc); err != nil {
			return nil, fmt.Errorf("scan payload description: %w", err)
		}
		out[code] = desc
	}
	return out, rows.Err()
}

// Create inserts a new payload template and sets p.ID on success.
func Create(db *sql.DB, p *Payload) error {
	id, err := helpers.InsertID(db, `INSERT INTO payloads (code, description, uop_capacity, robot_group, advanced_load_sequence) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		p.Code, p.Description, p.UOPCapacity, p.RobotGroup, p.AdvancedLoadSequence)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	p.ID = id
	return nil
}

// Update writes all payload columns by primary key.
func Update(db *sql.DB, p *Payload) error {
	_, err := db.Exec(`UPDATE payloads SET code=$1, description=$2, uop_capacity=$3, robot_group=$4, advanced_load_sequence=$5, updated_at=NOW() WHERE id=$6`,
		p.Code, p.Description, p.UOPCapacity, p.RobotGroup, p.AdvancedLoadSequence, p.ID)
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
// Runs as a single transaction. The INSERT is ON CONFLICT DO NOTHING: two
// concurrent SetBinTypes calls for the same payload can interleave their
// DELETE/INSERT phases, and without it one transaction's INSERT trips over
// the other's committed rows on payload_bin_types_pkey and the loser returns
// an error for a write whose end state is exactly what it asked for.
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
		if _, err := tx.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1, $2)
			ON CONFLICT (payload_id, bin_type_id) DO NOTHING`, payloadID, btID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
