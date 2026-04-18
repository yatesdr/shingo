package orders

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/domain"
)

// OrderBin is the order-bin junction domain type. The struct lives in
// shingocore/domain (Stage 2A); this alias keeps the orders.OrderBin
// name used by InsertOrderBin/ListOrderBins and the outer store/
// order_bins.go re-export (store.OrderBin). BinArrivalInstruction
// stays local — it is a persistence-layer intent (a batched staging
// update passed into ApplyMultiBinArrival), not a domain entity.
type OrderBin = domain.OrderBin

// BinArrivalInstruction describes how to move and unclaim a single bin atomically.
// The caller computes staging/expiry per destination node; the store executes all
// instructions in one transaction. Consumed by the outer store/ composition
// method ApplyMultiBinArrival, which needs to mutate both the orders/order_bins
// tables and the bins aggregate in a single transaction.
type BinArrivalInstruction struct {
	BinID     int64
	ToNodeID  int64
	Staged    bool
	ExpiresAt *time.Time
}

// InsertOrderBin records a claimed bin and its resolved destination for a complex order.
func InsertOrderBin(db *sql.DB, orderID, binID int64, stepIndex int, action, nodeName, destNode string) error {
	_, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, step_index, action, node_name, dest_node)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		orderID, binID, stepIndex, action, nodeName, destNode)
	if err != nil {
		return fmt.Errorf("insert order_bin: %w", err)
	}
	return nil
}

// ListOrderBins returns all junction rows for an order, ordered by step_index.
func ListOrderBins(db *sql.DB, orderID int64) ([]*OrderBin, error) {
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
func DeleteOrderBins(db *sql.DB, orderID int64) {
	db.Exec(`DELETE FROM order_bins WHERE order_id = $1`, orderID)
}
