package store

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/outbox"
	"shingo/protocol/testutil"
)

// outbox_count_subject_test.go — what PurgeOldOutbox deletes, per subject.

// agedOutboxRow enqueues a row of msgType and backdates it: created 8 days ago
// (past the 7-day dead-letter retention), undelivered, and exhausted when
// exhausted is set. A delivered row is sent 2 days ago (past the 24h one).
func agedOutboxRow(t *testing.T, db *DB, msgType string, exhausted, delivered bool) int64 {
	t.Helper()
	id, err := db.EnqueueOutbox([]byte(`{"n":1}`), msgType)
	testutil.MustNoErr(t, err, "enqueue "+msgType)
	retries := 0
	if exhausted {
		retries = outbox.MaxRetries
	}
	_, err = db.Exec(`UPDATE outbox SET created_at = datetime('now', '-8 days'), retries = ? WHERE id = ?`, retries, id)
	testutil.MustNoErr(t, err, "backdate "+msgType)
	if delivered {
		_, err = db.Exec(`UPDATE outbox SET sent_at = datetime('now', '-2 days') WHERE id = ?`, id)
		testutil.MustNoErr(t, err, "mark delivered "+msgType)
	}
	return id
}

func outboxRowExists(t *testing.T, db *DB, id int64) bool {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id = ?`, id).Scan(&n), "count row")
	return n == 1
}

// TestPurgeKeepsAnUndeliveredCountRow is S1's purge half. An undelivered
// bin_uop_delta or lineside_bucket_delta row is never deleted, whatever its age
// or retries: it is the only record of counts Core has not received. (A count
// row is exhausted only by the drainer's panic boundary, or by a failure budget
// spent before S1 deployed.)
//
// Inverted pin: at base (TestPin_P0a_PurgeDeletesAnExhaustedCountRow) such a
// row was deleted once past the 7-day dead-letter retention.
func TestPurgeKeepsAnUndeliveredCountRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bin := agedOutboxRow(t, db, protocol.SubjectBinUOPDelta, true, false)
	bucket := agedOutboxRow(t, db, protocol.SubjectLinesideBucketDelta, true, false)

	_, err := db.PurgeOldOutbox(outbox.MessageRetentionPeriod, outbox.DeadLetterRetentionPeriod)
	testutil.MustNoErr(t, err, "purge")

	if !outboxRowExists(t, db, bin) || !outboxRowExists(t, db, bucket) {
		t.Errorf("the purge deleted an undelivered count row; its counts are then lost for good")
	}
}

// TestPurgeOld_OtherRowsKeepTheirRetention is the control arm, green before and
// after S1: a dead letter of any other subject and a DELIVERED count row are
// still deleted past their retention, and a young exhausted row is kept.
func TestPurgeOld_OtherRowsKeepTheirRetention(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	deadOrder := agedOutboxRow(t, db, protocol.SubjectProductionReport, true, false)
	sentCount := agedOutboxRow(t, db, protocol.SubjectBinUOPDelta, false, true)
	young, err := db.EnqueueOutbox([]byte(`{"n":1}`), protocol.SubjectProductionReport)
	testutil.MustNoErr(t, err, "enqueue young row")
	testutil.MustNoErr(t, db.MarkOutboxExhausted(young, "test"), "exhaust young row")

	_, err = db.PurgeOldOutbox(outbox.MessageRetentionPeriod, outbox.DeadLetterRetentionPeriod)
	testutil.MustNoErr(t, err, "purge")

	if outboxRowExists(t, db, deadOrder) {
		t.Errorf("an old dead letter of an ordinary subject survived the purge")
	}
	if outboxRowExists(t, db, sentCount) {
		t.Errorf("a delivered count row past the delivered retention survived the purge")
	}
	if !outboxRowExists(t, db, young) {
		t.Errorf("a dead letter younger than the dead-letter retention was purged")
	}
}
