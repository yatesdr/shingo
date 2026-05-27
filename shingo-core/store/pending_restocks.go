// pending_restocks store helpers — persistence layer for the
// crash-safe restore-listener registry added in v7. See migration v23.
package store

import (
	"database/sql"
	"fmt"
)

// PendingRestock is the persisted form of a restore-blockers
// listener. The in-memory restoreRegistry still owns the live
// per-process state; this table is the cross-restart durability
// guarantee.
type PendingRestock struct {
	ID                 int64
	ComplexParentID    int64
	SyntheticParentID  int64
	TargetBinID        int64
	ExpectedFromNodeID int64
	RestockPlanJSON    string
}

// InsertPendingRestock writes a new row. Returns the row ID on
// success. Caller is responsible for the JSON encoding of the
// restock plan.
func (db *DB) InsertPendingRestock(r *PendingRestock) (int64, error) {
	var id int64
	err := db.QueryRow(`INSERT INTO pending_restocks
		(complex_parent_id, synthetic_parent_id, target_bin_id, expected_from_node_id, restock_plan_json)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		r.ComplexParentID, r.SyntheticParentID, r.TargetBinID, r.ExpectedFromNodeID, r.RestockPlanJSON,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert pending_restock: %w", err)
	}
	return id, nil
}

// DeletePendingRestockByComplexParent removes the row keyed on
// complex_parent_id. Returns nil even when no row matched (idempotent
// — the caller may invoke from multiple terminal paths).
func (db *DB) DeletePendingRestockByComplexParent(complexParentID int64) error {
	_, err := db.Exec(`DELETE FROM pending_restocks WHERE complex_parent_id = $1`, complexParentID)
	return err
}

// ListPendingRestocks returns all persisted listener rows — used at
// Core boot to re-register the in-memory listeners.
func (db *DB) ListPendingRestocks() ([]*PendingRestock, error) {
	rows, err := db.Query(`SELECT id, complex_parent_id, synthetic_parent_id, target_bin_id, expected_from_node_id, restock_plan_json FROM pending_restocks ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list pending_restocks: %w", err)
	}
	defer rows.Close()
	var out []*PendingRestock
	for rows.Next() {
		var r PendingRestock
		if err := rows.Scan(&r.ID, &r.ComplexParentID, &r.SyntheticParentID, &r.TargetBinID, &r.ExpectedFromNodeID, &r.RestockPlanJSON); err != nil {
			return nil, fmt.Errorf("scan pending_restock: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// queryPendingRestock is the shared scan helper used by Get and
// List. Returns sql.ErrNoRows when nothing matches.
func queryPendingRestock(row *sql.Row) (*PendingRestock, error) {
	var r PendingRestock
	if err := row.Scan(&r.ID, &r.ComplexParentID, &r.SyntheticParentID, &r.TargetBinID, &r.ExpectedFromNodeID, &r.RestockPlanJSON); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetPendingRestockByComplexParent looks up a single row by
// complex_parent_id. Returns (nil, sql.ErrNoRows) when no row matches
// — caller decides whether that's expected or a bug.
func (db *DB) GetPendingRestockByComplexParent(complexParentID int64) (*PendingRestock, error) {
	row := db.QueryRow(`SELECT id, complex_parent_id, synthetic_parent_id, target_bin_id, expected_from_node_id, restock_plan_json FROM pending_restocks WHERE complex_parent_id = $1`, complexParentID)
	return queryPendingRestock(row)
}
