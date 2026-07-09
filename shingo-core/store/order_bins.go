package store

// Stage 2D delegate file: order_bins junction CRUD lives in store/orders/.
// ApplyMultiBinArrival stays here as cross-aggregate composition because it
// mutates both the bins and order_bins tables in a single transaction.

import (
	"database/sql"
	"fmt"

	"shingocore/store/internal/helpers"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// InsertOrderBin records a claimed bin and its resolved destination for a
// complex order.
func (db *DB) InsertOrderBin(orderID, binID int64, stepIndex int, action, nodeName, destNode string) error {
	return orders.InsertOrderBin(db.DB, orderID, binID, stepIndex, action, nodeName, destNode)
}

// ListOrderBins returns all junction rows for an order, ordered by step_index.
func (db *DB) ListOrderBins(orderID int64) ([]*orders.OrderBin, error) {
	return orders.ListOrderBins(db.DB, orderID)
}

// DeleteOrderBins removes all junction rows for an order. Called alongside
// UnclaimOrderBins on cancel/fail paths to keep the junction table clean.
func (db *DB) DeleteOrderBins(orderID int64) { orders.DeleteOrderBins(db.DB, orderID) }

// EvictStaleGhostsTx is the *store.DB entry point to the shared stale-ghost
// reconciliation (helpers.EvictStaleGhostBinsTx) for callers that reach the
// store through *store.DB but cannot import store/internal — notably
// service.BinService.ApplyArrival. Store-internal callers (ApplyMultiBinArrival)
// and the recovery sub-package call the helper directly. See
// helpers.EvictStaleGhostBinsTx for the mechanism and plant-verified rationale.
func (db *DB) EvictStaleGhostsTx(tx *sql.Tx, toNodeID, keepBinID int64) ([]int64, error) {
	return helpers.EvictStaleGhostBinsTx(tx, toNodeID, keepBinID)
}

// ApplyMultiBinArrival moves multiple bins to their per-step destinations and
// unclaims them atomically in a single transaction. The caller provides
// pre-computed arrival instructions (destination node, staging, expiry) for
// each bin. Cross-aggregate (bins ↔ order_bins).
//
// Each instruction first reconciles occupancy via EvictStaleGhostsTx — the same
// stale-ghost eviction ApplyArrival performs for single-bin arrivals: a stale
// bin recorded at a destination is evicted to _TRANSIT + anomaly before the
// newcomer lands. Returns the ids of any evicted ghosts so the caller can alert.
func (db *DB) ApplyMultiBinArrival(instructions []orders.BinArrivalInstruction) ([]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var evictedGhosts []int64
	for _, inst := range instructions {
		// Reconcile occupancy before placing the newcomer: evict any stale ghost
		// recorded at this destination (non-synthetic only) to _TRANSIT + anomaly.
		ghosts, err := helpers.EvictStaleGhostBinsTx(tx, inst.ToNodeID, inst.BinID)
		if err != nil {
			return nil, err
		}
		evictedGhosts = append(evictedGhosts, ghosts...)

		if _, err := tx.Exec(`UPDATE bins SET node_id=$1, claimed_by=NULL, updated_at=NOW() WHERE id=$2`,
			inst.ToNodeID, inst.BinID); err != nil {
			return nil, fmt.Errorf("move bin %d: %w", inst.BinID, err)
		}
		// Release the bin's reservation alongside its claim (see ApplyArrival) so
		// a delivered bin frees for re-reservation at delivery, not at terminal.
		if err := reservations.ReleaseByBin(tx, inst.BinID); err != nil {
			return nil, fmt.Errorf("release reservation on arrival bin %d: %w", inst.BinID, err)
		}
		// Release the destination slot's dispatch-time claim (see ApplyArrival).
		if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, inst.ToNodeID); err != nil {
			return nil, fmt.Errorf("release destination slot claim node %d: %w", inst.ToNodeID, err)
		}
		// ...and its slot reservation, same tx (the slot dual of ReleaseByBin).
		if err := reservations.ReleaseByNode(tx, inst.ToNodeID); err != nil {
			return nil, fmt.Errorf("release slot reservation on arrival node %d: %w", inst.ToNodeID, err)
		}
		if inst.Staged {
			if _, err := tx.Exec(`UPDATE bins SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW() WHERE id=$2`,
				helpers.NullableTime(inst.ExpiresAt), inst.BinID); err != nil {
				return nil, fmt.Errorf("stage bin %d: %w", inst.BinID, err)
			}
		} else {
			if _, err := tx.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW() WHERE id=$1`,
				inst.BinID); err != nil {
				return nil, fmt.Errorf("set available bin %d: %w", inst.BinID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit multi-bin arrival: %w", err)
	}
	return evictedGhosts, nil
}
