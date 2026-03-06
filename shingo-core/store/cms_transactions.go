package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type CMSTransaction struct {
	ID            int64     `json:"id"`
	NodeID        int64     `json:"node_id"`
	NodeName      string    `json:"node_name"`
	TxnType       string    `json:"txn_type"`
	CatID         string    `json:"cat_id"`
	Delta         int64     `json:"delta"`
	QtyBefore     int64     `json:"qty_before"`
	QtyAfter      int64     `json:"qty_after"`
	PayloadID     int64     `json:"payload_id"`
	BinID         *int64    `json:"bin_id,omitempty"`
	BinLabel      string    `json:"bin_label"`
	BlueprintCode string    `json:"blueprint_code"`
	SourceType    string    `json:"source_type"`
	OrderID       *int64    `json:"order_id,omitempty"`
	Notes         string    `json:"notes"`
	CreatedAt     time.Time `json:"created_at"`
}

const cmsTxnSelectCols = `id, node_id, node_name, txn_type, cat_id, delta, qty_before, qty_after, payload_id, bin_id, bin_label, blueprint_code, source_type, order_id, notes, created_at`

func scanCMSTransaction(row interface{ Scan(...any) error }) (*CMSTransaction, error) {
	var t CMSTransaction
	var binID sql.NullInt64
	var orderID sql.NullInt64
	var createdAt any
	err := row.Scan(&t.ID, &t.NodeID, &t.NodeName, &t.TxnType, &t.CatID, &t.Delta,
		&t.QtyBefore, &t.QtyAfter, &t.PayloadID, &binID, &t.BinLabel, &t.BlueprintCode,
		&t.SourceType, &orderID, &t.Notes, &createdAt)
	if err != nil {
		return nil, err
	}
	if binID.Valid {
		t.BinID = &binID.Int64
	}
	if orderID.Valid {
		t.OrderID = &orderID.Int64
	}
	t.CreatedAt = parseTime(createdAt)
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
	for _, t := range txns {
		result, err := db.Exec(db.Q(`INSERT INTO cms_transactions (node_id, node_name, txn_type, cat_id, delta, qty_before, qty_after, payload_id, bin_id, bin_label, blueprint_code, source_type, order_id, notes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			t.NodeID, t.NodeName, t.TxnType, t.CatID, t.Delta, t.QtyBefore, t.QtyAfter,
			t.PayloadID, nullableInt64(t.BinID), t.BinLabel, t.BlueprintCode, t.SourceType,
			nullableInt64(t.OrderID), t.Notes)
		if err != nil {
			return fmt.Errorf("create cms transaction: %w", err)
		}
		id, _ := result.LastInsertId()
		t.ID = id
	}
	return nil
}

func (db *DB) ListCMSTransactions(nodeID int64, limit, offset int) ([]*CMSTransaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM cms_transactions WHERE node_id=? ORDER BY id DESC LIMIT ? OFFSET ?`, cmsTxnSelectCols)),
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
	rows, err := db.Query(db.Q(fmt.Sprintf(`SELECT %s FROM cms_transactions ORDER BY id DESC LIMIT ? OFFSET ?`, cmsTxnSelectCols)),
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCMSTransactions(rows)
}

// CollectDescendantNodeIDs returns all node IDs under a boundary (including the
// boundary itself, plus all children and grandchildren).
func (db *DB) CollectDescendantNodeIDs(boundaryID int64) []int64 {
	var ids []int64
	ids = append(ids, boundaryID)
	children, err := db.ListChildNodes(boundaryID)
	if err != nil {
		return ids
	}
	for _, c := range children {
		ids = append(ids, c.ID)
		grandchildren, err := db.ListChildNodes(c.ID)
		if err != nil {
			continue
		}
		for _, gc := range grandchildren {
			ids = append(ids, gc.ID)
		}
	}
	return ids
}

// SumCatIDAtBoundary returns the total manifest quantity (as integer) for a CATID
// across all payloads whose bins sit at nodes under the given boundary.
func (db *DB) SumCatIDAtBoundary(boundaryID int64, catID string) int64 {
	nodeIDs := db.CollectDescendantNodeIDs(boundaryID)
	if len(nodeIDs) == 0 {
		return 0
	}
	placeholders := make([]string, len(nodeIDs))
	args := make([]any, len(nodeIDs))
	for i, id := range nodeIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	args = append(args, catID)
	query := fmt.Sprintf(`SELECT COALESCE(CAST(SUM(mi.quantity) AS INTEGER), 0) FROM manifest_items mi
		JOIN payloads p ON p.id = mi.payload_id
		JOIN bins b ON b.id = p.bin_id
		WHERE b.node_id IN (%s) AND mi.part_number = ?`, strings.Join(placeholders, ","))
	var total int64
	db.QueryRow(db.Q(query), args...).Scan(&total)
	return total
}
