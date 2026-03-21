package store

import (
	"database/sql"
	"fmt"
	"time"
)

// RepairConfirmedOrderCompletion finalizes an order that is already confirmed
// but missing completed_at, while atomically applying the bin arrival.
func (db *DB) RepairConfirmedOrderCompletion(orderID, binID, toNodeID int64, staged bool, expiresAt *time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE orders
		SET completed_at=NOW(), updated_at=NOW()
		WHERE id=$1 AND status='confirmed' AND completed_at IS NULL`, orderID)
	if err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("order %d is not awaiting completion repair", orderID)
	}
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, 'confirmed', 'order completion repaired')`, orderID); err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	if _, err := tx.Exec(`UPDATE bins SET node_id=$1, claimed_by=NULL, updated_at=NOW() WHERE id=$2`, toNodeID, binID); err != nil {
		return fmt.Errorf("move bin: %w", err)
	}
	if staged {
		if _, err := tx.Exec(`UPDATE bins
			SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW()
			WHERE id=$2`, nullableTime(expiresAt), binID); err != nil {
			return fmt.Errorf("stage bin: %w", err)
		}
	} else {
		if _, err := tx.Exec(`UPDATE bins
			SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW()
			WHERE id=$1`, binID); err != nil {
			return fmt.Errorf("set available bin: %w", err)
		}
	}

	return tx.Commit()
}

// ReleaseTerminalBinClaim clears a stale bin claim only when the claiming order
// is already terminal. It refuses to release claims held by active orders.
func (db *DB) ReleaseTerminalBinClaim(binID int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var claimedBy sql.NullInt64
	if err := tx.QueryRow(`SELECT claimed_by FROM bins WHERE id=$1 FOR UPDATE`, binID).Scan(&claimedBy); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("bin %d not found", binID)
		}
		return 0, fmt.Errorf("load bin claim: %w", err)
	}
	if !claimedBy.Valid {
		return 0, fmt.Errorf("bin %d is not claimed", binID)
	}

	var status string
	var completedAt sql.NullTime
	err = tx.QueryRow(`SELECT status, completed_at FROM orders WHERE id=$1`, claimedBy.Int64).Scan(&status, &completedAt)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("load claiming order: %w", err)
	}
	if err == nil && !completedAt.Valid && status != "cancelled" && status != "failed" {
		return 0, fmt.Errorf("bin %d is claimed by active order %d (%s)", binID, claimedBy.Int64, status)
	}

	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, binID); err != nil {
		return 0, fmt.Errorf("release claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return claimedBy.Int64, nil
}
