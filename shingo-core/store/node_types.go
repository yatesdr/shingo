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
	err := row.Scan(&nt.ID, &nt.Code, &nt.Name, &nt.Description, &nt.IsSynthetic, &nt.CreatedAt, &nt.UpdatedAt)
	if err != nil {
		return nil, err
	}
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
	id, err := db.insertID(`INSERT INTO node_types (code, name, description, is_synthetic) VALUES ($1, $2, $3, $4) RETURNING id`,
		nt.Code, nt.Name, nt.Description, nt.IsSynthetic)
	if err != nil {
		return fmt.Errorf("create node type: %w", err)
	}
	nt.ID = id
	return nil
}

func (db *DB) UpdateNodeType(nt *NodeType) error {
	_, err := db.Exec(`UPDATE node_types SET code=$1, name=$2, description=$3, is_synthetic=$4, updated_at=NOW() WHERE id=$5`,
		nt.Code, nt.Name, nt.Description, nt.IsSynthetic, nt.ID)
	return err
}

func (db *DB) DeleteNodeType(id int64) error {
	_, err := db.Exec(`DELETE FROM node_types WHERE id=$1`, id)
	return err
}

func (db *DB) GetNodeType(id int64) (*NodeType, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM node_types WHERE id=$1`, nodeTypeSelectCols), id)
	return scanNodeType(row)
}

func (db *DB) GetNodeTypeByCode(code string) (*NodeType, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM node_types WHERE code=$1`, nodeTypeSelectCols), code)
	return scanNodeType(row)
}

func (db *DB) ListNodeTypes() ([]*NodeType, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM node_types ORDER BY code`, nodeTypeSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeTypes(rows)
}
