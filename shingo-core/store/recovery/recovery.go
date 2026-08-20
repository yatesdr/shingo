// Package recovery holds order-completion repair + recovery-action
// persistence for shingo-core.
//
// Phase 5 of the architecture plan moved RepairConfirmedOrderCompletion,
// ReleaseTerminalBinClaim, and the recovery_actions CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps type
// aliases (`store.RecoveryAction = recovery.Action`) and one-line
// delegate methods on *store.DB so external callers see no API change.
//
// The two repair methods cross orders + bins in a single transaction,
// but they are grouped here (rather than at the outer store/ level)
// because they form the "recovery" aggregate-of-coordinators cluster
// that the operations dashboard invokes.
package recovery

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"shingo/protocol/clock"
	"shingocore/store/internal/helpers"
)

// Action is the recovery_actions row entity. The type is re-aliased at
// the outer store/ level as store.RecoveryAction so
// engine/reconciliation_service.go compiles unchanged.
type Action struct {
	ID         int64     `json:"id"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type"`
	TargetID   int64     `json:"target_id"`
	Detail     string    `json:"detail"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

// RecordAction appends a recovery_actions row.
func RecordAction(db *sql.DB, action, targetType string, targetID int64, detail, actor string) error {
	_, err := db.Exec(`INSERT INTO recovery_actions (action, target_type, target_id, detail, actor) VALUES ($1, $2, $3, $4, $5)`,
		action, targetType, targetID, detail, actor)
	return err
}

// ListActions returns recent recovery_actions rows.
func ListActions(db *sql.DB, limit int) ([]*Action, error) {
	rows, err := db.Query(`SELECT id, action, target_type, target_id, detail, actor, created_at FROM recovery_actions ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*Action
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.ID, &a.Action, &a.TargetType, &a.TargetID, &a.Detail, &a.Actor, &a.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, &a)
	}
	return items, rows.Err()
}

// RepairConfirmedOrderCompletion finalizes an order that is already
// confirmed but missing completed_at, while atomically applying the bin
// arrival.
func RepairConfirmedOrderCompletion(db *sql.DB, orderID, binID, toNodeID int64, staged bool, expiresAt *time.Time) error {
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
	if _, err := tx.Exec(`INSERT INTO order_history (order_id, status, detail, created_at) VALUES ($1, 'confirmed', 'order completion repaired', $2)`, orderID, clock.Now().UTC()); err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	// ONE PLACEMENT, shared with the two arrival writers. Every defect this
	// path carried came from spelling the placement itself: it evicted no stale
	// ghost until that was added by hand, it unclaimed without scope long after
	// 445f79eb fixed the same line in applyArrival, and it released no bin
	// reservation at all — so a repaired bin stayed reserved until its owning
	// order terminalized.
	//
	// A repair is a HANDOFF: the whole method is "this order's completion is
	// being finished", so the order is done with the bin. orderID is the placer
	// by construction.
	//
	// IT DOES NOT RELEASE THE DESTINATION SLOT, which is the one place this
	// caller differs from the arrival writers. A repair reconstructs a completion
	// that already happened, possibly long ago; the slot's dispatch-time claim,
	// if there is one, belongs to whatever is driving there now, and clearing it
	// would take a live claim off a slot on the strength of a historical event.
	evicted, err := helpers.PlaceBinTx(tx, helpers.BinPlacement{
		BinID:                  binID,
		ToNodeID:               toNodeID,
		PlacedByOrder:          orderID,
		ReleaseClaim:           true,
		ReleaseDestinationSlot: false,
		Staged:                 staged,
		ExpiresAt:              expiresAt,
	})
	if err != nil {
		return err
	}
	for _, ghostID := range evicted {
		log.Printf("WARN: completion repair for order %d evicted a stale bin record (bin %d) to "+
			"_TRANSIT — the slot it repaired onto already recorded another bin; recover via the "+
			"anomalies page", orderID, ghostID)
	}

	return tx.Commit()
}

// ReleaseTerminalBinClaim clears a stale bin claim only when the
// claiming order is already terminal. Returns the claiming-order ID.
func ReleaseTerminalBinClaim(db *sql.DB, binID int64) (int64, error) {
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
