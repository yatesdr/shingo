package store

// Phase 5 delegate file: inbox idempotency record lives in
// store/messaging/. This file preserves the *store.DB method surface
// so external callers don't need to change.

import (
	"shingocore/store/messaging"
)

// RecordInboundMessage records a processed inbound envelope ID.
// Returns true when the message is newly recorded, false when it was already seen.
func (db *DB) RecordInboundMessage(msgID, msgType, stationID string) (bool, error) {
	return messaging.RecordInboundMessage(db.DB, msgID, msgType, stationID)
}
