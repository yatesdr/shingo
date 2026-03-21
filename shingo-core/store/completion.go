package store

import (
	"fmt"
	"time"
)

// ApplyBinArrival moves a claimed bin to its destination and updates its claim/staging state atomically.
func (db *DB) ApplyBinArrival(binID, toNodeID int64, staged bool, expiresAt *time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE bins SET node_id=$1, updated_at=NOW() WHERE id=$2`, toNodeID, binID); err != nil {
		return fmt.Errorf("move bin: %w", err)
	}
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1`, binID); err != nil {
		return fmt.Errorf("unclaim bin: %w", err)
	}
	if staged {
		if _, err := tx.Exec(`UPDATE bins SET status='staged', staged_at=NOW(), staged_expires_at=$1, updated_at=NOW() WHERE id=$2`,
			nullableTime(expiresAt), binID); err != nil {
			return fmt.Errorf("stage bin: %w", err)
		}
	} else {
		if _, err := tx.Exec(`UPDATE bins SET status='available', staged_at=NULL, staged_expires_at=NULL, updated_at=NOW() WHERE id=$1`, binID); err != nil {
			return fmt.Errorf("set available bin: %w", err)
		}
	}

	return tx.Commit()
}
