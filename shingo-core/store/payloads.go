package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Payload represents a payload template defining bin contents and UOP capacity.
type Payload struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	UOPCapacity int       `json:"uop_capacity"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const payloadSelectCols = `id, code, description, uop_capacity, created_at, updated_at`

func scanPayload(row interface{ Scan(...any) error }) (*Payload, error) {
	var p Payload
	err := row.Scan(&p.ID, &p.Code, &p.Description,
		&p.UOPCapacity, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func scanPayloads(rows *sql.Rows) ([]*Payload, error) {
	var payloads []*Payload
	for rows.Next() {
		p, err := scanPayload(rows)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, p)
	}
	return payloads, rows.Err()
}

func (db *DB) CreatePayload(p *Payload) error {
	id, err := db.insertID(`INSERT INTO payloads (code, description, uop_capacity) VALUES ($1, $2, $3) RETURNING id`,
		p.Code, p.Description, p.UOPCapacity)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	p.ID = id
	return nil
}

func (db *DB) UpdatePayload(p *Payload) error {
	_, err := db.Exec(`UPDATE payloads SET code=$1, description=$2, uop_capacity=$3, updated_at=NOW() WHERE id=$4`,
		p.Code, p.Description, p.UOPCapacity, p.ID)
	return err
}

func (db *DB) DeletePayload(id int64) error {
	_, err := db.Exec(`DELETE FROM payloads WHERE id=$1`, id)
	return err
}

func (db *DB) GetPayload(id int64) (*Payload, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM payloads WHERE id=$1`, payloadSelectCols), id)
	return scanPayload(row)
}

func (db *DB) GetPayloadByCode(code string) (*Payload, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM payloads WHERE code=$1`, payloadSelectCols), code)
	return scanPayload(row)
}

func (db *DB) ListPayloads() ([]*Payload, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM payloads ORDER BY code`, payloadSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

// ListBinTypesForPayload returns all bin types associated with a payload template.
func (db *DB) ListBinTypesForPayload(payloadID int64) ([]*BinType, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM bin_types WHERE id IN (SELECT bin_type_id FROM payload_bin_types WHERE payload_id=$1) ORDER BY code`, binTypeSelectCols), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBinTypes(rows)
}

// SetPayloadBinTypes replaces all bin type associations for a payload template.
func (db *DB) SetPayloadBinTypes(payloadID int64, binTypeIDs []int64) error {
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
