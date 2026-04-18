package nodes

import (
	"database/sql"

	"shingocore/domain"
)

// Property is the node-property domain entity. The struct lives in
// shingocore/domain as NodeProperty (Stage 2A); this alias keeps the
// nodes.Property name used by SetProperty/ListProperties and the
// outer store/ node_properties.go re-export (store.NodeProperty).
type Property = domain.NodeProperty

// SetProperty upserts a key-value property on a node.
func SetProperty(db *sql.DB, nodeID int64, key, value string) error {
	_, err := db.Exec(`INSERT INTO node_properties (node_id, key, value) VALUES ($1, $2, $3) ON CONFLICT (node_id, key) DO UPDATE SET value=$4`,
		nodeID, key, value, value)
	return err
}

// DeleteProperty removes a property from a node.
func DeleteProperty(db *sql.DB, nodeID int64, key string) error {
	_, err := db.Exec(`DELETE FROM node_properties WHERE node_id=$1 AND key=$2`, nodeID, key)
	return err
}

// ListProperties returns all properties for a node ordered by key.
func ListProperties(db *sql.DB, nodeID int64) ([]*Property, error) {
	rows, err := db.Query(`SELECT id, node_id, key, value, created_at FROM node_properties WHERE node_id=$1 ORDER BY key`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var props []*Property
	for rows.Next() {
		var p Property
		if err := rows.Scan(&p.ID, &p.NodeID, &p.Key, &p.Value, &p.CreatedAt); err != nil {
			return nil, err
		}
		props = append(props, &p)
	}
	return props, rows.Err()
}

// GetProperty returns a single property value for a node, or empty string if not set.
func GetProperty(db *sql.DB, nodeID int64, key string) string {
	var value string
	err := db.QueryRow(`SELECT value FROM node_properties WHERE node_id=$1 AND key=$2`, nodeID, key).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}
