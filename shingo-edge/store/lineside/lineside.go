// Package lineside holds persistence for node_lineside_bucket — the
// first-class "parts the operator pulled to lineside during a swap"
// inventory model. A bucket is scoped to a (node-or-pair, style, part)
// and has a small lifecycle:
//
//   - active:   parts currently on the bench, being decremented by
//               counter ticks before the node's RemainingUOP.
//   - inactive: stranded from a prior style run; auto-reactivates and
//               merges into the fresh capture when the same style runs
//               at this node again.
//
// Buckets with qty == 0 are deleted on Deactivate/Drain; the inactive
// state always has qty > 0 in practice.
//
// The outer store/ package keeps type aliases and delegate methods on
// *store.DB so callers see no API change.
package lineside

import (
	"database/sql"
	"errors"
	"fmt"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Bucket is one row of node_lineside_bucket. The struct lives in
// shingoedge/domain (Stage 2A.2) under the more descriptive name
// LinesideBucket; this alias keeps the lineside.Bucket name used by
// every scan helper, Activate/Deactivate/Drain call site, and the
// outer store/ re-export.
type Bucket = domain.LinesideBucket

// Bucket states.
const (
	StateActive   = "active"
	StateInactive = "inactive"
)

const bucketCols = `id, node_id, pair_key, style_id, part_number, qty, state, created_at, updated_at`

func scanBucket(scanner interface{ Scan(...interface{}) error }) (Bucket, error) {
	var b Bucket
	var createdAt, updatedAt string
	if err := scanner.Scan(&b.ID, &b.NodeID, &b.PairKey, &b.StyleID, &b.PartNumber,
		&b.Qty, &b.State, &createdAt, &updatedAt); err != nil {
		return b, err
	}
	b.CreatedAt = helpers.ScanTime(createdAt)
	b.UpdatedAt = helpers.ScanTime(updatedAt)
	return b, nil
}

func scanBuckets(rows helpers.RowScanner) ([]Bucket, error) {
	var out []Bucket
	for rows.Next() {
		b, err := scanBucket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetActive returns the active bucket for (node, style, part) or
// sql.ErrNoRows if none exists.
func GetActive(db *sql.DB, nodeID, styleID int64, partNumber string) (*Bucket, error) {
	b, err := scanBucket(db.QueryRow(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id=? AND style_id=? AND part_number=? AND state=?`,
		nodeID, styleID, partNumber, StateActive))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Find returns any bucket (active or inactive) for (node, style, part)
// or sql.ErrNoRows if none exists. In practice at most one row matches
// because we merge on reactivate.
func Find(db *sql.DB, nodeID, styleID int64, partNumber string) (*Bucket, error) {
	b, err := scanBucket(db.QueryRow(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id=? AND style_id=? AND part_number=?
		ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END
		LIMIT 1`,
		nodeID, styleID, partNumber))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// GetByID returns one bucket by id.
func GetByID(db *sql.DB, id int64) (*Bucket, error) {
	b, err := scanBucket(db.QueryRow(`SELECT `+bucketCols+` FROM node_lineside_bucket WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListForNode returns every bucket on a node, ordered with active rows
// first. Useful for HMI rendering (active bar + stacked chips).
func ListForNode(db *sql.DB, nodeID int64) ([]Bucket, error) {
	rows, err := db.Query(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id=?
		ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END, updated_at DESC`,
		nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// ListActiveForNode returns only the active buckets on a node.
func ListActiveForNode(db *sql.DB, nodeID int64) ([]Bucket, error) {
	rows, err := db.Query(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id=? AND state=?
		ORDER BY updated_at DESC`,
		nodeID, StateActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// ListInactiveForNode returns only the stranded (inactive) buckets on
// a node — the ones that render as stacked chips.
func ListInactiveForNode(db *sql.DB, nodeID int64) ([]Bucket, error) {
	rows, err := db.Query(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id=? AND state=?
		ORDER BY updated_at DESC`,
		nodeID, StateInactive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// ListForPair returns every bucket keyed to a pair, across both
// A/B nodes. Empty pairKey returns an empty slice.
func ListForPair(db *sql.DB, pairKey string) ([]Bucket, error) {
	if pairKey == "" {
		return nil, nil
	}
	rows, err := db.Query(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE pair_key=?
		ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END, updated_at DESC`,
		pairKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBuckets(rows)
}

// Capture records parts pulled to lineside for (node, style, part). It
// merges into an existing bucket when one is present (reactivating an
// inactive one), or creates a fresh active bucket otherwise. A non-zero
// qty is required — Capture with qty == 0 is a no-op and returns nil.
//
// Capture should be called inside a transaction together with
// DeactivateOtherStyles so the single-active-per-(style,part) invariant
// is never transiently violated.
func Capture(db Execer, nodeID int64, pairKey string, styleID int64, partNumber string, qty int) (*Bucket, error) {
	if qty <= 0 {
		return nil, nil
	}

	// Try update-merge first. This hits the unique index cleanly when an
	// active bucket already exists for this (node, style, part); for a
	// stranded inactive bucket we promote and add the qty in one shot.
	res, err := db.Exec(`UPDATE node_lineside_bucket
		SET qty = qty + ?, state = ?, updated_at = datetime('now')
		WHERE node_id=? AND style_id=? AND part_number=?`,
		qty, StateActive, nodeID, styleID, partNumber)
	if err != nil {
		return nil, fmt.Errorf("lineside: capture merge: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected > 0 {
		return findOne(db, nodeID, styleID, partNumber)
	}

	// No row to merge into — insert fresh.
	_, err = db.Exec(`INSERT INTO node_lineside_bucket
		(node_id, pair_key, style_id, part_number, qty, state)
		VALUES (?, ?, ?, ?, ?, ?)`,
		nodeID, pairKey, styleID, partNumber, qty, StateActive)
	if err != nil {
		return nil, fmt.Errorf("lineside: capture insert: %w", err)
	}
	return findOne(db, nodeID, styleID, partNumber)
}

// DeactivateOtherStyles flips any *other* active buckets on this node
// (different style) to inactive, so the post-release state respects
// the "one active style per node" rule. Zero-qty rows are deleted.
//
// Must be called inside the same transaction as Capture.
func DeactivateOtherStyles(db Execer, nodeID, keepStyleID int64) error {
	if _, err := db.Exec(`DELETE FROM node_lineside_bucket
		WHERE node_id=? AND state=? AND style_id != ? AND qty <= 0`,
		nodeID, StateActive, keepStyleID); err != nil {
		return fmt.Errorf("lineside: deactivate delete zeros: %w", err)
	}
	_, err := db.Exec(`UPDATE node_lineside_bucket
		SET state=?, updated_at=datetime('now')
		WHERE node_id=? AND state=? AND style_id != ?`,
		StateInactive, nodeID, StateActive, keepStyleID)
	if err != nil {
		return fmt.Errorf("lineside: deactivate others: %w", err)
	}
	return nil
}

// Drain decrements the active bucket for (node, style, part) by up to
// delta. Returns the amount actually drained from the bucket; the
// caller passes the remainder (delta - drained) to the node-level
// RemainingUOP decrement. Missing active bucket returns 0 with no
// error — the counter tick simply flows through to the node counter.
//
// When the bucket hits zero it is deleted so zero-qty rows don't
// linger in the UI.
func Drain(db Execer, nodeID, styleID int64, partNumber string, delta int) (int, error) {
	if delta <= 0 {
		return 0, nil
	}

	// Read current qty.
	var id int64
	var qty int
	row := db.QueryRow(`SELECT id, qty FROM node_lineside_bucket
		WHERE node_id=? AND style_id=? AND part_number=? AND state=?`,
		nodeID, styleID, partNumber, StateActive)
	if err := row.Scan(&id, &qty); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("lineside: drain read: %w", err)
	}

	take := delta
	if take > qty {
		take = qty
	}
	newQty := qty - take
	if newQty == 0 {
		if _, err := db.Exec(`DELETE FROM node_lineside_bucket WHERE id=?`, id); err != nil {
			return 0, fmt.Errorf("lineside: drain delete: %w", err)
		}
		return take, nil
	}
	if _, err := db.Exec(`UPDATE node_lineside_bucket
		SET qty=?, updated_at=datetime('now') WHERE id=?`, newQty, id); err != nil {
		return 0, fmt.Errorf("lineside: drain update: %w", err)
	}
	return take, nil
}

// Delete removes a bucket by id (used by scrap/repack/recall actions
// wired up later).
func Delete(db Execer, id int64) error {
	_, err := db.Exec(`DELETE FROM node_lineside_bucket WHERE id=?`, id)
	return err
}

// --- helpers ---

// Execer is the minimal interface shared by *sql.DB and *sql.Tx.
// Every mutating function accepts this so callers can wrap a sequence
// of captures + deactivations in a transaction.
type Execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

// findOne is the internal single-row fetch used after Capture. Takes
// an Execer so it works inside a transaction.
func findOne(db Execer, nodeID, styleID int64, partNumber string) (*Bucket, error) {
	b, err := scanBucket(db.QueryRow(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id=? AND style_id=? AND part_number=?
		ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END
		LIMIT 1`,
		nodeID, styleID, partNumber))
	if err != nil {
		return nil, err
	}
	return &b, nil
}
