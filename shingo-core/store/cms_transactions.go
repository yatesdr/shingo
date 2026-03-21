package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type CMSTransaction struct {
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

const cmsTxnSelectCols = `id, node_id, node_name, txn_type, cat_id, delta, qty_before, qty_after, bin_id, bin_label, payload_code, source_type, order_id, notes, created_at`

func scanCMSTransaction(row interface{ Scan(...any) error }) (*CMSTransaction, error) {
	var t CMSTransaction
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

func scanCMSTransactions(rows *sql.Rows) ([]*CMSTransaction, error) {
	var txns []*CMSTransaction
	for rows.Next() {
		t, err := scanCMSTransaction(rows)
		if err != nil {
			return nil, err
		}
		txns = append(txns, t)
	}
	return txns, rows.Err()
}

func (db *DB) CreateCMSTransactions(txns []*CMSTransaction) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin cms tx: %w", err)
	}
	defer tx.Rollback()
	for _, t := range txns {
		var id int64
		err := tx.QueryRow(`INSERT INTO cms_transactions (node_id, node_name, txn_type, cat_id, delta, qty_before, qty_after, bin_id, bin_label, payload_code, source_type, order_id, notes) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
			t.NodeID, t.NodeName, t.TxnType, t.CatID, t.Delta, t.QtyBefore, t.QtyAfter,
			nullableInt64(t.BinID), t.BinLabel, t.PayloadCode, t.SourceType,
			nullableInt64(t.OrderID), t.Notes).Scan(&id)
		if err != nil {
			return fmt.Errorf("create cms transaction: %w", err)
		}
		t.ID = id
	}
	return tx.Commit()
}

func (db *DB) ListCMSTransactions(nodeID int64, limit, offset int) ([]*CMSTransaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions WHERE node_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, cmsTxnSelectCols),
		nodeID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCMSTransactions(rows)
}

func (db *DB) ListAllCMSTransactions(limit, offset int) ([]*CMSTransaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions ORDER BY id DESC LIMIT $1 OFFSET $2`, cmsTxnSelectCols),
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCMSTransactions(rows)
}

// SumCatIDsAtBoundary returns total manifest quantities for all CATIDs
// across all bins at nodes under the given boundary, parsing from bin manifest JSON.
func (db *DB) SumCatIDsAtBoundary(boundaryID int64) map[string]int64 {
	totals := make(map[string]int64)
	rows, err := db.Query(`
		WITH RECURSIVE descendants AS (
			SELECT id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id FROM nodes n
			JOIN descendants d ON n.parent_id = d.id
		)
		SELECT b.manifest FROM bins b
		JOIN descendants d ON b.node_id = d.id
		WHERE b.manifest IS NOT NULL
	`, boundaryID)
	if err != nil {
		return totals
	}
	defer rows.Close()

	for rows.Next() {
		var manifestJSON string
		if rows.Scan(&manifestJSON) != nil {
			continue
		}
		var m BinManifest
		if json.Unmarshal([]byte(manifestJSON), &m) != nil {
			continue
		}
		for _, item := range m.Items {
			totals[item.CatID] += item.Quantity
		}
	}
	return totals
}
