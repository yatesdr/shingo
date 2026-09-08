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
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"shingocore/store/internal/helpers"
)

// Source types for cms_transactions.source_type. Spelled once so a typo is a
// compile error rather than a row no query will ever match — the same reason
// the posting statuses next door are constants.
//
// Which of them reach the middleware is Postable's answer, below — the
// subscriber in engine/wiring.go asks it rather than comparing literals, because
// a typo in that comparison drops every posting on the floor while the
// transactions keep being recorded, which looks from every count like a plant
// that is not moving anything.
const (
	SourceTypeMovement = "movement"
	// SourceTypeClear is the unloader emptying a carrier: material leaving a
	// tagged storeroom for a CMS zone shingo cannot see. ONE-SIDED BY
	// CONSTRUCTION, which is why it is its own type rather than a movement with
	// a missing half — a movement is a pair and a reader is entitled to expect
	// the other row exists.
	SourceTypeClear = "clear"
	// SourceTypeCorrection is HISTORICAL ONLY. The path that wrote it was
	// removed; the value is kept here so the diagnostics filter and anyone
	// reading old rows share one vocabulary with the writers.
	SourceTypeCorrection = "correction"
)

// Postable answers whether a source type belongs on the inventory feed — the
// question engine/wiring.go's subscriber asks of every recorded row before it
// becomes a posting.
//
// IT LIVES HERE, TWO LINES UNDER THE VOCABULARY, so that adding a source type
// forces the decision in the same edit. A FACT WITH NO READER IS NOT DONE: a
// type the subscriber does not accept writes cms_transactions rows that never
// become postings and never reach CMS, and every count on the health page then
// describes a plant that moved nothing. That class is behind three of this
// codebase's incidents, and the filter being a bare literal in another package
// is what made it easy to ship the fact without its consumer.
//
// SourceTypeCorrection is deliberately NOT postable. The path that wrote it is
// deleted and its rows are historical; re-introducing corrections has to argue
// that a correction belongs on an inventory-TRANSFER feed rather than arriving
// on it by inheritance.
func Postable(sourceType string) bool {
	return sourceType == SourceTypeMovement || sourceType == SourceTypeClear
}

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
	ID     int64 `json:"id"`
	NodeID int64 `json:"node_id"`
	// NodeName is the BOUNDARY -- the tagged ancestor the location resolved
	// to, which is what Storeroom derives from. It is frequently a GROUP in
	// the node tree rather than a place a carrier can stand
	// (`Supermarket Area`), so it is not the answer to "where was this".
	// LocationNodeName is.
	NodeName string `json:"node_name"`
	// LocationNodeID / LocationNodeName are WHERE THE MATERIAL ACTUALLY WAS:
	// the concrete node the walk started from, before it climbed to the
	// boundary above. Kept because the boundary cannot be walked back down --
	// it is an ancestor of many nodes -- so a row that stored only the
	// boundary had permanently lost the place.
	//
	// Zero/empty on rows written before v111, and on any path that has no
	// concrete node to name. Never inferred from the boundary.
	LocationNodeID   int64  `json:"location_node_id,omitempty"`
	LocationNodeName string `json:"location_node_name"`
	CatID            string `json:"cat_id"`
	Delta            int64  `json:"delta"`
	BinID            *int64 `json:"bin_id,omitempty"`
	BinLabel         string `json:"bin_label"`
	PayloadCode      string `json:"payload_code"`
	SourceType       string `json:"source_type"`
	OrderID          *int64 `json:"order_id,omitempty"`
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

const selectCols = `id, node_id, node_name, location_node_id, location_node_name, cat_id, delta, bin_id, bin_label, payload_code, source_type, order_id, storeroom, robot_id, posting_id, notes, created_at`

