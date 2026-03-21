package store

import "fmt"

func (db *DB) AssignPayloadToNode(nodeID, payloadID int64) error {
	_, err := db.Exec(`INSERT INTO node_payloads (node_id, payload_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, nodeID, payloadID)
	return err
}

func (db *DB) UnassignPayloadFromNode(nodeID, payloadID int64) error {
	_, err := db.Exec(`DELETE FROM node_payloads WHERE node_id=$1 AND payload_id=$2`, nodeID, payloadID)
	return err
}

func (db *DB) ListPayloadsForNode(nodeID int64) ([]*Payload, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s FROM payloads
		WHERE id IN (SELECT payload_id FROM node_payloads WHERE node_id=$1)
		ORDER BY code`, payloadSelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

func (db *DB) ListNodesForPayload(payloadID int64) ([]*Node, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT %s %s
		WHERE n.id IN (SELECT np.node_id FROM node_payloads np WHERE np.payload_id=$1)
		ORDER BY n.name`, nodeSelectCols, nodeFromClause), payloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetEffectivePayloads returns payload templates for a node, walking up the parent
// chain until a non-empty set is found. Returns nil (all payloads) if no ancestor has payloads.
// Uses a recursive CTE to resolve the ancestor chain in a single query.
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
		ORDER BY code`, payloadSelectCols), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPayloads(rows)
}

// SetNodePayloads replaces all payload template assignments for a node.
func (db *DB) SetNodePayloads(nodeID int64, payloadIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM node_payloads WHERE node_id=$1`, nodeID); err != nil {
		return err
	}
	for _, pID := range payloadIDs {
		if _, err := tx.Exec(`INSERT INTO node_payloads (node_id, payload_id) VALUES ($1, $2)`, nodeID, pID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
