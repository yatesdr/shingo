package store

import (
	"database/sql"
	"fmt"
	"time"
)

type PayloadType struct {
	ID                 int64
	Name               string
	Description        string
	FormFactor         string
	DefaultManifestJSON string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

const payloadTypeSelectCols = `id, name, description, form_factor, default_manifest_json, created_at, updated_at`

func scanPayloadType(row interface{ Scan(...any) error }) (*PayloadType, error) {
	var pt PayloadType
	var createdAt, updatedAt string
	err := row.Scan(&pt.ID, &pt.Name, &pt.Description, &pt.FormFactor, &pt.DefaultManifestJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	pt.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	pt.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &pt, nil
}

func scanPayloadTypes(rows *sql.Rows) ([]*PayloadType, error) {
	var types []*PayloadType
	for rows.Next() {
		pt, err := scanPayloadType(rows)
		if err != nil {
			return nil, err
		}
		types = append(types, pt)
	}
	return types, rows.Err()
}

func (db *DB) CreatePayloadType(pt *PayloadType) error {
	result, err := db.Exec(db.Q(`INSERT INTO payload_types (name, description, form_factor, default_manifest_json) VALUES (?, ?, ?, ?)`),
		pt.Name, pt.Description, pt.FormFactor, pt.DefaultManifestJSON)
	if err != nil {
		return fmt.Errorf("create payload type: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create payload type last id: %w", err)
	}
	pt.ID = id
	return nil
}

func (db *DB) UpdatePayloadType(pt *PayloadType) error {
	_, err := db.Exec(db.Q(`UPDATE payload_types SET name=?, description=?, form_factor=?, default_manifest_json=?, updated_at=datetime('now','localtime') WHERE id=?`),
		pt.Name, pt.Description, pt.FormFactor, pt.DefaultManifestJSON, pt.ID)
	return err
}

func (db *DB) DeletePayloadType(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payload_types WHERE id=?`), id)
	return err
}

func (db *DB) GetPayloadType(id int64) (*PayloadType, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payload_types WHERE id=?`, payloadTypeSelectCols)), id)
	return scanPayloadType(row)
}

func (db *DB) GetPayloadTypeByName(name string) (*PayloadType, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payload_types WHERE name=?`, payloadTypeSelectCols)), name)
	return scanPayloadType(row)
}

func (db *DB) ListPayloadTypes() ([]*PayloadType, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM payload_types ORDER BY name`, payloadTypeSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloadTypes(rows)
}
