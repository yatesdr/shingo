package store

// Phase 5 delegate file: outbox CRUD lives in store/messaging/. This
// file preserves the *store.DB method surface so external callers don't
// need to change.

import (
	"time"

	"shingocore/store/messaging"
)

// MaxOutboxRetries preserves the store.MaxOutboxRetries public API.
const MaxOutboxRetries = messaging.MaxOutboxRetries

func (db *DB) EnqueueOutbox(topic string, payload []byte, eventType, stationID string) error {
	return messaging.EnqueueOutbox(db.DB, topic, payload, eventType, stationID)
}

func (db *DB) ListPendingOutbox(limit int) ([]*messaging.OutboxMessage, error) {
	return messaging.ListPendingOutbox(db.DB, limit)
}

func (db *DB) ListDeadLetterOutbox(limit int) ([]*messaging.OutboxMessage, error) {
	return messaging.ListDeadLetterOutbox(db.DB, limit)
}

func (db *DB) AckOutbox(id int64) error { return messaging.AckOutbox(db.DB, id) }

func (db *DB) IncrementOutboxRetries(id int64) error {
	return messaging.IncrementOutboxRetries(db.DB, id)
}

func (db *DB) RequeueOutbox(id int64) error { return messaging.RequeueOutbox(db.DB, id) }

func (db *DB) PurgeOldOutbox(olderThan time.Duration) (int64, error) {
	return messaging.PurgeOldOutbox(db.DB, olderThan)
}
