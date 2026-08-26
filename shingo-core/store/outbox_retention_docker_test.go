//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingo/protocol/outbox"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// outbox_retention_docker_test.go — delivered rows and dead letters have
// different lifetimes.
//
// They shared a 24h cutoff. That destroyed evidence twice in one week: an
// investigation read a clean outbox and concluded Springfield had never retried
// a message (it had — the proof had been purged), and two dead-lettered
// production deltas were ~50 minutes from deletion before anyone knew they
// existed. A delivered row is a receipt; a dead letter is the only surviving
// record of a message that was destroyed.

func seedOutbox(t *testing.T, db *store.DB, msgType string, age time.Duration, retries int, delivered bool) int64 {
	t.Helper()
	created := time.Now().UTC().Add(-age)
	var id int64
	var sentAt any
	if delivered {
		sentAt = created
	}
	err := db.QueryRow(
		`INSERT INTO outbox (topic, payload, msg_type, retries, created_at, sent_at)
		 VALUES ('orders', $1, $2, $3, $4, $5) RETURNING id`,
		[]byte(`{"x":1}`), msgType, retries, created, sentAt,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed %s: %v", msgType, err)
	}
	return id
}

func outboxRowExists(t *testing.T, db *store.DB, id int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("exists %d: %v", id, err)
	}
	return n > 0
}

func TestPurgeOldOutbox_DeadLettersOutliveDeliveredRows(t *testing.T) {
	db := testdb.Open(t)

	deliveredFresh := seedOutbox(t, db, "test.delivered.fresh", time.Hour, 0, true)
	deliveredStale := seedOutbox(t, db, "test.delivered.stale", 25*time.Hour, 0, true)
	deadRecent := seedOutbox(t, db, "test.dead.recent", 25*time.Hour, outbox.MaxRetries, false)
	deadAncient := seedOutbox(t, db, "test.dead.ancient", 8*24*time.Hour, outbox.MaxRetries, false)
	pendingOld := seedOutbox(t, db, "test.pending.old", 8*24*time.Hour, 1, false)

	if _, err := db.PurgeOldOutbox(outbox.MessageRetentionPeriod, outbox.DeadLetterRetentionPeriod); err != nil {
		t.Fatalf("purge: %v", err)
	}

	if !outboxRowExists(t, db, deliveredFresh) {
		t.Error("a delivered row inside the window was purged")
	}
	if outboxRowExists(t, db, deliveredStale) {
		t.Error("a delivered row at 25h survived the 24h delivered cutoff")
	}
	if !outboxRowExists(t, db, deadRecent) {
		t.Error("a dead letter at 25h was purged — this is the regression: it is " +
			"the only surviving record of a destroyed message and it must outlive " +
			"the delivered window")
	}
	if outboxRowExists(t, db, deadAncient) {
		t.Error("a dead letter at 8 days survived the 7-day dead-letter cutoff")
	}
	// A row still under the retry cap is neither delivered nor dead — it is
	// work in progress and no cutoff applies to it at any age.
	if !outboxRowExists(t, db, pendingOld) {
		t.Error("an 8-day-old row still BELOW the retry cap was purged — it is " +
			"still retryable and no retention window covers it")
	}
}
