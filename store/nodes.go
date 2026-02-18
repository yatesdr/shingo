package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Node struct {
	ID          int64
	Name        string
	RDSLocation string
	NodeType    string
	Zone        string
	Capacity    int
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const nodeSelectCols = `id, name, rds_location, node_type, zone, capacity, enabled, created_at, updated_at`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var enabled int
	var createdAt, updatedAt string
	err := row.Scan(&n.ID, &n.Name, &n.RDSLocation, &n.NodeType, &n.Zone, &n.Capacity, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	n.Enabled = enabled != 0
	n.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	n.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return &n, nil
}

func scanNodes(rows *sql.Rows) ([]*Node, error) {
	var nodes []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (db *DB) CreateNode(n *Node) error {
	result, err := db.Exec(db.Q(`INSERT INTO nodes (name, rds_location, node_type, zone, capacity, enabled) VALUES (?, ?, ?, ?, ?, ?)`),
		n.Name, n.RDSLocation, n.NodeType, n.Zone, n.Capacity, boolToInt(n.Enabled))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("create node last id: %w", err)
	}
	n.ID = id
	return nil
}

func (db *DB) UpdateNode(n *Node) error {
	_, err := db.Exec(db.Q(`UPDATE nodes SET name=?, rds_location=?, node_type=?, zone=?, capacity=?, enabled=?, updated_at=datetime('now','localtime') WHERE id=?`),
		n.Name, n.RDSLocation, n.NodeType, n.Zone, n.Capacity, boolToInt(n.Enabled), n.ID)
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	return nil
}

func (db *DB) DeleteNode(id int64) error {
	_, err := db.Exec(db.Q(`DELETE FROM nodes WHERE id=?`), id)
	return err
}

func (db *DB) GetNode(id int64) (*Node, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM nodes WHERE id=?`, nodeSelectCols)), id)
	return scanNode(row)
}

func (db *DB) GetNodeByName(name string) (*Node, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s FROM nodes WHERE name=?`, nodeSelectCols)), name)
	return scanNode(row)
}

func (db *DB) ListNodes() ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM nodes ORDER BY name`, nodeSelectCols))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListNodesByType(nodeType string) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM nodes WHERE node_type=? ORDER BY name`, nodeSelectCols)), nodeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListEnabledStorageNodes() ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM nodes WHERE node_type='storage' AND enabled=1 ORDER BY name`, nodeSelectCols)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
