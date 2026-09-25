// Package lineside holds persistence for node_lineside_bucket: the parts an
// operator pulled from a bin to the bench at a node. A pile is one row per
// (node, payload, state):
//
//   - active:   on-hand from the pull until the node's cutover. Consume ticks
//     drain it before the node's bin (Drain), and Core counts it.
//   - stranded: what an active pile had left at a cutover (StrandProcess). A
//     permanent count-anomaly record: operators run out what they pull, so
//     a leftover is most likely the size of a declaration error. It never
//     drains, never counts, and never revives; the next pull of that part
//     makes a new active pile.
//
// A row whose qty reaches 0 is deleted, so a level of 0 means "no row". Core
// mirrors each (core node, payload, state) by its level (Level), which the
// delta accumulator reads at flush; every function here that writes a row is
// one the accumulator must be told about (the Key of the row it wrote).
//
// The outer store/ package keeps delegate methods on *store.DB and
// re-exports the state constants; callers name this package's types directly.
package lineside

import (
	"database/sql"
	"errors"
	"fmt"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Bucket is one row of node_lineside_bucket. The struct lives in
// shingoedge/domain under the more descriptive name LinesideBucket.
type Bucket = domain.LinesideBucket

// Pile states.
const (
	StateActive   = "active"
	StateStranded = "stranded"
)

// Key names one pile level as Core mirrors it: (CoreNodeName, PayloadCode,
// State). NodeID is the local process node the row sits on, kept so a missing
// core name can still be resolved; two local nodes with one core name give two
// Keys with one level.
type Key struct {
	NodeID       int64
	CoreNodeName string
	PayloadCode  string
	State        string
}

// Stranded is one active pile a cutover folded into its stranded row.
type Stranded struct {
	NodeID       int64
	CoreNodeName string
	PayloadCode  string
	Qty          int
}

const bucketCols = `id, node_id, payload_code, qty, state, created_at, updated_at`

func scanBucket(scanner interface{ Scan(...any) error }) (Bucket, error) {
	var b Bucket
	var createdAt, updatedAt string
	if err := scanner.Scan(&b.ID, &b.NodeID, &b.PayloadCode,
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

func scanKeys(rows *sql.Rows, qErr error) ([]Key, error) {
	if qErr != nil {
		return nil, qErr
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.NodeID, &k.CoreNodeName, &k.PayloadCode, &k.State); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetByID returns one pile by id.
func GetByID(db *sql.DB, id int64) (*Bucket, error) {
	b, err := scanBucket(db.QueryRow(`SELECT `+bucketCols+` FROM node_lineside_bucket WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// ListForNode returns every pile on a node, active rows first.
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

// ListActiveForNodes returns the active piles of a whole set of nodes in one
// query, keyed by node id. An operator-station view needs these for every
// tile, and a per-node query in the tile loop cost one query per tile on a
// connection that serialises every read (store.Open sets SetMaxOpenConns(1)).
// Nodes with no piles are absent from the map.
func ListActiveForNodes(db *sql.DB, nodeIDs []int64) (map[int64][]Bucket, error) {
	return listForNodes(db, nodeIDs, StateActive)
}

// ListStrandedForNodes is ListActiveForNodes for the stranded piles, which the
// HMI shows as count-anomaly chips.
func ListStrandedForNodes(db *sql.DB, nodeIDs []int64) (map[int64][]Bucket, error) {
	return listForNodes(db, nodeIDs, StateStranded)
}

func listForNodes(db *sql.DB, nodeIDs []int64, state string) (map[int64][]Bucket, error) {
	out := map[int64][]Bucket{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	// Deduplicate: a station can list the same node twice (a changeover
	// participant adopted as a child tile), and a duplicated id would otherwise
	// duplicate that node's piles in the result.
	seen := make(map[int64]bool, len(nodeIDs))
	args := make([]any, 0, len(nodeIDs)+1)
	placeholders := make([]byte, 0, len(nodeIDs)*2)
	for _, id := range nodeIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(placeholders) > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	args = append(args, state)
	rows, err := db.Query(`SELECT `+bucketCols+`
		FROM node_lineside_bucket
		WHERE node_id IN (`+string(placeholders)+`) AND state=?
		ORDER BY updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buckets, err := scanBuckets(rows)
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		out[b.NodeID] = append(out[b.NodeID], b)
	}
	return out, nil
}

// Capture adds qty pulled to lineside to the node's ACTIVE pile of the
// payload, creating it when there is none, and returns the pile's new qty. It
// never reads or writes a stranded row: a part stranded at an earlier cutover
// stays stranded, and this pull is a new active pile beside it. qty <= 0 is a
// no-op returning 0.
func Capture(db Execer, nodeID int64, payloadCode string, qty int) (int, error) {
	if qty <= 0 {
		return 0, nil
	}
	var newQty int
	if err := db.QueryRow(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (node_id, payload_code, state)
		DO UPDATE SET qty = qty + excluded.qty, updated_at = datetime('now')
		RETURNING qty`,
		nodeID, payloadCode, qty, StateActive).Scan(&newQty); err != nil {
		return 0, fmt.Errorf("lineside: capture: %w", err)
	}
	return newQty, nil
}

// Drain decrements the node's ACTIVE pile of the payload by up to delta and
// returns the amount it took; the caller passes the remainder (delta -
// drained) to the node's bin count. No active pile returns (0, nil): the tick
// flows through to the bin. A stranded pile is never drained.
//
// Style is not part of the match, and is not part of the pile: during a
// changeover the pile captured at the release keeps draining while the process
// still runs the from-style, until the cutover strands it (the 2026-05-19 fix,
// kept).
//
// A pile that reaches zero is deleted.
func Drain(db Execer, nodeID int64, payloadCode string, delta int) (int, error) {
	if delta <= 0 {
		return 0, nil
	}
	var id int64
	var qty int
	if err := db.QueryRow(`SELECT id, qty FROM node_lineside_bucket
		WHERE node_id=? AND payload_code=? AND state=?`,
		nodeID, payloadCode, StateActive).Scan(&id, &qty); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("lineside: drain read: %w", err)
	}

	take := min(delta, qty)
	if qty-take <= 0 {
		if _, err := db.Exec(`DELETE FROM node_lineside_bucket WHERE id=?`, id); err != nil {
			return 0, fmt.Errorf("lineside: drain delete: %w", err)
		}
		return take, nil
	}
	if _, err := db.Exec(`UPDATE node_lineside_bucket
		SET qty=?, updated_at=datetime('now') WHERE id=?`, qty-take, id); err != nil {
		return 0, fmt.Errorf("lineside: drain update: %w", err)
	}
	return take, nil
}

// DeleteByID removes one pile, whatever its state: the admin Clear.
func DeleteByID(db Execer, id int64) error {
	if _, err := db.Exec(`DELETE FROM node_lineside_bucket WHERE id=?`, id); err != nil {
		return fmt.Errorf("lineside: delete %d: %w", id, err)
	}
	return nil
}

// StrandProcess is the cutover: in one transaction, every ACTIVE pile at any
// of the process's nodes folds into its (node, payload) stranded row (qty
// summed, updated_at touched) and the active row is deleted. Returns what it
// folded, so the caller can log each as a count anomaly and send both levels.
// A node the changeover does not touch is stranded too: the rule is the
// process's style flip, not the node's swap.
func StrandProcess(db *sql.DB, processID int64) ([]Stranded, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("lineside: strand process %d: begin: %w", processID, err)
	}
	defer tx.Rollback()

	type activePile struct {
		id int64
		Stranded
	}
	rows, err := tx.Query(`SELECT b.id, b.node_id, pn.core_node_name, b.payload_code, b.qty
		FROM node_lineside_bucket b
		JOIN process_nodes pn ON pn.id = b.node_id
		WHERE pn.process_id = ? AND b.state = ?
		ORDER BY b.id`, processID, StateActive)
	if err != nil {
		return nil, fmt.Errorf("lineside: strand process %d: read: %w", processID, err)
	}
	var piles []activePile
	for rows.Next() {
		var p activePile
		if err := rows.Scan(&p.id, &p.NodeID, &p.CoreNodeName, &p.PayloadCode, &p.Qty); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lineside: strand process %d: scan: %w", processID, err)
		}
		piles = append(piles, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lineside: strand process %d: rows: %w", processID, err)
	}

	out := make([]Stranded, 0, len(piles))
	for _, p := range piles {
		if p.Qty > 0 {
			if _, err := tx.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state)
				VALUES (?, ?, ?, ?)
				ON CONFLICT (node_id, payload_code, state)
				DO UPDATE SET qty = qty + excluded.qty, updated_at = datetime('now')`,
				p.NodeID, p.PayloadCode, p.Qty, StateStranded); err != nil {
				return nil, fmt.Errorf("lineside: strand pile %d: %w", p.id, err)
			}
			out = append(out, p.Stranded)
		}
		if _, err := tx.Exec(`DELETE FROM node_lineside_bucket WHERE id=?`, p.id); err != nil {
			return nil, fmt.Errorf("lineside: strand pile %d: delete active: %w", p.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("lineside: strand process %d: commit: %w", processID, err)
	}
	return out, nil
}

// ListKeys returns the Key of every pile row: the boot resend of every level.
func ListKeys(db *sql.DB) ([]Key, error) {
	return scanKeys(db.Query(`SELECT b.node_id, COALESCE(pn.core_node_name, ''), b.payload_code, b.state
		FROM node_lineside_bucket b
		LEFT JOIN process_nodes pn ON pn.id = b.node_id
		ORDER BY b.id`))
}

// ListKeysForProcess returns the Key of every pile row at the process's nodes:
// what a process delete takes with it, read before the delete so each level
// can be sent as 0 after it.
func ListKeysForProcess(db *sql.DB, processID int64) ([]Key, error) {
	return scanKeys(db.Query(`SELECT b.node_id, pn.core_node_name, b.payload_code, b.state
		FROM node_lineside_bucket b
		JOIN process_nodes pn ON pn.id = b.node_id
		WHERE pn.process_id = ?
		ORDER BY b.id`, processID))
}

// Level is the qty Core mirrors for (coreNodeName, payloadCode, state): the sum
// over every process node carrying that core name, since two local nodes with
// one core name are one place at Core. 0 when there is no row. One statement;
// the accumulator reads it once per dirty key per flush.
func Level(db *sql.DB, coreNodeName, payloadCode, state string) (int, error) {
	var qty int
	if err := db.QueryRow(`SELECT COALESCE(SUM(b.qty), 0)
		FROM node_lineside_bucket b
		JOIN process_nodes pn ON pn.id = b.node_id
		WHERE pn.core_node_name = ? AND b.payload_code = ? AND b.state = ?`,
		coreNodeName, payloadCode, state).Scan(&qty); err != nil {
		return 0, fmt.Errorf("lineside: level %s/%s/%s: %w", coreNodeName, payloadCode, state, err)
	}
	return qty, nil
}

// Execer is the minimal interface shared by *sql.DB and *sql.Tx.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}
