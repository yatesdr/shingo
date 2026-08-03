// Package messaging holds outbox + inbox persistence for shingo-core.
//
// Phase 5 of the architecture plan moved outbox CRUD and the inbox
// idempotency record out of the flat store/ package and into this
// sub-package. The outer store/ keeps the constant + type alias
// (`store.OutboxMessage = messaging.OutboxMessage`,
// `store.MaxOutboxRetries = messaging.MaxOutboxRetries`) and one-line
// delegate methods on *store.DB so external callers see no API change.
package messaging

import (
	"database/sql"
	"time"
)

// MaxOutboxRetries is the number of delivery attempts before a message
// is considered dead-lettered and skipped by the drainer.
const MaxOutboxRetries = 10

// OutboxMessage is one queued outbound envelope.
type OutboxMessage struct {
	ID        int64      `json:"id"`
	Topic     string     `json:"topic"`
	Payload   []byte     `json:"payload"`
	MsgType   string     `json:"msg_type"`
	StationID string     `json:"station_id"`
	Retries   int        `json:"retries"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
}

// EnqueueOutbox writes a new outbox row on ex — the pool for an ordinary
// send, or a transaction when the message must live or die with the work that
// caused it (see EnqueueDataToEdge).
func EnqueueOutbox(ex Execer, topic string, payload []byte, eventType, stationID string) error {
	_, err := ex.Exec(`INSERT INTO outbox (topic, payload, msg_type, station_id) VALUES ($1, $2, $3, $4)`,
		topic, payload, eventType, stationID)
	return err
}

// ListPendingOutbox returns unsent rows whose retries are below the cap.
func ListPendingOutbox(db *sql.DB, limit int) ([]*OutboxMessage, error) {
	rows, err := db.Query(`SELECT id, topic, payload, msg_type, station_id, retries, created_at, sent_at FROM outbox WHERE sent_at IS NULL AND retries < $1 ORDER BY id LIMIT $2`, MaxOutboxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutbox(rows)
}

// ListDeadLetterOutbox returns unsent rows that exhausted retries.
func ListDeadLetterOutbox(db *sql.DB, limit int) ([]*OutboxMessage, error) {
	rows, err := db.Query(`SELECT id, topic, payload, msg_type, station_id, retries, created_at, sent_at FROM outbox WHERE sent_at IS NULL AND retries >= $1 ORDER BY id LIMIT $2`, MaxOutboxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutbox(rows)
}

func scanOutbox(rows *sql.Rows) ([]*OutboxMessage, error) {
	var msgs []*OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.ID, &m.Topic, &m.Payload, &m.MsgType, &m.StationID, &m.Retries, &m.CreatedAt, &m.SentAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

// AckOutbox marks an outbox row sent.
func AckOutbox(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET sent_at=NOW() WHERE id=$1`, id)
	return err
}

// IncrementOutboxRetries bumps the retries counter on an outbox row.
func IncrementOutboxRetries(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries=retries+1 WHERE id=$1`, id)
	return err
}

// MarkOutboxExhausted forces a row into the implicit dead-letter
// state in a single UPDATE by setting retries to MaxOutboxRetries.
// Used by the outbox drainer's per-message panic boundary. reason
// is accepted to match the protocol/outbox Store interface but is
// not persisted — the schema doesn't carry a last_error column.
func MarkOutboxExhausted(db *sql.DB, id int64, reason string) error {
	_, err := db.Exec(`UPDATE outbox SET retries=$1 WHERE id=$2`, MaxOutboxRetries, id)
	return err
}

// RequeueOutbox resets retries to 0 on an unsent row.
func RequeueOutbox(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries=0 WHERE id=$1 AND sent_at IS NULL`, id)
	return err
}

// PurgeOldOutbox deletes sent or dead-lettered outbox rows older than the
// given duration. Returns the count of deleted rows.
func PurgeOldOutbox(db *sql.DB, olderThan time.Duration) (int64, error) {
	// Bind a time.Time, not a formatted string: sent_at/created_at are
	// TIMESTAMPTZ, and a zoneless literal would be compared in the session
	// TimeZone, shifting the cutoff by the offset on a non-UTC session.
	cutoff := time.Now().UTC().Add(-olderThan)
	res, err := db.Exec(`DELETE FROM outbox WHERE (sent_at IS NOT NULL AND sent_at < $1) OR (retries >= $2 AND created_at < $3)`, cutoff, MaxOutboxRetries, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecordInboundMessage records a processed inbound envelope ID. Returns
// true when the message is newly recorded, false when it was already seen.
func RecordInboundMessage(db *sql.DB, msgID, msgType, stationID string) (bool, error) {
	res, err := db.Exec(`INSERT INTO inbox (msg_id, msg_type, station_id) VALUES ($1, $2, $3) ON CONFLICT (msg_id) DO NOTHING`,
		msgID, msgType, stationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// InboxRetentionPeriod is how long processed inbound message IDs are kept.
//
// Ninety days, matching the heartbeat/downtime windows, and generous by
// orders of magnitude: the record only has to outlive the redelivery it
// guards against, which the outbox bounds at 24 hours.
const InboxRetentionPeriod = 90 * 24 * time.Hour

// PurgeOldInbox deletes processed-message records older than the given
// duration. Returns the count of deleted rows.
//
// THIS IS NOT URGENT AND SHOULD NOT BE READ AS SUCH. Measured on the
// restored Springfield database: 5,525 rows and 1,160 kB spanning
// 2026-03-25 to 2026-07-26 — 123 days, about 3.5 MB a year, on a Core
// database where nothing else is measured in single-digit megabytes. It
// will never be a size problem, and it sits in the same retention census
// as a table growing at 805 MB/year only because the two share a shape.
//
// It is worth ten lines anyway, for two reasons. Its outbox twin has had
// PurgeOldOutbox since the beginning, so "inbound records are kept forever"
// reads as an oversight in the symmetry rather than a decision, and every
// future census re-raises it at the same cost. And idx_inbox_processed_at
// already exists while processed_at appears nowhere outside the DDL —
// verified by grep across the repo at f0b1a6a6 — an index built for a purge
// that was planned (derek's 3e6d3f3a) and never written. Writing the purge
// is what makes that index earn its keep; the honest alternative was to
// drop the index.
func PurgeOldInbox(db *sql.DB, olderThan time.Duration) (int64, error) {
	// Bind a time.Time, not a formatted string: processed_at is TIMESTAMPTZ
	// and a zoneless literal would be compared in the session TimeZone,
	// shifting the cutoff by the offset on a non-UTC session (same trap
	// PurgeOldOutbox documents above).
	cutoff := time.Now().UTC().Add(-olderThan)
	res, err := db.Exec(`DELETE FROM inbox WHERE processed_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
