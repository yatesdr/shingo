// Package cms holds CMS-transaction persistence for shingo-core.
//
// Phase 5 of the architecture plan moved cms_transactions CRUD out of
// the flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.CMSTransaction = cms.Transaction`) and
// one-line delegate methods on *store.DB so external callers don't
// change.
package cms

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingocore/store/internal/helpers"
)

// Transaction is the cms_transactions row entity. The type is re-aliased
// at the outer store/ level as store.CMSTransaction so service/, engine/,
// and material/ compile unchanged.
// Delta is signed and is the whole story: negative leaves the boundary,
// positive arrives at it. There is no TxnType — that column stored sign(delta)
// beside the delta, so the two could disagree and one of them would be a lie.
// There are no QtyBefore/QtyAfter either: they were computed at build time from
// a recursive subtree scan, which is not sound under concurrent moves, and the
// only reader was the diagnostics table.
type Transaction struct {
	ID          int64  `json:"id"`
	NodeID      int64  `json:"node_id"`
	NodeName    string `json:"node_name"`
	CatID       string `json:"cat_id"`
	Delta       int64  `json:"delta"`
	BinID       *int64 `json:"bin_id,omitempty"`
	BinLabel    string `json:"bin_label"`
	PayloadCode string `json:"payload_code"`
	SourceType  string `json:"source_type"`
	OrderID     *int64 `json:"order_id,omitempty"`
	// Storeroom is the CMS code of the boundary this row is about, stamped at
	// build time from the node property. It travels on the row so the wire
	// layer needs no reach-back into the node tree — a translator that had to
	// look it up could not be a pure function.
	Storeroom string `json:"storeroom"`
	// RobotID is the AMR that carried the bin, captured from the event.
	// Blank for an operator drag, which has no robot.
	RobotID string `json:"robot_id"`
	// PostingID is NULL until a posting claims this row. NULL is the queue:
	// "recorded locally, not yet sent".
	PostingID *int64    `json:"posting_id,omitempty"`
	Notes     string    `json:"notes"`
	CreatedAt time.Time `json:"created_at"`
}

const selectCols = `id, node_id, node_name, cat_id, delta, bin_id, bin_label, payload_code, source_type, order_id, storeroom, robot_id, posting_id, notes, created_at`

func scanTransaction(row interface{ Scan(...any) error }) (*Transaction, error) {
	var t Transaction
	var binID sql.NullInt64
	var orderID sql.NullInt64
	var postingID sql.NullInt64
	err := row.Scan(&t.ID, &t.NodeID, &t.NodeName, &t.CatID, &t.Delta,
		&binID, &t.BinLabel, &t.PayloadCode,
		&t.SourceType, &orderID, &t.Storeroom, &t.RobotID, &postingID,
		&t.Notes, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if binID.Valid {
		t.BinID = &binID.Int64
	}
	if orderID.Valid {
		t.OrderID = &orderID.Int64
	}
	if postingID.Valid {
		t.PostingID = &postingID.Int64
	}
	return &t, nil
}

func scanTransactions(rows *sql.Rows) ([]*Transaction, error) {
	var txns []*Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		txns = append(txns, t)
	}
	return txns, rows.Err()
}

// Create inserts the given cms_transactions rows in a single transaction
// and sets each row's ID on success.
func Create(db *sql.DB, txns []*Transaction) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin cms tx: %w", err)
	}
	defer tx.Rollback()
	for _, t := range txns {
		var id int64
		// posting_id is deliberately not inserted: a new row is unsent by
		// definition, and NULL is what the unposted index selects on.
		err := tx.QueryRow(`INSERT INTO cms_transactions (node_id, node_name, cat_id, delta, bin_id, bin_label, payload_code, source_type, order_id, storeroom, robot_id, notes) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
			t.NodeID, t.NodeName, t.CatID, t.Delta,
			helpers.NullableInt64(t.BinID), t.BinLabel, t.PayloadCode, t.SourceType,
			helpers.NullableInt64(t.OrderID), t.Storeroom, t.RobotID, t.Notes).Scan(&id)
		if err != nil {
			return fmt.Errorf("create cms transaction: %w", err)
		}
		t.ID = id
	}
	return tx.Commit()
}

// ListByNode returns the most recent cms_transactions for a node.
func ListByNode(db *sql.DB, nodeID int64, limit, offset int) ([]*Transaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions WHERE node_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, selectCols),
		nodeID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransactions(rows)
}

// ListAll returns the most recent cms_transactions across all nodes.
func ListAll(db *sql.DB, limit, offset int) ([]*Transaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions ORDER BY id DESC LIMIT $1 OFFSET $2`, selectCols),
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransactions(rows)
}

// ListUnposted returns transactions no posting has claimed yet, oldest first.
//
// posting_id IS NULL is the queue. It is a structural fact rather than a status
// column: a row either belongs to a POST or it does not, and there is no third
// state to fall out of step with the posting's own lifecycle.
func ListUnposted(db *sql.DB, limit int) ([]*Transaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions
		WHERE posting_id IS NULL ORDER BY id LIMIT $1`, selectCols), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransactions(rows)
}

// ListByPosting returns every transaction a posting carries, ordered by id.
//
// The ORDER is the wire order. EntryNumber is assigned over this sequence, so
// a stable sort here is what makes the same posting serialise identically on a
// retry — and a retry that serialises differently is a different body, which
// defeats body_sha as a dedup key.
func ListByPosting(db *sql.DB, postingID int64) ([]*Transaction, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions
		WHERE posting_id=$1 ORDER BY id`, selectCols), postingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTransactions(rows)
}

// AttachPosting claims the given transactions for a posting.
//
// Guarded on posting_id IS NULL so a row cannot be claimed twice — two postings
// carrying the same transaction would book the same transfer twice, and the
// guard is cheaper than discovering that in CMS. The row count is returned so
// the caller can tell a partial claim from a complete one.
func AttachPosting(db *sql.DB, txnIDs []int64, postingID int64) (int, error) {
	if len(txnIDs) == 0 {
		return 0, nil
	}
	// A positional IN list rather than pq.Array: this package takes a bare
	// *sql.DB with the driver wired above it, and store/bins and store/nodes
	// both do the same for the same reason. One dependency-free idiom beats
	// three packages disagreeing.
	ph := make([]string, len(txnIDs))
	args := make([]any, 0, len(txnIDs)+1)
	args = append(args, postingID)
	for i, id := range txnIDs {
		ph[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, id)
	}
	res, err := db.Exec(fmt.Sprintf(`UPDATE cms_transactions SET posting_id=$1
		WHERE id IN (%s) AND posting_id IS NULL`, strings.Join(ph, ", ")), args...)
	if err != nil {
		return 0, fmt.Errorf("attach posting %d: %w", postingID, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
