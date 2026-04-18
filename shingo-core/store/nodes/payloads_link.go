package nodes

import "database/sql"

// AssignPayload links a payload template to a node (upsert).
// Owns the node_payloads junction table from the node side; payload-returning
// queries (ListPayloadsForNode, GetEffectivePayloads) live at outer store/
// because they return *Payload.
func AssignPayload(db *sql.DB, nodeID, payloadID int64) error {
	_, err := db.Exec(`INSERT INTO node_payloads (node_id, payload_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, nodeID, payloadID)
	return err
}

// UnassignPayload removes a payload assignment from a node.
func UnassignPayload(db *sql.DB, nodeID, payloadID int64) error {
	_, err := db.Exec(`DELETE FROM node_payloads WHERE node_id=$1 AND payload_id=$2`, nodeID, payloadID)
	return err
}

// SetPayloads replaces all payload template assignments for a node.
func SetPayloads(db *sql.DB, nodeID int64, payloadIDs []int64) error {
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
