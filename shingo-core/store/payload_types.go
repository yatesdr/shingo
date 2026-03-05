package store

import (
	"database/sql"
	"fmt"
	"time"
)

type PayloadStyle struct {
	ID                  int64     `json:"id"`
	Name                string    `json:"name"`
	Code                string    `json:"code"`
	Description         string    `json:"description"`
	FormFactor          string    `json:"form_factor"`
	UOPCapacity         int       `json:"uop_capacity"`
	WidthMM             float64   `json:"width_mm"`
	HeightMM            float64   `json:"height_mm"`
	DepthMM             float64   `json:"depth_mm"`
	WeightKG            float64   `json:"weight_kg"`
	DefaultManifestJSON string    `json:"default_manifest_json"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

const payloadStyleSelectCols = `id, name, code, description, form_factor, uop_capacity, width_mm, height_mm, depth_mm, weight_kg, default_manifest_json, created_at, updated_at`

func scanPayloadStyle(row interface{ Scan(...any) error }) (*PayloadStyle, error) {
	var ps PayloadStyle
	var createdAt, updatedAt any
	err := row.Scan(&ps.ID, &ps.Name, &ps.Code, &ps.Description, &ps.FormFactor,
		&ps.UOPCapacity, &ps.WidthMM, &ps.HeightMM, &ps.DepthMM, &ps.WeightKG,
		&ps.DefaultManifestJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	ps.CreatedAt = parseTime(createdAt)
	ps.UpdatedAt = parseTime(updatedAt)
	return &ps, nil
}

func scanPayloadStyles(rows *sql.Rows) ([]*PayloadStyle, error) {
	var styles []*PayloadStyle
	for rows.Next() {
		ps, err := scanPayloadStyle(rows)
		if err != nil {
			return nil, err
		}
		styles = append(styles, ps)
	}
	return styles, rows.Err()
}

func (db *DB) CreatePayloadStyle(ps *PayloadStyle) error {
	result, err := db.Exec(db.Q(`INSERT INTO payload_styles (name, code, description, form_factor, uop_capacity, width_mm, height_mm, depth_mm, weight_kg, default_manifest_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		ps.Name, ps.Code, ps.Description, ps.FormFactor, ps.UOPCapacity,
		ps.WidthMM, ps.HeightMM, ps.DepthMM, ps.WeightKG, ps.DefaultManifestJSON)
	if err != nil {
		return fmt.Errorf("create payload style: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create payload style last id: %w", err)
	}
	ps.ID = id
	return nil
}

func (db *DB) UpdatePayloadStyle(ps *PayloadStyle) error {
	_, err := db.Exec(db.Q(`UPDATE payload_styles SET name=?, code=?, description=?, form_factor=?, uop_capacity=?, width_mm=?, height_mm=?, depth_mm=?, weight_kg=?, default_manifest_json=?, updated_at=datetime('now','localtime') WHERE id=?`),
		ps.Name, ps.Code, ps.Description, ps.FormFactor, ps.UOPCapacity,
		ps.WidthMM, ps.HeightMM, ps.DepthMM, ps.WeightKG, ps.DefaultManifestJSON, ps.ID)
	return err
}

func (db *DB) DeletePayloadStyle(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM payload_styles WHERE id=?`), id)
	return err
}

func (db *DB) GetPayloadStyle(id int64) (*PayloadStyle, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payload_styles WHERE id=?`, payloadStyleSelectCols)), id)
	return scanPayloadStyle(row)
}

func (db *DB) GetPayloadStyleByName(name string) (*PayloadStyle, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payload_styles WHERE name=?`, payloadStyleSelectCols)), name)
	return scanPayloadStyle(row)
}

func (db *DB) GetPayloadStyleByCode(code string) (*PayloadStyle, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM payload_styles WHERE code=?`, payloadStyleSelectCols)), code)
	return scanPayloadStyle(row)
}

func (db *DB) ListPayloadStyles() ([]*PayloadStyle, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM payload_styles ORDER BY name`, payloadStyleSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloadStyles(rows)
}
