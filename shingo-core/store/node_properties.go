package store

// Stage 2D delegate file: node_properties CRUD lives in store/nodes/.

import "shingocore/store/nodes"

// SetNodeProperty upserts a key-value property on a node.
func (db *DB) SetNodeProperty(nodeID int64, key, value string) error {
	return nodes.SetProperty(db.DB, nodeID, key, value)
}

func (db *DB) DeleteNodeProperty(nodeID int64, key string) error {
	return nodes.DeleteProperty(db.DB, nodeID, key)
}

func (db *DB) ListNodeProperties(nodeID int64) ([]*nodes.Property, error) {
	return nodes.ListProperties(db.DB, nodeID)
}

// GetNodeProperty returns a single property value for a node, or empty
// string if not set. Use it only where the property is advisory — it cannot
// tell "unset" from "the read failed". Where the answer decides something,
// use GetNodePropertyOrError.
func (db *DB) GetNodeProperty(nodeID int64, key string) string {
	return nodes.GetProperty(db.DB, nodeID, key)
}

// GetNodePropertyOrError returns a node property, distinguishing UNSET
// ("", nil) from UNREADABLE ("", err).
func (db *DB) GetNodePropertyOrError(nodeID int64, key string) (string, error) {
	return nodes.GetPropertyOrError(db.DB, nodeID, key)
}
