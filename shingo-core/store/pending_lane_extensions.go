// pending_lane_extensions store helpers — persistence layer for the
// crash-safe lane-lock-extension listener added post-v7. See migration
// v24. Mirrors pending_restocks but is tighter — no synthetic parent
// or restock plan JSON, just the four fields the listener needs to
// release the lane lock with the right race-guard at fire time.
package store

import (
	"fmt"
)

// PendingLaneExtension is the persisted form of a lane-lock-extension
// listener. The in-memory laneHoldRegistry still owns the live
// per-process state; this table is the cross-restart durability
// guarantee — without it, Core restart between unbury completion and
// bin pickup loses the listener and the lane stays locked forever
// (the v6-era in-memory-only design).
type PendingLaneExtension struct {
	ID                 int64
	ComplexParentID    int64
	LaneID             int64
	TargetBinID        int64
	ExpectedFromNodeID int64
}

// InsertPendingLaneExtension writes a new row. Returns the row ID on
// success.
func (db *DB) InsertPendingLaneExtension(r *PendingLaneExtension) (int64, error) {
	var id int64
	err := db.QueryRow(`INSERT INTO pending_lane_extensions
		(complex_parent_id, lane_id, target_bin_id, expected_from_node_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		r.ComplexParentID, r.LaneID, r.TargetBinID, r.ExpectedFromNodeID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert pending_lane_extension: %w", err)
	}
	return id, nil
}

// DeletePendingLaneExtensionByComplexParent removes the row keyed on
// complex_parent_id. Idempotent — returns nil even when no row matched
// so callers can invoke from multiple terminal paths (bin transit,
// parent cancel, parent fail) without coordination.
func (db *DB) DeletePendingLaneExtensionByComplexParent(complexParentID int64) error {
	_, err := db.Exec(`DELETE FROM pending_lane_extensions WHERE complex_parent_id = $1`, complexParentID)
	return err
}

// ListPendingLaneExtensions returns all persisted listener rows —
// used at Core boot to re-register the in-memory listeners.
func (db *DB) ListPendingLaneExtensions() ([]*PendingLaneExtension, error) {
	rows, err := db.Query(`SELECT id, complex_parent_id, lane_id, target_bin_id, expected_from_node_id FROM pending_lane_extensions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list pending_lane_extensions: %w", err)
	}
	defer rows.Close()
	var out []*PendingLaneExtension
	for rows.Next() {
		var r PendingLaneExtension
		if err := rows.Scan(&r.ID, &r.ComplexParentID, &r.LaneID, &r.TargetBinID, &r.ExpectedFromNodeID); err != nil {
			return nil, fmt.Errorf("scan pending_lane_extension: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetPendingLaneExtensionByComplexParent looks up a single row by
// complex_parent_id. Returns (nil, sql.ErrNoRows) when no row matches
// — caller decides whether that's expected or a bug.
func (db *DB) GetPendingLaneExtensionByComplexParent(complexParentID int64) (*PendingLaneExtension, error) {
	row := db.QueryRow(`SELECT id, complex_parent_id, lane_id, target_bin_id, expected_from_node_id FROM pending_lane_extensions WHERE complex_parent_id = $1`, complexParentID)
	var r PendingLaneExtension
	if err := row.Scan(&r.ID, &r.ComplexParentID, &r.LaneID, &r.TargetBinID, &r.ExpectedFromNodeID); err != nil {
		return nil, err
	}
	return &r, nil
}
