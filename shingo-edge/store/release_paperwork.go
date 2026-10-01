package store

// release_paperwork.go — the release ledger (SHAPE §3.6): what a release has
// already applied for an order, keyed (order_id, kind), so a repeated release
// applies nothing twice. Each row is written in the same transaction as the
// effect it records — the lineside capture's pile writes, the ingest's outbox
// row — so a crash between the two leaves neither: a retry finds no row and
// applies the effect once.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"shingoedge/store/lineside"
	"shingoedge/store/messaging"
)

// LedgerExec is what a ledger row is written through: the transaction its
// effect runs in.
type LedgerExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// InTx runs fn in one transaction, committing when it returns nil and rolling
// back otherwise.
func (db *DB) InTx(fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// CaptureLinesideBuckets adds each part's qty to the node's active pile of
// it, then runs inTx, in one transaction: the capture's pile gains and the
// ledger row that records them commit together or not at all.
func (db *DB) CaptureLinesideBuckets(nodeID int64, parts map[string]int, inTx func(tx *sql.Tx) error) error {
	return db.InTx(func(tx *sql.Tx) error {
		for part, qty := range parts {
			if _, err := lineside.Capture(tx, nodeID, part, qty); err != nil {
				return fmt.Errorf("capture lineside pile (node=%d part=%s): %w", nodeID, part, err)
			}
		}
		return inTx(tx)
	})
}

// EnqueueOutboxIn inserts an outbound message inside tx; the caller wakes the
// drainer after the commit (messaging.NotifyEnqueued).
func EnqueueOutboxIn(tx *sql.Tx, payload []byte, msgType string) (int64, error) {
	return messaging.EnqueueIn(tx, payload, msgType)
}

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
// any earlier record, through ex (the capture's transaction).
func PutReleaseCapture(ex LedgerExec, orderID int64, rec ReleaseCaptureRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode release capture for order %d: %w", orderID, err)
	}
	_, err = ex.Exec(`INSERT INTO release_paperwork (order_id, kind, applied_json, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT (order_id, kind) DO UPDATE SET applied_json = excluded.applied_json, updated_at = excluded.updated_at`,
		orderID, releasePaperworkCapture, string(raw))
	if err != nil {
		return fmt.Errorf("write release capture for order %d: %w", orderID, err)
	}
	return nil
}

// releasePaperworkIngest is the ledger kind for a departing produce bin's
// ingest manifest.
const releasePaperworkIngest = "ingest"

// ReleaseIngestShipped reports whether a release already shipped the ingest
// manifest for the bin this order carries away.
func (db *DB) ReleaseIngestShipped(orderID int64) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM release_paperwork WHERE order_id = ? AND kind = ?`,
		orderID, releasePaperworkIngest).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read release ingest for order %d: %w", orderID, err)
	}
	return n > 0, nil
}

// MarkReleaseIngestShipped records that a release shipped the ingest manifest
// for the bin this order carries away, with the bin and quantity as data,
// through ex (the transaction the ingest's outbox row is written in).
func MarkReleaseIngestShipped(ex LedgerExec, orderID, binID int64, qty int64) error {
	raw, err := json.Marshal(struct {
		BinID int64 `json:"bin_id"`
		Qty   int64 `json:"qty"`
	}{binID, qty})
	if err != nil {
		return err
	}
	_, err = ex.Exec(`INSERT INTO release_paperwork (order_id, kind, applied_json, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT (order_id, kind) DO UPDATE SET applied_json = excluded.applied_json, updated_at = excluded.updated_at`,
		orderID, releasePaperworkIngest, string(raw))
	if err != nil {
		return fmt.Errorf("write release ingest for order %d: %w", orderID, err)
	}
	return nil
}
