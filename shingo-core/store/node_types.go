package store

import (
	"database/sql"
	"fmt"
	"time"
)

type NodeType struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsSynthetic bool      `json:"is_synthetic"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const nodeTypeSelectCols = `id, code, name, description, is_synthetic, created_at, updated_at`

func scanNodeType(row interface{ Scan(...any) error }) (*NodeType, error) {
	var nt NodeType
	var isSynthetic int
	var createdAt, updatedAt any
	err := row.Scan(&nt.ID, &nt.Code, &nt.Name, &nt.Description, &isSynthetic, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	nt.IsSynthetic = isSynthetic != 0
	nt.CreatedAt = parseTime(createdAt)
	nt.UpdatedAt = parseTime(updatedAt)
	return &nt, nil
}

func scanNodeTypes(rows *sql.Rows) ([]*NodeType, error) {
	var types []*NodeType
	for rows.Next() {
		nt, err := scanNodeType(rows)
		if err != nil {
			return nil, err
		}
		types = append(types, nt)
	}
	return types, rows.Err()
}

func (db *DB) CreateNodeType(nt *NodeType) error {
	id, err := db.insertID(`INSERT INTO node_types (code, name, description, is_synthetic) VALUES (?, ?, ?, ?) RETURNING id`,
		nt.Code, nt.Name, nt.Description, boolToInt(nt.IsSynthetic))
	if err != nil {
		return fmt.Errorf("create node type: %w", err)
	}
	nt.ID = id
	return nil
}

func (db *DB) UpdateNodeType(nt *NodeType) error {
	_, err := db.Exec(db.Q(`UPDATE node_types SET code=?, name=?, description=?, is_synthetic=?, updated_at=datetime('now') WHERE id=?`),
		nt.Code, nt.Name, nt.Description, boolToInt(nt.IsSynthetic), nt.ID)
	return err
}

func (db *DB) DeleteNodeType(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM node_types WHERE id=?`), id)
	return err
}

func (db *DB) GetNodeType(id int64) (*NodeType, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM node_types WHERE id=?`, nodeTypeSelectCols)), id)
	return scanNodeType(row)
}

func (db *DB) GetNodeTypeByCode(code string) (*NodeType, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM node_types WHERE code=?`, nodeTypeSelectCols)), code)
	return scanNodeType(row)
}

func (db *DB) ListNodeTypes() ([]*NodeType, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM node_types ORDER BY code`, nodeTypeSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeTypes(rows)
}
