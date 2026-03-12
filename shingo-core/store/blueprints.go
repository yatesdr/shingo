package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Payload represents a payload template defining bin contents and UOP capacity.
type Payload struct {
	ID                  int64     `json:"id"`
	Code                string    `json:"code"`
	Description         string    `json:"description"`
	UOPCapacity         int       `json:"uop_capacity"`
	DefaultManifestJSON string    `json:"default_manifest_json"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

const payloadSelectCols = `id, code, description, uop_capacity, default_manifest_json, created_at, updated_at`

func scanPayload(row interface{ Scan(...any) error }) (*Payload, error) {
	var p Payload
	var createdAt, updatedAt any
	err := row.Scan(&p.ID, &p.Code, &p.Description,
		&p.UOPCapacity, &p.DefaultManifestJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = parseTime(createdAt)
	p.UpdatedAt = parseTime(updatedAt)
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
	id, err := db.insertID(`INSERT INTO payloads (code, description, uop_capacity, default_manifest_json) VALUES (?, ?, ?, ?) RETURNING id`,
		p.Code, p.Description, p.UOPCapacity, p.DefaultManifestJSON)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	p.ID = id
	return nil
}

func (db *DB) UpdatePayload(p *Payload) error {
	_, err := db.Exec(db.Q(`UPDATE payloads SET code=?, description=?, uop_capacity=?, default_manifest_json=?, updated_at=datetime('now') WHERE id=?`),
		p.Code, p.Description, p.UOPCapacity, p.DefaultManifestJSON, p.ID)
	return err
}

func (db *DB) DeletePayload(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payloads WHERE id=?`), id)
	return err
}

func (db *DB) GetPayload(id int64) (*Payload, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payloads WHERE id=?`, payloadSelectCols)), id)
	return scanPayload(row)
}

func (db *DB) GetPayloadByCode(code string) (*Payload, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payloads WHERE code=?`, payloadSelectCols)), code)
	return scanPayload(row)
}

func (db *DB) ListPayloads() ([]*Payload, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM payloads ORDER BY code`, payloadSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

// ListBinTypesForPayload returns all bin types associated with a payload template.
func (db *DB) ListBinTypesForPayload(payloadID int64) ([]*BinType, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM bin_types WHERE id IN (SELECT bin_type_id FROM payload_bin_types WHERE payload_id=?) ORDER BY code`, binTypeSelectCols)), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBinTypes(rows)
}

// SetPayloadBinTypes replaces all bin type associations for a payload template.
func (db *DB) SetPayloadBinTypes(payloadID int64, binTypeIDs []int64) error {
	if _, err := db.Exec(db.Q(`DELETE FROM payload_bin_types WHERE payload_id=?`), payloadID); err != nil {
		return err
	}
	for _, btID := range binTypeIDs {
		if _, err := db.Exec(db.Q(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES (?, ?)`), payloadID, btID); err != nil {
			return err
		}
	}
	return nil
}

