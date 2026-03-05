package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Node struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	NodeType    string    `json:"node_type"`
	IsSynthetic bool      `json:"is_synthetic"`
	Zone        string    `json:"zone"`
	Capacity    int       `json:"capacity"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NodeTypeID  *int64    `json:"node_type_id,omitempty"`
	ParentID    *int64    `json:"parent_id,omitempty"`
	// Joined fields
	NodeTypeCode string `json:"node_type_code,omitempty"`
	NodeTypeName string `json:"node_type_name,omitempty"`
	ParentName   string `json:"parent_name,omitempty"`
}

const nodeSelectCols = `n.id, n.name, n.node_type, n.is_synthetic, n.zone, n.capacity, n.enabled, n.created_at, n.updated_at, n.node_type_id, n.parent_id, COALESCE(nt.code, ''), COALESCE(nt.name, ''), COALESCE(pn.name, '')`

const nodeFromClause = `FROM nodes n LEFT JOIN node_types nt ON nt.id = n.node_type_id LEFT JOIN nodes pn ON pn.id = n.parent_id`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var enabled, isSynthetic int
	var createdAt, updatedAt any
	var nodeTypeID, parentID sql.NullInt64
	err := row.Scan(&n.ID, &n.Name, &n.NodeType, &isSynthetic, &n.Zone, &n.Capacity, &enabled, &createdAt, &updatedAt,
		&nodeTypeID, &parentID, &n.NodeTypeCode, &n.NodeTypeName, &n.ParentName)
	if err != nil {
		return nil, err
	}
	n.Enabled = enabled != 0
	n.IsSynthetic = isSynthetic != 0
	n.CreatedAt = parseTime(createdAt)
	n.UpdatedAt = parseTime(updatedAt)
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

func (db *DB) CreateNode(n *Node) error {
	result, err := db.Exec(db.Q(`INSERT INTO nodes (name, node_type, is_synthetic, zone, capacity, enabled, node_type_id, parent_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		n.Name, n.NodeType, boolToInt(n.IsSynthetic), n.Zone, n.Capacity, boolToInt(n.Enabled), nullableInt64(n.NodeTypeID), nullableInt64(n.ParentID))
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
	_, err := db.Exec(db.Q(`UPDATE nodes SET name=?, node_type=?, is_synthetic=?, zone=?, capacity=?, enabled=?, node_type_id=?, parent_id=?, updated_at=datetime('now','localtime') WHERE id=?`),
		n.Name, n.NodeType, boolToInt(n.IsSynthetic), n.Zone, n.Capacity, boolToInt(n.Enabled), nullableInt64(n.NodeTypeID), nullableInt64(n.ParentID), n.ID)
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
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.id=?`, nodeSelectCols, nodeFromClause)), id)
	return scanNode(row)
}

func (db *DB) GetNodeByName(name string) (*Node, error) {
	row := db.QueryRow(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.name=?`, nodeSelectCols, nodeFromClause)), name)
	return scanNode(row)
}

func (db *DB) ListNodes() ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s ORDER BY n.name`, nodeSelectCols, nodeFromClause)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListNodesByType(nodeType string) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.node_type=? ORDER BY n.name`, nodeSelectCols, nodeFromClause)), nodeType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListNodesByTypeID(nodeTypeID int64) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.node_type_id=? ORDER BY n.name`, nodeSelectCols, nodeFromClause)), nodeTypeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListEnabledStorageNodes() ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.node_type='storage' AND n.enabled=1 AND n.is_synthetic=0 ORDER BY n.name`, nodeSelectCols, nodeFromClause)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListChildNodes(parentID int64) ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.parent_id=? ORDER BY n.name`, nodeSelectCols, nodeFromClause)), parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) SetNodeParent(nodeID, parentID int64) error {
	_, err := db.Exec(db.Q(`UPDATE nodes SET parent_id=?, updated_at=datetime('now','localtime') WHERE id=?`), parentID, nodeID)
	return err
}

func (db *DB) ClearNodeParent(nodeID int64) error {
	_, err := db.Exec(db.Q(`UPDATE nodes SET parent_id=NULL, updated_at=datetime('now','localtime') WHERE id=?`), nodeID)
	return err
}

func (db *DB) ListSyntheticNodes() ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.is_synthetic=1 ORDER BY n.name`, nodeSelectCols, nodeFromClause)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (db *DB) ListOrphanPhysicalNodes() ([]*Node, error) {
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s %s WHERE n.parent_id IS NULL AND n.is_synthetic=0 ORDER BY n.name`, nodeSelectCols, nodeFromClause)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ReparentNode moves a node into a new parent (or removes it from a parent).
// When adopting into a lane, it sets the depth property based on position.
// When orphaning, it clears depth and role properties.
func (db *DB) ReparentNode(nodeID int64, parentID *int64, position int) error {
	if parentID == nil {
		if err := db.ClearNodeParent(nodeID); err != nil {
			return err
		}
		db.DeleteNodeProperty(nodeID, "depth")
		db.DeleteNodeProperty(nodeID, "role")
		return nil
	}
	if err := db.SetNodeParent(nodeID, *parentID); err != nil {
		return err
	}
	if position > 0 {
		db.SetNodeProperty(nodeID, "depth", fmt.Sprintf("%d", position))
	}
	return nil
}

// ReorderLaneSlots updates depth properties for all slots in a lane based on
// the provided ordered list of node IDs.
func (db *DB) ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error {
	for i, nid := range orderedNodeIDs {
		depth := i + 1
		if err := db.SetNodeProperty(nid, "depth", fmt.Sprintf("%d", depth)); err != nil {
			return err
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
