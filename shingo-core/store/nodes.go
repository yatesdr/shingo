package store

// Stage 2D delegate file: node CRUD lives in store/nodes/. This file
// preserves the *store.DB method surface so external callers don't need to
// change.

import "shingocore/store/nodes"

func (db *DB) CreateNode(n *nodes.Node) error              { return nodes.Create(db.DB, n) }
func (db *DB) UpdateNode(n *nodes.Node) error              { return nodes.Update(db.DB, n) }
func (db *DB) DeleteNode(id int64) error             { return nodes.Delete(db.DB, id) }
func (db *DB) GetNode(id int64) (*nodes.Node, error)       { return nodes.Get(db.DB, id) }
func (db *DB) GetNodeByName(name string) (*nodes.Node, error) {
	return nodes.GetByName(db.DB, name)
}
func (db *DB) GetNodeByDotName(name string) (*nodes.Node, error) {
	return nodes.GetByDotName(db.DB, name)
}
func (db *DB) GetRootNode(nodeID int64) (*nodes.Node, error) { return nodes.GetRoot(db.DB, nodeID) }
func (db *DB) ListNodes() ([]*nodes.Node, error)             { return nodes.List(db.DB) }
func (db *DB) ListChildNodes(parentID int64) ([]*nodes.Node, error) {
	return nodes.ListChildren(db.DB, parentID)
}
func (db *DB) SetNodeParent(nodeID, parentID int64) error {
	return nodes.SetParent(db.DB, nodeID, parentID)
}
func (db *DB) ClearNodeParent(nodeID int64) error { return nodes.ClearParent(db.DB, nodeID) }

// ReparentNode moves a node into a new parent (or removes it from a parent).
// When adopting into a lane, it sets the depth based on position. When
// orphaning, it clears depth and role properties.
func (db *DB) ReparentNode(nodeID int64, parentID *int64, position int) error {
	return nodes.Reparent(db.DB, nodeID, parentID, position)
}

// ReorderLaneSlots updates depth for all slots in a lane based on the
// provided ordered list of node IDs.
func (db *DB) ReorderLaneSlots(laneID int64, orderedNodeIDs []int64) error {
	return nodes.ReorderLaneSlots(db.DB, laneID, orderedNodeIDs)
}
