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
	_, err := db.Exec(`INSERT INTO node_properties (node_id, key, value) VALUES ($1, $2, $3) ON CONFLICT (node_id, key) DO UPDATE SET value=$4`,
		nodeID, key, value, value)
	return err
}

func (db *DB) DeleteNodeProperty(nodeID int64, key string) error {
	_, err := db.Exec(`DELETE FROM node_properties WHERE node_id=$1 AND key=$2`, nodeID, key)
	return err
}

func (db *DB) ListNodeProperties(nodeID int64) ([]*NodeProperty, error) {
	rows, err := db.Query(`SELECT id, node_id, key, value, created_at FROM node_properties WHERE node_id=$1 ORDER BY key`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var props []*NodeProperty
	for rows.Next() {
		var p NodeProperty
		if err := rows.Scan(&p.ID, &p.NodeID, &p.Key, &p.Value, &p.CreatedAt); err != nil {
			return nil, err
		}
		props = append(props, &p)
	}
	return props, rows.Err()
}

// GetNodeProperty returns a single property value for a node, or empty string if not set.
func (db *DB) GetNodeProperty(nodeID int64, key string) string {
	var value string
	err := db.QueryRow(`SELECT value FROM node_properties WHERE node_id=$1 AND key=$2`, nodeID, key).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}
