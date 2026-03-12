package store

import (
	"database/sql"
	"fmt"
	"time"
)

type BinType struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Description string    `json:"description"`
	WidthIn     float64   `json:"width_in"`
	HeightIn    float64   `json:"height_in"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const binTypeSelectCols = `id, code, description, width_in, height_in, created_at, updated_at`

func scanBinType(row interface{ Scan(...any) error }) (*BinType, error) {
	var bt BinType
	var createdAt, updatedAt any
	err := row.Scan(&bt.ID, &bt.Code, &bt.Description, &bt.WidthIn, &bt.HeightIn, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	bt.CreatedAt = parseTime(createdAt)
	bt.UpdatedAt = parseTime(updatedAt)
	return &bt, nil
}

func scanBinTypes(rows *sql.Rows) ([]*BinType, error) {
	var types []*BinType
	for rows.Next() {
		bt, err := scanBinType(rows)
		if err != nil {
			return nil, err
		}
		types = append(types, bt)
	}
	return types, rows.Err()
}

func (db *DB) CreateBinType(bt *BinType) error {
	id, err := db.insertID(`INSERT INTO bin_types (code, description, width_in, height_in) VALUES (?, ?, ?, ?) RETURNING id`,
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn)
	if err != nil {
		return fmt.Errorf("create bin type: %w", err)
	}
	bt.ID = id
	return nil
}

func (db *DB) UpdateBinType(bt *BinType) error {
	_, err := db.Exec(db.Q(`UPDATE bin_types SET code=?, description=?, width_in=?, height_in=?, updated_at=datetime('now') WHERE id=?`),
		bt.Code, bt.Description, bt.WidthIn, bt.HeightIn, bt.ID)
	return err
}

func (db *DB) DeleteBinType(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM bin_types WHERE id=?`), id)
	return err
}

func (db *DB) GetBinType(id int64) (*BinType, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM bin_types WHERE id=?`, binTypeSelectCols)), id)
	return scanBinType(row)
}

func (db *DB) GetBinTypeByCode(code string) (*BinType, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM bin_types WHERE code=?`, binTypeSelectCols)), code)
	return scanBinType(row)
}

func (db *DB) ListBinTypes() ([]*BinType, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM bin_types ORDER BY code`, binTypeSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBinTypes(rows)
}
