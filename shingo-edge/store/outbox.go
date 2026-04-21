package store

// Phase 5b delegate file: outbox CRUD now lives in store/outbox/.
// This file preserves the *store.DB method surface and the
// MaxOutboxRetries const so external callers do not need to change.

import (
	"time"

	"shingoedge/store/outbox"
)

// OutboxMessage is a queued outbound message.
type OutboxMessage = outbox.Message

// MaxOutboxRetries is the number of delivery attempts before a message is
// considered dead-lettered and skipped by the drainer.
const MaxOutboxRetries = outbox.MaxRetries

// EnqueueOutbox inserts a new outbound message and returns its row id.
func (db *DB) EnqueueOutbox(payload []byte, msgType string) (int64, error) {
	return outbox.Enqueue(db.DB, payload, msgType)
}

// ListPendingOutbox returns the next batch of un-sent messages whose
// retry count is below MaxOutboxRetries.
func (db *DB) ListPendingOutbox(limit int) ([]OutboxMessage, error) {
	return outbox.ListPending(db.DB, limit)
}

// ListDeadLetterOutbox returns un-sent messages that have hit
// MaxOutboxRetries.
func (db *DB) ListDeadLetterOutbox(limit int) ([]OutboxMessage, error) {
	return outbox.ListDeadLetter(db.DB, limit)
}

// AckOutbox marks a message as sent.
func (db *DB) AckOutbox(id int64) error {
	return outbox.Ack(db.DB, id)
}

// IncrementOutboxRetries bumps the retry counter on a message.
func (db *DB) IncrementOutboxRetries(id int64) error {
	return outbox.IncrementRetries(db.DB, id)
}

// RequeueOutbox resets the retry counter so a dead-lettered message
// will be picked up by the drainer again.
func (db *DB) RequeueOutbox(id int64) error {
	return outbox.Requeue(db.DB, id)
}

// PurgeOldOutbox deletes sent messages older than the given duration,
// and dead-lettered messages (retries >= max) older than the given
// duration.
func (db *DB) PurgeOldOutbox(olderThan time.Duration) (int64, error) {
	return outbox.PurgeOld(db.DB, olderThan)
}
