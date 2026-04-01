package store

import "time"

type ProcessNodeRuntimeState struct {
	ID            int64  `json:"id"`
	ProcessNodeID int64  `json:"process_node_id"`
	ActiveClaimID *int64 `json:"active_claim_id,omitempty"`
	RemainingUOP  int    `json:"remaining_uop"`
	ActiveOrderID *int64 `json:"active_order_id,omitempty"`
	StagedOrderID *int64 `json:"staged_order_id,omitempty"`
	ActivePull    bool   `json:"active_pull"` // A/B cycling: only the active-pull node gets counter deltas
	UpdatedAt     time.Time `json:"updated_at"`
}

func scanRuntime(scanner interface{ Scan(...interface{}) error }) (ProcessNodeRuntimeState, error) {
	var r ProcessNodeRuntimeState
	var updatedAt string
	err := scanner.Scan(&r.ID, &r.ProcessNodeID, &r.ActiveClaimID, &r.RemainingUOP,
		&r.ActiveOrderID, &r.StagedOrderID, &r.ActivePull, &updatedAt)
	if err != nil {
		return r, err
	}
	r.UpdatedAt = scanTime(updatedAt)
	return r, nil
}

func (db *DB) EnsureProcessNodeRuntime(processNodeID int64) (*ProcessNodeRuntimeState, error) {
	r, err := db.GetProcessNodeRuntime(processNodeID)
	if err == nil {
		return r, nil
	}
	_, err = db.Exec(`INSERT INTO process_node_runtime_states (process_node_id) VALUES (?)`, processNodeID)
	if err != nil {
		return nil, err
	}
	return db.GetProcessNodeRuntime(processNodeID)
}

func (db *DB) GetProcessNodeRuntime(processNodeID int64) (*ProcessNodeRuntimeState, error) {
	r, err := scanRuntime(db.QueryRow(`SELECT id, process_node_id, active_claim_id, remaining_uop,
		active_order_id, staged_order_id, active_pull, updated_at
		FROM process_node_runtime_states WHERE process_node_id=?`, processNodeID))
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET
		active_claim_id=?, remaining_uop=?, updated_at=datetime('now')
		WHERE process_node_id=?`,
		activeClaimID, remainingUOP, processNodeID)
	return err
}

func (db *DB) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_order_id=?, staged_order_id=?, updated_at=datetime('now') WHERE process_node_id=?`,
		activeOrderID, stagedOrderID, processNodeID)
	return err
}

func (db *DB) UpdateProcessNodeUOP(processNodeID int64, remainingUOP int) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET remaining_uop=?, updated_at=datetime('now') WHERE process_node_id=?`,
		remainingUOP, processNodeID)
	return err
}

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func (db *DB) SetActivePull(processNodeID int64, active bool) error {
	_, err := db.Exec(`UPDATE process_node_runtime_states SET active_pull=?, updated_at=datetime('now') WHERE process_node_id=?`,
		active, processNodeID)
	return err
}
