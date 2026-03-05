package store

import (
	"time"
)

type NodeProperty struct {
	ID        int64     `json:"id"`
	NodeID    int64     `json:"node_id"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	CreatedAt time.Time `json:"created_at"`
}

// SetNodeProperty upserts a key-value property on a node.
func (db *DB) SetNodeProperty(nodeID int64, key, value string) error {
	_, err := db.Exec(db.Q(`INSERT INTO node_properties (node_id, key, value) VALUES (?, ?, ?) ON CONFLICT (node_id, key) DO UPDATE SET value=?`),
		nodeID, key, value, value)
	return err
}

func (db *DB) DeleteNodeProperty(nodeID int64, key string) error {
	_, err := db.Exec(db.Q(`DELETE FROM node_properties WHERE node_id=? AND key=?`), nodeID, key)
	return err
}

func (db *DB) ListNodeProperties(nodeID int64) ([]*NodeProperty, error) {
	rows, err := db.Query(db.Q(`SELECT id, node_id, key, value, created_at FROM node_properties WHERE node_id=? ORDER BY key`), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var props []*NodeProperty
	for rows.Next() {
		var p NodeProperty
		var createdAt any
		if err := rows.Scan(&p.ID, &p.NodeID, &p.Key, &p.Value, &createdAt); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTime(createdAt)
		props = append(props, &p)
	}
	return props, rows.Err()
}

// GetEffectiveProperties returns properties for a node, merging parent properties
// (node's own values override parent's).
func (db *DB) GetEffectiveProperties(nodeID int64) (map[string]string, error) {
	node, err := db.GetNode(nodeID)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)

	// Load parent properties first if node has a parent
	if node.ParentID != nil {
		parentProps, err := db.ListNodeProperties(*node.ParentID)
		if err == nil {
			for _, p := range parentProps {
				result[p.Key] = p.Value
			}
		}
	}

	// Override with node's own properties
	props, err := db.ListNodeProperties(nodeID)
	if err != nil {
		return result, err
	}
	for _, p := range props {
		result[p.Key] = p.Value
	}

	return result, nil
}
