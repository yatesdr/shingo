package store

// Phase 5 delegate file: inbox idempotency record lives in
// store/messaging/. This file preserves the *store.DB method surface
// so external callers don't need to change.

import (
	"time"

	"shingocore/store/messaging"
)

// InboxRetentionPeriod preserves the store-level constant name alongside
// MaxOutboxRetries; the value and its rationale live in store/messaging.
const InboxRetentionPeriod = messaging.InboxRetentionPeriod

// RecordInboundMessage records a processed inbound envelope ID.
// Returns true when the message is newly recorded, false when it was already seen.
func (db *DB) RecordInboundMessage(msgID, msgType, stationID string) (bool, error) {
	return messaging.RecordInboundMessage(db.DB, msgID, msgType, stationID)
}

// PurgeOldInbox deletes processed-message records older than the given
// duration, closing the retention gap against outbox's PurgeOldOutbox.
func (db *DB) PurgeOldInbox(olderThan time.Duration) (int64, error) {
	return messaging.PurgeOldInbox(db.DB, olderThan)
}

// RecordInboundReply records the outbox row that answered a processed inbound
// message (an ask whose repeat is answered by replay).
func (db *DB) RecordInboundReply(msgID string, outboxID int64) error {
	return messaging.RecordInboundReply(db.DB, msgID, outboxID)
}

// InboundReply is the outbox row that answered a processed inbound message.
func (db *DB) InboundReply(msgID string) (int64, bool, error) {
	return messaging.InboundReply(db.DB, msgID)
}
