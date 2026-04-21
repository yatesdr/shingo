// Package cms holds CMS-transaction persistence for shingo-core.
//
// Phase 5 of the architecture plan moved cms_transactions CRUD out of
// the flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.CMSTransaction = cms.Transaction`) and
// one-line delegate methods on *store.DB so external callers don't
// change. The cross-aggregate SumCatIDsAtBoundary helper (which reads
// bin manifests) stays at the outer store/ level.
package cms

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/store/internal/helpers"
)

// Transaction is the cms_transactions row entity. The type is re-aliased
// at the outer store/ level as store.CMSTransaction so service/, engine/,
// and material/ compile unchanged.
type Transaction struct {
	ID          int64     `json:"id"`
	NodeID      int64     `json:"node_id"`
	NodeName    string    `json:"node_name"`
	TxnType     string    `json:"txn_type"`
	CatID       string    `json:"cat_id"`
	Delta       int64     `json:"delta"`
	QtyBefore   int64     `json:"qty_before"`
	QtyAfter    int64     `json:"qty_after"`
	BinID       *int64    `json:"bin_id,omitempty"`
	BinLabel    string    `json:"bin_label"`
	PayloadCode string    `json:"payload_code"`
	SourceType  string    `json:"source_type"`
	OrderID     *int64    `json:"order_id,omitempty"`
	Notes       string    `json:"notes"`
	CreatedAt   time.Time `json:"created_at"`
}

const selectCols = `id, node_id, node_name, txn_type, cat_id, delta, qty_before, qty_after, bin_id, bin_label, payload_code, source_type, order_id, notes, created_at`

func scanTransaction(row interface{ Scan(...any) error }) (*Transaction, error) {
	var t Transaction
	var binID sql.NullInt64
	var orderID sql.NullInt64
	err := row.Scan(&t.ID, &t.NodeID, &t.NodeName, &t.TxnType, &t.CatID, &t.Delta,
		&t.QtyBefore, &t.QtyAfter, &binID, &t.BinLabel, &t.PayloadCode,
		&t.SourceType, &orderID, &t.Notes, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if binID.Valid {
		t.BinID = &binID.Int64
	}
	if orderID.Valid {
		t.OrderID = &orderID.Int64
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
		err := tx.QueryRow(`INSERT INTO cms_transactions (node_id, node_name, txn_type, cat_id, delta, qty_before, qty_after, bin_id, bin_label, payload_code, source_type, order_id, notes) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
			t.NodeID, t.NodeName, t.TxnType, t.CatID, t.Delta, t.QtyBefore, t.QtyAfter,
			helpers.NullableInt64(t.BinID), t.BinLabel, t.PayloadCode, t.SourceType,
			helpers.NullableInt64(t.OrderID), t.Notes).Scan(&id)
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
