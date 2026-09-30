package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ReleaseCaptureRecord is what a release has already captured to lineside for
// one order: the per-part quantities applied, and the bin and epoch they were
// applied against, recorded as data. A later attempt at the same release
// applies only the per-part difference (see engine's capture step).
type ReleaseCaptureRecord struct {
	BinID int64          `json:"bin_id"`
	Epoch int64          `json:"epoch"`
	Parts map[string]int `json:"parts"`
}

// releasePaperworkCapture is the ledger kind for the lineside capture.
const releasePaperworkCapture = "capture"

// GetReleaseCapture returns what a release has already captured for an order,
// or nil when nothing has been.
func (db *DB) GetReleaseCapture(orderID int64) (*ReleaseCaptureRecord, error) {
	var raw string
	err := db.QueryRow(`SELECT applied_json FROM release_paperwork WHERE order_id = ? AND kind = ?`,
		orderID, releasePaperworkCapture).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release capture for order %d: %w", orderID, err)
	}
	var rec ReleaseCaptureRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("decode release capture for order %d: %w", orderID, err)
	}
	return &rec, nil
}

// PutReleaseCapture records what a release has captured for an order, replacing
// any earlier record.
func (db *DB) PutReleaseCapture(orderID int64, rec ReleaseCaptureRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode release capture for order %d: %w", orderID, err)
	}
	_, err = db.Exec(`INSERT INTO release_paperwork (order_id, kind, applied_json, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT (order_id, kind) DO UPDATE SET applied_json = excluded.applied_json, updated_at = excluded.updated_at`,
		orderID, releasePaperworkCapture, string(raw))
	if err != nil {
		return fmt.Errorf("write release capture for order %d: %w", orderID, err)
	}
	return nil
}
