package store

// Stage 2D delegate file: order_bins junction CRUD lives in store/orders/.
// ApplyMultiBinArrival stays here as cross-aggregate composition because it
// mutates both the bins and order_bins tables in a single transaction.

import (
	"fmt"

	"shingocore/store/internal/helpers"
	"shingocore/store/orders"
)

// Type aliases preserve the store.OrderBin / store.BinArrivalInstruction
// public API.
type OrderBin = orders.OrderBin
type BinArrivalInstruction = orders.BinArrivalInstruction

// InsertOrderBin records a claimed bin and its resolved destination for a
// complex order.
func (db *DB) InsertOrderBin(orderID, binID int64, stepIndex int, action, nodeName, destNode string) error {
	return orders.InsertOrderBin(db.DB, orderID, binID, stepIndex, action, nodeName, destNode)
}

// ListOrderBins returns all junction rows for an order, ordered by step_index.
func (db *DB) ListOrderBins(orderID int64) ([]*OrderBin, error) {
	return orders.ListOrderBins(db.DB, orderID)
}

// DeleteOrderBins removes all junction rows for an order. Called alongside
// UnclaimOrderBins on cancel/fail paths to keep the junction table clean.
func (db *DB) DeleteOrderBins(orderID int64) { orders.DeleteOrderBins(db.DB, orderID) }

// ApplyMultiBinArrival moves multiple bins to their per-step destinations and
// unclaims them atomically in a single transaction. The caller provides
// pre-computed arrival instructions (destination node, staging, expiry) for
// each bin. Cross-aggregate (bins ↔ order_bins).
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
				helpers.NullableTime(inst.ExpiresAt), inst.BinID); err != nil {
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
