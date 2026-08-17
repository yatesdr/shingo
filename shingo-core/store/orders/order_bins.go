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

// InsertOrderBin records a claimed bin and its resolved destination for a complex
// order. Idempotent per (order, bin): re-recording a claim updates the row rather
// than adding a second one.
//
// ── IT WAS A BARE INSERT, AND ALLOCATION RETRIES ───────────────────────────
//
// The grain has always been one row per claimed bin — UpdateOrderBinDestNode
// keys on the bin and says so, and binForStep reads the first row matching a
// step index as if it were the only one. Nothing enforced it, so every re-run of
// an allocation added the same row again. Measured on the lane-stress rig
// 2026-08-13, during a window in which five demands were stuck in a re-drive
// loop: 2,472 rows for a handful of orders, 450 of them identical for one
// (order, step, bin), growing for as long as the orders stayed stuck.
//
// Nothing read a wrong answer — the duplicates are identical and the first match
// wins — so what this costs is writes and the ability to read the table during
// an incident, which is exactly when somebody does.
//
// ON CONFLICT DO UPDATE rather than DO NOTHING, because the two differ on the
// case that matters: an allocation that retries after the plan moved carries a
// NEW step index or destination for the same bin, and DO NOTHING would keep the
// stale one while reporting success. The unique index (v80) is what makes either
// possible.
func InsertOrderBin(db *sql.DB, orderID, binID int64, stepIndex int, action, nodeName, destNode string) error {
	_, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, step_index, action, node_name, dest_node)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (order_id, bin_id) DO UPDATE
		SET step_index = EXCLUDED.step_index,
		    action     = EXCLUDED.action,
		    node_name  = EXCLUDED.node_name,
		    dest_node  = EXCLUDED.dest_node`,
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

// UpdateOrderBinDestNode re-points one bin's recorded destination. Returns the
// number of rows changed, which is 0 or 1.
//
// KEYED BY BIN, NOT BY step_index, and that is forced by what the column means.
// step_index names the PICKUP the allocator claimed this bin at
// (InsertOrderBin's action argument is hard-coded ActionPickup), while every
// caller wanting to re-point a destination knows a DROPOFF. Keying an update on
// the dropoff's index would match no row and report success — the junction has
// one row per claimed bin, so the bin is its natural grain and the only key that
// cannot silently miss.
//
// Zero rows is NOT an error. Single-bin orders have no junction rows at all (the
// allocator writes them only when an order claims more than one), so "nothing to
// update" is the ordinary answer for most of the plant. The count is returned
// rather than swallowed so a caller can tell "no rows exist" from "the row I
// meant to hit was not there", which are different facts.
func UpdateOrderBinDestNode(db *sql.DB, orderID, binID int64, destNode string) (int64, error) {
	res, err := db.Exec(`UPDATE order_bins SET dest_node = $3 WHERE order_id = $1 AND bin_id = $2`,
		orderID, binID, destNode)
	if err != nil {
		return 0, fmt.Errorf("update order_bin dest_node (order %d bin %d): %w", orderID, binID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("update order_bin dest_node (order %d bin %d): rows affected: %w", orderID, binID, err)
	}
	return n, nil
}

// DeleteOrderBins removes all junction rows for an order. Called alongside
// UnclaimOrderBins on cancel/fail paths to keep the junction table clean.
func DeleteOrderBins(db *sql.DB, orderID int64) {
	db.Exec(`DELETE FROM order_bins WHERE order_id = $1`, orderID)
}

// ShiftOrderBinSteps rewrites step_index for an order's junction rows, applying
// shift[oldIndex] = newIndex. Rows whose index has no entry are left alone.
//
// step_index is a POSITION IN THE ORDER'S PLAN, so any transform that inserts
// steps invalidates every row after the insertion. The transform owns the
// repair; this is the write it makes. Doing it in one transaction matters
// because the shift moves indices UPWARD into positions other rows still
// occupy — row-at-a-time updates would transiently collide with the
// (order_id, step_index) uniqueness the table relies on.
func ShiftOrderBinSteps(db *sql.DB, orderID int64, shift map[int]int) error {
	if len(shift) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("shift order_bins: begin: %w", err)
	}
	defer tx.Rollback()

	// Park every row out of the way first (negative indices cannot collide with
	// a real position), then land each on its final index.
	for old := range shift {
		if _, err := tx.Exec(
			`UPDATE order_bins SET step_index = -1 - step_index
			   WHERE order_id = $1 AND step_index = $2`, orderID, old); err != nil {
			return fmt.Errorf("shift order_bins: park step %d: %w", old, err)
		}
	}
	for old, new := range shift {
		if _, err := tx.Exec(
			`UPDATE order_bins SET step_index = $3
			   WHERE order_id = $1 AND step_index = $2`, orderID, -1-old, new); err != nil {
			return fmt.Errorf("shift order_bins: step %d -> %d: %w", old, new, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("shift order_bins: commit: %w", err)
	}
	return nil
}
