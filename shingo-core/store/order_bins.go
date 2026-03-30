package store

import (
	"fmt"
	"time"
)

// OrderBin tracks a claimed bin and its per-step destination for multi-bin complex orders.
// Single-bin orders continue using Order.BinID; the junction table is only populated
// when claimComplexBins claims two or more bins.
type OrderBin struct {
	ID        int64     `json:"id"`
	OrderID   int64     `json:"order_id"`
	BinID     int64     `json:"bin_id"`
	StepIndex int       `json:"step_index"`
	Action    string    `json:"action"`
	NodeName  string    `json:"node_name"`
	DestNode  string    `json:"dest_node"`
	CreatedAt time.Time `json:"created_at"`
}

// BinArrivalInstruction describes how to move and unclaim a single bin atomically.
// The caller computes staging/expiry per destination node; the store executes all
// instructions in one transaction.
type BinArrivalInstruction struct {
	BinID     int64
	ToNodeID  int64
	Staged    bool
	ExpiresAt *time.Time
}

// InsertOrderBin records a claimed bin and its resolved destination for a complex order.
func (db *DB) InsertOrderBin(orderID, binID int64, stepIndex int, action, nodeName, destNode string) error {
	_, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, step_index, action, node_name, dest_node)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		orderID, binID, stepIndex, action, nodeName, destNode)
	if err != nil {
		return fmt.Errorf("insert order_bin: %w", err)
	}
	return nil
}

// ListOrderBins returns all junction rows for an order, ordered by step_index.
func (db *DB) ListOrderBins(orderID int64) ([]*OrderBin, error) {
	rows, err := db.Query(`SELECT id, order_id, bin_id, step_index, action, node_name, dest_node, created_at
		FROM order_bins WHERE order_id = $1 ORDER BY step_index`, orderID)
	if err != nil {
		return nil, fmt.Errorf("list order_bins: %w", err)
	}
	defer rows.Close()

	var result []*OrderBin
	for rows.Next() {
		ob := &OrderBin{}
		if err := rows.Scan(&ob.ID, &ob.OrderID, &ob.BinID, &ob.StepIndex, &ob.Action, &ob.NodeName, &ob.DestNode, &ob.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan order_bin: %w", err)
		}
		result = append(result, ob)
	}
	return result, nil
}

// DeleteOrderBins removes all junction rows for an order. Called alongside
// UnclaimOrderBins on cancel/fail paths to keep the junction table clean.
func (db *DB) DeleteOrderBins(orderID int64) {
	db.Exec(`DELETE FROM order_bins WHERE order_id = $1`, orderID)
}

// ApplyMultiBinArrival moves multiple bins to their per-step destinations and unclaims
// them atomically in a single transaction. The caller provides pre-computed arrival
// instructions (destination node, staging, expiry) for each bin.
//
// This prevents partial failures where some bins are moved but others are not.
func (db *DB) ApplyMultiBinArrival(instructions []BinArrivalInstruction) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	for _, inst := range instructions {
		if _, err := tx.Exec(`UPDATE bins SET node_id=$1, claimed_by=NULL, updated_at=NOW() WHERE id=$2`,
			inst.ToNodeID, inst.BinID); err != nil {
			return fmt.Errorf("move bin %d: %w", inst.BinID, err)
		}
		if inst.Staged {
			if _, err := tx.Exec(`UPDATE bins SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW() WHERE id=$2`,
				nullableTime(inst.ExpiresAt), inst.BinID); err != nil {
				return fmt.Errorf("stage bin %d: %w", inst.BinID, err)
			}
		} else {
			if _, err := tx.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW() WHERE id=$1`,
				inst.BinID); err != nil {
				return fmt.Errorf("set available bin %d: %w", inst.BinID, err)
			}
		}
	}

	return tx.Commit()
}
