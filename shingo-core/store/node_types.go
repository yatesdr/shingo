package store

// Stage 2D delegate file: node_types CRUD lives in store/nodes/.

import "shingocore/store/nodes"

func (db *DB) CreateNodeType(nt *nodes.NodeType) error       { return nodes.CreateType(db.DB, nt) }
func (db *DB) UpdateNodeType(nt *nodes.NodeType) error       { return nodes.UpdateType(db.DB, nt) }
func (db *DB) DeleteNodeType(id int64) error           { return nodes.DeleteType(db.DB, id) }
func (db *DB) GetNodeType(id int64) (*nodes.NodeType, error) { return nodes.GetType(db.DB, id) }
func (db *DB) GetNodeTypeByCode(code string) (*nodes.NodeType, error) {
	return nodes.GetTypeByCode(db.DB, code)
}
func (db *DB) ListNodeTypes() ([]*nodes.NodeType, error) { return nodes.ListTypes(db.DB) }
