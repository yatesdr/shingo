package store

// Phase 5 delegate file: cms_transactions CRUD lives in store/cms/.

import (
	"time"

	"shingocore/store/cms"
)

func (db *DB) CreateCMSTransactions(txns []*cms.Transaction) error {
	return cms.Create(db.DB, txns)
}

func (db *DB) ListCMSTransactions(nodeID int64, limit, offset int) ([]*cms.Transaction, error) {
	return cms.ListByNode(db.DB, nodeID, limit, offset)
}

func (db *DB) ListAllCMSTransactions(limit, offset int) ([]*cms.Transaction, error) {
	return cms.ListAll(db.DB, limit, offset)
}

// ListUnpostedCMSTransactions returns transactions no posting has claimed and
// that are older than age — the poster's orphan sweep.
func (db *DB) ListUnpostedCMSTransactions(age time.Duration, limit int) ([]*cms.Transaction, error) {
	return cms.ListUnpostedOlderThan(db.DB, age, limit)
}

// ListCMSTransactionsByPosting returns every transaction a posting carries.
func (db *DB) ListCMSTransactionsByPosting(postingID int64) ([]*cms.Transaction, error) {
	return cms.ListByPosting(db.DB, postingID)
}

// AttachCMSPosting claims transactions for a posting, returning how many it took.
func (db *DB) AttachCMSPosting(txnIDs []int64, postingID int64) (int, error) {
	return cms.AttachPosting(db.DB, txnIDs, postingID)
}

// --- cms_postings ---------------------------------------------------------

func (db *DB) CreateCMSPosting(p *cms.Posting) error { return cms.CreatePosting(db.DB, p) }

// CMSPostingHealth reads the posting queue's state. window bounds which
// terminal rows still count toward the verdict; see cms.Health.
func (db *DB) CMSPostingHealth(window time.Duration) (*cms.Health, error) {
	return cms.PostingHealth(db.DB, window)
}

func (db *DB) NextPendingCMSPostings(limit int) ([]*cms.Posting, error) {
	return cms.NextPending(db.DB, limit)
}

func (db *DB) MarkCMSPostingInflight(id int64, bodySHA string) error {
	return cms.MarkInflight(db.DB, id, bodySHA)
}

func (db *DB) MarkCMSPostingTransactionID(id int64, transactionID string) error {
	return cms.MarkTransactionID(db.DB, id, transactionID)
}

func (db *DB) MarkCMSPostingPosted(id int64, transactionID string, httpStatus int) error {
	return cms.MarkPosted(db.DB, id, transactionID, httpStatus)
}

func (db *DB) MarkCMSPostingRejected(id int64, httpStatus int, lastErr string) error {
	return cms.MarkRejected(db.DB, id, httpStatus, lastErr)
}

func (db *DB) MarkCMSPostingFailed(id int64, lastErr string) error {
	return cms.MarkFailed(db.DB, id, lastErr)
}

func (db *DB) MarkCMSPostingPending(id int64, lastErr string, nextRetry time.Time) error {
	return cms.MarkPending(db.DB, id, lastErr, nextRetry)
}

func (db *DB) RecordCMSPostingAfterSendError(id int64, lastErr string) error {
	return cms.RecordAfterSendError(db.DB, id, lastErr)
}

func (db *DB) ListInflightCMSPostingsOlderThan(d time.Duration) ([]*cms.Posting, error) {
	return cms.ListInflightOlderThan(db.DB, d)
}

func (db *DB) RequeueCMSPosting(id int64) error { return cms.RequeuePending(db.DB, id) }
