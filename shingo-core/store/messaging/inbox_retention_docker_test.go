//go:build docker

package messaging_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/messaging"
)

// TestCoverage_PurgeOldInbox closes the retention gap between inbox and its
// outbox twin. This will never be a size fix — 5,525 rows and 1.2 MB after
// 123 days on the busiest plant — so what is worth pinning is that the
// cutoff is honoured in both directions and that expiring a record does not
// break the idempotency it provides while it is alive.
//
// Verified red: with PurgeOldInbox's WHERE clause removed (`DELETE FROM
// inbox`), this fails with "purged 2 rows, want 1" and then "a record
// inside the window was purged".
func TestCoverage_PurgeOldInbox(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if _, err := db.Exec(
		`INSERT INTO inbox (msg_id, msg_type, station_id, processed_at) VALUES ($1, $2, $3, $4)`,
		"msg-old", "order.ack", "line-1", time.Now().UTC().AddDate(0, 0, -120)); err != nil {
		t.Fatalf("insert old inbox row: %v", err)
	}
	isNew, err := db.RecordInboundMessage("msg-recent", "order.ack", "line-1")
	if err != nil || !isNew {
		t.Fatalf("record recent: isNew=%v err=%v", isNew, err)
	}

	n, err := db.PurgeOldInbox(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d rows, want 1", n)
	}

	// The recent record still suppresses its replay.
	if isNew, err := db.RecordInboundMessage("msg-recent", "order.ack", "line-1"); err != nil || isNew {
		t.Errorf("a record inside the window was purged: isNew=%v err=%v", isNew, err)
	}
	// The expired one no longer does — the accepted cost of retaining
	// dedup records for a finite time, three months against a redelivery
	// window the outbox bounds at 24 hours.
	if isNew, err := db.RecordInboundMessage("msg-old", "order.ack", "line-1"); err != nil || !isNew {
		t.Errorf("expired record still suppressing: isNew=%v err=%v", isNew, err)
	}
}

// TestPurgeOldInbox_PreservesEverythingInsideTheWindow is the n=0 arm: a
// purge that finds nothing to do must say so rather than quietly deleting.
func TestPurgeOldInbox_PreservesEverythingInsideTheWindow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if isNew, err := db.RecordInboundMessage("msg-fresh", "order.ack", "line-1"); err != nil || !isNew {
		t.Fatalf("record: isNew=%v err=%v", isNew, err)
	}
	n, err := db.PurgeOldInbox(messaging.InboxRetentionPeriod)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Errorf("purged %d rows, want 0 (the retention window covers it)", n)
	}
}