func scanTransaction(row interface{ Scan(...any) error }) (*Transaction, error) {
	var t Transaction
	var binID sql.NullInt64
	var orderID sql.NullInt64
	var postingID sql.NullInt64
	var locationNodeID sql.NullInt64
	err := row.Scan(&t.ID, &t.NodeID, &t.NodeName,
		&locationNodeID, &t.LocationNodeName, &t.CatID, &t.Delta,
		&binID, &t.BinLabel, &t.PayloadCode,
		&t.SourceType, &orderID, &t.Storeroom, &t.RobotID, &postingID,
		&t.Notes, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if locationNodeID.Valid {
		t.LocationNodeID = locationNodeID.Int64
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
	if err := CreateInTx(tx, txns); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateInTx inserts the rows inside a transaction the CALLER owns, and sets
// each row's ID on success.
//
// IT EXISTS FOR THE CLEAR PATH, where the rows and the act they describe have to
// commit together. A clear destroys the bin contents the quantities were derived
// from, so rows written after it cannot be reconstructed if the write fails —
// and rows committed before it become a departure the plant never made if the
// clear then fails. One transaction is the only arrangement with neither
// failure. Same family as MarkInflight committing before the POST: the durable
// record and the irreversible act are ordered on purpose.
func CreateInTx(q helpers.QueryRower, txns []*Transaction) error {
	skipped := 0
	for _, t := range txns {
		// posting_id is deliberately not inserted: a new row is unsent by
		// definition, and NULL is what the unposted index selects on.
		//
		// notes is not inserted either, and for a different reason: nothing
		// stamps a note on a new row, so binding it wrote an empty string
		// through twelve placeholders and made the column look like a field
		// this path fills. The COLUMN stays — historical rows carry notes and
		// the diagnostics table still reads them — and the default supplies
		// the empty string for new ones.
		var locationNodeID any
		if t.LocationNodeID != 0 {
			locationNodeID = t.LocationNodeID
		}
		// ON CONFLICT DO NOTHING against idx_cms_txn_one_movement (v112): more
		// than one engine emitter can report the same arrival, and booking it
		// twice is a transfer the plant never made. The conflict is the NORMAL
		// outcome of a second report, not an error — the row it would duplicate
		// is already there and already queued.
		//
		// A skipped row returns no id, so it keeps t.ID zero and is reported to
		// the caller. It must NOT abort the batch: on the clear path these rows
		// commit inside the caller's transaction alongside the act they
		// describe, and failing there would refuse a clear because the ledger
		// already knew about it.
		id, err := helpers.InsertID(q, `INSERT INTO cms_transactions (node_id, node_name, location_node_id, location_node_name, cat_id, delta, bin_id, bin_label, payload_code, source_type, order_id, storeroom, robot_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) ON CONFLICT DO NOTHING RETURNING id`,
			t.NodeID, t.NodeName, locationNodeID, t.LocationNodeName, t.CatID, t.Delta,
			helpers.NullableInt64(t.BinID), t.BinLabel, t.PayloadCode, t.SourceType,
			helpers.NullableInt64(t.OrderID), t.Storeroom, t.RobotID)
		if errors.Is(err, sql.ErrNoRows) {
			skipped++
			continue
		}
		if err != nil {
			return fmt.Errorf("create cms transaction: %w", err)
		}
		t.ID = id
	}
	if skipped > 0 {
		log.Printf("cms: %d of %d movement row(s) were already recorded — a second emitter reported the same arrival; not booking it twice",
			skipped, len(txns))
	}
	return nil
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

// ListUnpostedOlderThan returns transactions no posting has claimed, older than
// age, oldest first. It is the poster's orphan sweep.
//
// posting_id IS NULL is the queue. It is a structural fact rather than a status
// column: a row either belongs to a POST or it does not, and there is no third
// state to fall out of step with the posting's own lifecycle.
//
// THE AGE IS WHAT KEEPS THIS OFF THE HAPPY PATH. A transaction is written and
// its posting is created moments later, on a different goroutine, so a sweep
// with no grace period would race the subscriber for every row the system
// produces. The claim is guarded (AttachPosting takes only NULL rows), so the
// race is not a correctness problem — it is churn, a posting created and
// immediately failed for having taken nothing. A grace period longer than the
// enqueue takes removes it.
//
// Legacy rows are not in scope and need no clause here: v102 backfilled them
// to the 0 sentinel, so they are not NULL. That is the same backfill that keeps
// them out of the health count.
func ListUnpostedOlderThan(db *sql.DB, age time.Duration, limit int) ([]*Transaction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM cms_transactions
		WHERE posting_id IS NULL AND created_at < NOW() - $1::interval
		ORDER BY id LIMIT $2`, selectCols),
		fmt.Sprintf("%d milliseconds", age.Milliseconds()), limit)
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
