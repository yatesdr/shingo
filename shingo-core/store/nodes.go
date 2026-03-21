package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Node struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	IsSynthetic bool      `json:"is_synthetic"`
	Zone        string    `json:"zone"`
	Enabled     bool      `json:"enabled"`
	Depth       *int      `json:"depth,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NodeTypeID  *int64    `json:"node_type_id,omitempty"`
	ParentID    *int64    `json:"parent_id,omitempty"`
	// Joined fields
	NodeTypeCode string `json:"node_type_code,omitempty"`
	NodeTypeName string `json:"node_type_name,omitempty"`
	ParentName   string `json:"parent_name,omitempty"`
}

const nodeSelectCols = `n.id, n.name, n.is_synthetic, n.zone, n.enabled, n.depth, n.created_at, n.updated_at, n.node_type_id, n.parent_id, COALESCE(nt.code, ''), COALESCE(nt.name, ''), COALESCE(pn.name, '')`

const nodeFromClause = `FROM nodes n LEFT JOIN node_types nt ON nt.id = n.node_type_id LEFT JOIN nodes pn ON pn.id = n.parent_id`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var depth sql.NullInt32
	var nodeTypeID, parentID sql.NullInt64
	err := row.Scan(&n.ID, &n.Name, &n.IsSynthetic, &n.Zone, &n.Enabled, &depth, &n.CreatedAt, &n.UpdatedAt,
		&nodeTypeID, &parentID, &n.NodeTypeCode, &n.NodeTypeName, &n.ParentName)
	if err != nil {
		return nil, err
	}
	if depth.Valid {
		d := int(depth.Int32)
		n.Depth = &d
	}
	if nodeTypeID.Valid {
		n.NodeTypeID = &nodeTypeID.Int64
	}
	if parentID.Valid {
		n.ParentID = &parentID.Int64
	}
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

func nullableInt(p *int) any {
	if p != nil {
		return *p
	}
	return nil
}

func (db *DB) CreateNode(n *Node) error {
	id, err := db.insertID(`INSERT INTO nodes (name, is_synthetic, zone, enabled, depth, node_type_id, parent_id) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		n.Name, n.IsSynthetic, n.Zone, n.Enabled, nullableInt(n.Depth), nullableInt64(n.NodeTypeID), nullableInt64(n.ParentID))
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	n.ID = id
	return nil
}

func (db *DB) UpdateNode(n *Node) error {
	_, err := db.Exec(`UPDATE nodes SET name=$1, is_synthetic=$2, zone=$3, enabled=$4, depth=$5, node_type_id=$6, parent_id=$7, updated_at=NOW() WHERE id=$8`,
		n.Name, n.IsSynthetic, n.Zone, n.Enabled, nullableInt(n.Depth), nullableInt64(n.NodeTypeID), nullableInt64(n.ParentID), n.ID)
	if err != nil {
		return fmt.Errorf("update node: %w", err)
	}
	return nil
}

func (db *DB) DeleteNode(id int64) error {
	_, err := db.Exec(`DELETE FROM nodes WHERE id=$1`, id)
	return err
}

func (db *DB) GetNode(id int64) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s WHERE n.id=$1`, nodeSelectCols, nodeFromClause), id)
	return scanNode(row)
}

func (db *DB) GetNodeByName(name string) (*Node, error) {
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s WHERE n.name=$1`, nodeSelectCols, nodeFromClause), name)
	return scanNode(row)
}

// GetNodeByDotName resolves a node name that may use dot-notation (PARENT.CHILD).
// If the name contains a dot, it looks up the child under the given parent.
// Otherwise it falls back to GetNodeByName.
func (db *DB) GetNodeByDotName(name string) (*Node, error) {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) != 2 {
		return db.GetNodeByName(name)
	}
	row := db.QueryRow(fmt.Sprintf(`SELECT %s %s WHERE n.name=$1 AND n.parent_id IN (SELECT id FROM nodes WHERE name=$2)`, nodeSelectCols, nodeFromClause), parts[1], parts[0])
	return scanNode(row)
}

func (db *DB) ListNodes() ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s ORDER BY n.name`, nodeSelectCols, nodeFromClause))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListChildNodes(parentID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s %s WHERE n.parent_id=$1 ORDER BY n.name`, nodeSelectCols, nodeFromClause), parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) SetNodeParent(nodeID, parentID int64) error {
	_, err := db.Exec(`UPDATE nodes SET parent_id=$1, updated_at=NOW() WHERE id=$2`, parentID, nodeID)
	return err
}

func (db *DB) ClearNodeParent(nodeID int64) error {
	_, err := db.Exec(`UPDATE nodes SET parent_id=NULL, updated_at=NOW() WHERE id=$1`, nodeID)
	return err
}

// ReparentNode moves a node into a new parent (or removes it from a parent).
// When adopting into a lane, it sets the depth based on position.
// When orphaning, it clears depth and role properties.
func (db *DB) ReparentNode(nodeID int64, parentID *int64, position int) error {
	if parentID == nil {
		if err := db.ClearNodeParent(nodeID); err != nil {
			return err
		}
		db.Exec(`UPDATE nodes SET depth=NULL WHERE id=$1`, nodeID)
		db.DeleteNodeProperty(nodeID, "role")
		return nil
	}
	if err := db.SetNodeParent(nodeID, *parentID); err != nil {
		return err
	}
	if position > 0 {
		db.Exec(`UPDATE nodes SET depth=$1 WHERE id=$2`, position, nodeID)
	}
	return nil
}

// ReorderLaneSlots updates depth for all slots in a lane based on
// the provided ordered list of node IDs.
func (db *DB) ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error {
	for i, nid := range orderedNodeIDs {
		depth := i + 1
		if _, err := db.Exec(`UPDATE nodes SET depth=$1 WHERE id=$2`, depth, nid); err != nil {
			return err
		}
	}
	return nil
}
