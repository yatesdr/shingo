package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Material struct {
	ID          int64
	Code        string
	Description string
	Unit        string
	CreatedAt   time.Time
}

const materialSelectCols = `id, code, description, unit, created_at`

func scanMaterial(row interface{ Scan(...any) error }) (*Material, error) {
	var m Material
	var createdAt string
	err := row.Scan(&m.ID, &m.Code, &m.Description, &m.Unit, &createdAt)
	if err != nil {
		return nil, err
	}
	m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return &m, nil
}

func scanMaterials(rows *sql.Rows) ([]*Material, error) {
	var materials []*Material
	for rows.Next() {
		m, err := scanMaterial(rows)
		if err != nil {
			return nil, err
		}
		materials = append(materials, m)
	}
	return materials, rows.Err()
}

func (db *DB) CreateMaterial(m *Material) error {
	result, err := db.Exec(db.Q(`INSERT INTO materials (code, description, unit) VALUES (?, ?, ?)`),
		m.Code, m.Description, m.Unit)
	if err != nil {
		return fmt.Errorf("create material: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create material last id: %w", err)
	}
	m.ID = id
	return nil
}

func (db *DB) UpdateMaterial(m *Material) error {
	_, err := db.Exec(db.Q(`UPDATE materials SET code=?, description=?, unit=? WHERE id=?`),
		m.Code, m.Description, m.Unit, m.ID)
	return err
}

func (db *DB) DeleteMaterial(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM materials WHERE id=?`), id)
	return err
}

func (db *DB) GetMaterial(id int64) (*Material, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM materials WHERE id=?`, materialSelectCols)), id)
	return scanMaterial(row)
}

func (db *DB) GetMaterialByCode(code string) (*Material, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM materials WHERE code=?`, materialSelectCols)), code)
	return scanMaterial(row)
}

func (db *DB) ListMaterials() ([]*Material, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM materials ORDER BY code`, materialSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMaterials(rows)
}
