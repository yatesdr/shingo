package store

// Stage 2D delegate file: node_payloads junction writes live in store/nodes/
// (AssignPayload/UnassignPayload/SetPayloads), while the cross-aggregate
// queries that return *Payload or *Node stay here as composition methods.

import (
	"fmt"

	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

func (db *DB) AssignPayloadToNode(nodeID, payloadID int64) error {
	return nodes.AssignPayload(db.DB, nodeID, payloadID)
}

func (db *DB) UnassignPayloadFromNode(nodeID, payloadID int64) error {
	return nodes.UnassignPayload(db.DB, nodeID, payloadID)
}

// ListPayloadsForNode returns the directly-assigned payload templates for a
// node. Cross-aggregate (nodes ↔ payloads).
func (db *DB) ListPayloadsForNode(nodeID int64) ([]*Payload, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s FROM payloads
		WHERE id IN (SELECT payload_id FROM node_payloads WHERE node_id=$1)
		ORDER BY code`, payloads.SelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return payloads.ScanPayloads(rows)
}

// ListNodesForPayload returns all nodes that have the given payload assigned.
// Cross-aggregate (nodes ↔ payloads).
func (db *DB) ListNodesForPayload(payloadID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s %s
		WHERE n.id IN (SELECT np.node_id FROM node_payloads np WHERE np.payload_id=$1)
		ORDER BY n.name`, nodes.SelectCols, nodes.FromClause), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return nodes.ScanNodes(rows)
}

// GetEffectivePayloads returns payload templates for a node, walking up the
// parent chain until a non-empty set is found. Returns nil (all payloads) if
// no ancestor has payloads. Uses a recursive CTE to resolve the ancestor
// chain in a single query. Cross-aggregate (nodes ↔ payloads).
func (db *DB) GetEffectivePayloads(nodeID int64) ([]*Payload, error) {
	rows, err := db.Query(fmt.Sprintf(`
		WITH RECURSIVE ancestors AS (
			SELECT id, parent_id, 0 AS depth FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id, a.depth + 1 FROM nodes n
			JOIN ancestors a ON n.id = a.parent_id
		)
		SELECT %s FROM payloads
		WHERE id IN (
			SELECT np.payload_id FROM node_payloads np
			WHERE np.node_id = (
				SELECT a.id FROM ancestors a
				WHERE EXISTS (SELECT 1 FROM node_payloads np2 WHERE np2.node_id = a.id)
				ORDER BY a.depth ASC
				LIMIT 1
			)
		)
		ORDER BY code`, payloads.SelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return payloads.ScanPayloads(rows)
}

// SetNodePayloads replaces all payload template assignments for a node.
func (db *DB) SetNodePayloads(nodeID int64, payloadIDs []int64) error {
	return nodes.SetPayloads(db.DB, nodeID, payloadIDs)
}
