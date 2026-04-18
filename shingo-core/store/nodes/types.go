package nodes

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
)

// NodeType is the node-type domain entity. The struct lives in
// shingocore/domain (Stage 2A); this alias keeps the nodes.NodeType
// name used by scanNodeType, CreateType, UpdateType, and the outer
// store/ node_types.go re-export.
type NodeType = domain.NodeType

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

// CreateType inserts a new node type and sets nt.ID on success.
func CreateType(db *sql.DB, nt *NodeType) error {
	id, err := insertID(db, `INSERT INTO node_types (code, name, description, is_synthetic) VALUES ($1, $2, $3, $4) RETURNING id`,
		nt.Code, nt.Name, nt.Description, nt.IsSynthetic)
	if err != nil {
		return fmt.Errorf("create node type: %w", err)
	}
	nt.ID = id
	return nil
}

// UpdateType writes the mutable columns on a node type.
func UpdateType(db *sql.DB, nt *NodeType) error {
	_, err := db.Exec(`UPDATE node_types SET code=$1, name=$2, description=$3, is_synthetic=$4, updated_at=NOW() WHERE id=$5`,
		nt.Code, nt.Name, nt.Description, nt.IsSynthetic, nt.ID)
	return err
}

// DeleteType removes a node type.
func DeleteType(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM node_types WHERE id=$1`, id)
	return err
}

// GetType fetches a node type by ID.
func GetType(db *sql.DB, id int64) (*NodeType, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM node_types WHERE id=$1`, nodeTypeSelectCols), id)
	return scanNodeType(row)
}

// GetTypeByCode fetches a node type by its unique code.
func GetTypeByCode(db *sql.DB, code string) (*NodeType, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s FROM node_types WHERE code=$1`, nodeTypeSelectCols), code)
	return scanNodeType(row)
}

// ListTypes returns all node types ordered by code.
func ListTypes(db *sql.DB) ([]*NodeType, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM node_types ORDER BY code`, nodeTypeSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeTypes(rows)
}
