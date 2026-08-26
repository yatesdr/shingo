package store

// Stage 2D delegate file: node_types CRUD lives in store/nodes/.

import "shingocore/store/nodes"

func (db *DB) CreateNodeType(nt *nodes.NodeType) error { return nodes.CreateType(db.DB, nt) }
func (db *DB) GetNodeTypeByCode(code string) (*nodes.NodeType, error) {
	return nodes.GetTypeByCode(db.DB, code)
}
func (db *DB) ListNodeTypes() ([]*nodes.NodeType, error) { return nodes.ListTypes(db.DB) }
