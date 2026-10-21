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

// TestAskReplay_TheInboxRecordsItsAnswer pins the store half of an ask's
// replay: an answer enqueued with its row id rings the drainer like any send;
// the ask's inbox row records that id and reads it back; a sent answer is put
// back in the drain by ReplayOutbox (sent_at cleared, retries reset); a row
// that is gone answers false; a message with no recorded answer reads not ok.
func TestAskReplay_TheInboxRecordsItsAnswer(t *testing.T) {
	db := testdb.Open(t)
	rings := 0
	messaging.SetEnqueueNotifier(func() { rings++ })
	t.Cleanup(func() { messaging.SetEnqueueNotifier(nil) })

	id, err := db.EnqueueOutboxReturningID("shingo.dispatch", []byte("answer"), "data.reply.edge.answer", "line-1")
	if err != nil || id == 0 {
		t.Fatalf("enqueue: id %d err %v", id, err)
	}
	if rings != 1 {
		t.Errorf("rings after the answer's enqueue = %d, want 1", rings)
	}
	if err := db.AckOutbox(id); err != nil {
		t.Fatalf("ack: %v", err)
	}

	if inserted, err := db.RecordInboundMessage("ask-1", "edge.ask", "line-1"); err != nil || !inserted {
		t.Fatalf("record ask: inserted %v err %v", inserted, err)
	}
	if _, ok, err := db.InboundReply("ask-1"); err != nil || ok {
		t.Errorf("before its answer is recorded: ok %v err %v, want not ok", ok, err)
	}
	if err := db.RecordInboundReply("ask-1", id); err != nil {
		t.Fatalf("record reply: %v", err)
	}
	if got, ok, err := db.InboundReply("ask-1"); err != nil || !ok || got != id {
		t.Fatalf("recorded answer = %d ok %v err %v, want %d", got, ok, err, id)
	}
	if _, ok, err := db.InboundReply("never-seen"); err != nil || ok {
		t.Errorf("an unseen message: ok %v err %v, want not ok", ok, err)
	}

	sent, err := db.ReplayOutbox(id)
	if err != nil || !sent {
		t.Fatalf("replay: sent %v err %v", sent, err)
	}
	var pending bool
	if err := db.QueryRow(`SELECT sent_at IS NULL AND retries = 0 FROM outbox WHERE id = $1`, id).Scan(&pending); err != nil || !pending {
		t.Errorf("after the replay the row is pending %v (err %v), want pending", pending, err)
	}
	if rings != 2 {
		t.Errorf("rings after the replay = %d, want 2", rings)
	}
	if sent, err := db.ReplayOutbox(id + 1000); err != nil || sent {
		t.Errorf("replay of a row that is gone: sent %v err %v, want false", sent, err)
	}
}
