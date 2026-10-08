package processes

import (
	"database/sql"
	"fmt"
	"time"

	"shingoedge/store/internal/helpers"
)

// ResumeKeepStaged stamps a line's keep-staged resume: an operator asked the
// keeper to start again after a cancel paused it — the board's RESUME, or a
// REQUEST on the line. The keeper reads the stamp as re-arming, like a claim
// save, without changing the claim's "flow saved" date.
func ResumeKeepStaged(db *sql.DB, processNodeID int64) error {
	res, err := db.Exec(`UPDATE process_node_runtime_states SET keep_staged_resumed_at = datetime('now')
		WHERE process_node_id = ?`, processNodeID)
	if err != nil {
		return fmt.Errorf("resume keep-staged on node %d: %w", processNodeID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("resume keep-staged on node %d: the node has no runtime row", processNodeID)
	}
	return nil
}

// KeepStagedResumedAt is when a line's keep-staged was last resumed from the
// board; nil when never.
func KeepStagedResumedAt(db *sql.DB, processNodeID int64) (*time.Time, error) {
	var at string
	err := db.QueryRow(`SELECT keep_staged_resumed_at FROM process_node_runtime_states
		WHERE process_node_id = ?`, processNodeID).Scan(&at)
	if err == sql.ErrNoRows || (err == nil && at == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return helpers.ScanTimePtr(sql.NullString{String: at, Valid: true}), nil
}
