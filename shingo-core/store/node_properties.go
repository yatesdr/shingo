package store

// Stage 2D delegate file: node_properties CRUD lives in store/nodes/.

import "shingocore/store/nodes"

// NodeProperty aliases the nodes sub-package's property row type.
type NodeProperty = nodes.Property

// SetNodeProperty upserts a key-value property on a node.
func (db *DB) SetNodeProperty(nodeID int64, key, value string) error {
	return nodes.SetProperty(db.DB, nodeID, key, value)
}

func (db *DB) DeleteNodeProperty(nodeID int64, key string) error {
	return nodes.DeleteProperty(db.DB, nodeID, key)
}

func (db *DB) ListNodeProperties(nodeID int64) ([]*NodeProperty, error) {
	return nodes.ListProperties(db.DB, nodeID)
}

// GetNodeProperty returns a single property value for a node, or empty
// string if not set.
func (db *DB) GetNodeProperty(nodeID int64, key string) string {
	return nodes.GetProperty(db.DB, nodeID, key)
}
