// pending_lane_extensions store helpers — persistence layer for the
// crash-safe lane-lock-extension listener added post-v7. See migration
// v24. Mirrors pending_restocks but is tighter — no synthetic parent
// or restock plan JSON, just the four fields the listener needs to
// release the lane lock with the right race-guard at fire time.
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// PendingLaneExtension is the lane-lock-extension listener: the standing
// instruction "when bin B leaves node N, release the reshuffle's hold on lane L".
//
// THE ROW IS THE LISTENER. It used to be a durability mirror behind an in-memory
// laneHoldRegistry, which is the arrangement this line of work has now unwound
// three times — the lane lock's map, the dig claim in scenesim, and this. Two
// writers for one fact is the failure mode, and a crash-volatile one that has to
// be re-registered at boot is the version of it that also loses state.
//
// Consume is a DELETE ... RETURNING, so matching and claiming are one statement.
// That is not just tidier than the peek-then-consume it replaces: the old pair
// had a real window between deciding an entry matched and taking it, which its
// own comment called out ("raced with another consumer").
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

// ConsumePendingLaneExtensionByBin claims the listener waiting on binID, if that
// listener is also waiting on this from-node. Returns (nil, nil) when nothing
// matches — a bin moved by an unrelated order must not release anyone's lane.
//
// THE FROM-NODE MATCH IS IN THE SQL, which is where it belongs: the check and
// the claim are the same statement, so two callers cannot both decide they
// matched. expected_from_node_id = 0 means "any node" and is kept as a wildcard
// because rows written before the from-node was recorded carry it.
func (db *DB) ConsumePendingLaneExtensionByBin(binID, fromNodeID int64) (*PendingLaneExtension, error) {
	row := db.QueryRow(`DELETE FROM pending_lane_extensions
		WHERE target_bin_id = $1
		  AND (expected_from_node_id = 0 OR expected_from_node_id = $2)
		RETURNING id, complex_parent_id, lane_id, target_bin_id, expected_from_node_id`,
		binID, fromNodeID)
	return scanConsumedExtension(row)
}

// ConsumePendingLaneExtensionByComplexParent claims the listener belonging to a
// complex parent — the terminal-cleanup path. Returns (nil, nil) when there is
// none, which is the ordinary case for a parent that never had one.
func (db *DB) ConsumePendingLaneExtensionByComplexParent(complexParentID int64) (*PendingLaneExtension, error) {
	row := db.QueryRow(`DELETE FROM pending_lane_extensions
		WHERE complex_parent_id = $1
		RETURNING id, complex_parent_id, lane_id, target_bin_id, expected_from_node_id`,
		complexParentID)
	return scanConsumedExtension(row)
}

func scanConsumedExtension(row *sql.Row) (*PendingLaneExtension, error) {
	var r PendingLaneExtension
	err := row.Scan(&r.ID, &r.ComplexParentID, &r.LaneID, &r.TargetBinID, &r.ExpectedFromNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume pending_lane_extension: %w", err)
	}
	return &r, nil
}
