package store

// Phase 5b delegate file: outbox CRUD lives in store/messaging/.
// (Phase 6.0c renamed the sub-package from outbox/ to messaging/ —
// the package name now describes its responsibility — durable
// inter-process communication — rather than the implementation
// pattern. The on-disk table name `outbox` is unchanged.) This file
// preserves the *store.DB method surface and the MaxOutboxRetries
// const so external callers do not need to change.

import (
	"time"

	"shingoedge/store/messaging"
)

// MaxOutboxRetries is the number of delivery attempts before a message is
// considered dead-lettered and skipped by the drainer.
const MaxOutboxRetries = messaging.MaxRetries

// EnqueueOutbox inserts a new outbound message and returns its row id.
func (db *DB) EnqueueOutbox(payload []byte, msgType string) (int64, error) {
	return messaging.Enqueue(db.DB, payload, msgType)
}

// ListPendingOutbox returns the next batch of un-sent messages whose
// retry count is below MaxOutboxRetries.
func (db *DB) ListPendingOutbox(limit int) ([]messaging.Message, error) {
	return messaging.ListPending(db.DB, limit)
}

// ListUnsentOutboxByType returns every un-sent outbox message matching
// any of the given msg_type values. Used at startup to recover
// in-memory state from durable outbox entries (e.g. inventory delta
// pending sets after a crash).
func (db *DB) ListUnsentOutboxByType(msgTypes []string) ([]messaging.Message, error) {
	return messaging.ListUnsentByType(db.DB, msgTypes)
}

// ListDeadLetterOutbox returns un-sent messages that have hit
// MaxOutboxRetries.
func (db *DB) ListDeadLetterOutbox(limit int) ([]messaging.Message, error) {
	return messaging.ListDeadLetter(db.DB, limit)
}

// AckOutbox marks a message as sent.
func (db *DB) AckOutbox(id int64) error {
	return messaging.Ack(db.DB, id)
}

// IncrementOutboxRetries bumps the retry counter on a message.
func (db *DB) IncrementOutboxRetries(id int64) error {
	return messaging.IncrementRetries(db.DB, id)
}

// MarkOutboxExhausted forces a message into the implicit dead-letter
// state (retries = MaxRetries) in a single UPDATE. Used by the
// outbox drainer's per-message panic boundary to ensure a
// poison-pill message doesn't loop forever.
func (db *DB) MarkOutboxExhausted(id int64, reason string) error {
	return messaging.MarkExhausted(db.DB, id, reason)
}

// CountPendingOutbox returns the number of un-sent outbox messages
// (sent_at IS NULL AND retries < MaxRetries). Surfaced via /status
// as a wedge indicator — a steadily growing outbox depth is the
// operational signal that Kafka or Core is unreachable.
func (db *DB) CountPendingOutbox() (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND retries < ?`, messaging.MaxRetries).Scan(&n)
	return n, err
}

// RequeueOutbox resets the retry counter so a dead-lettered message
// will be picked up by the drainer again.
func (db *DB) RequeueOutbox(id int64) error {
	return messaging.Requeue(db.DB, id)
}

// PurgeOldOutbox deletes sent messages older than the given duration,
// and dead-lettered messages (retries >= max) older than the given
// duration.
func (db *DB) PurgeOldOutbox(olderThan time.Duration) (int64, error) {
	return messaging.PurgeOld(db.DB, olderThan)
}
