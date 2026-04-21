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

// EnqueueOutbox writes a new outbox row.
func EnqueueOutbox(db *sql.DB, topic string, payload []byte, eventType, stationID string) error {
	_, err := db.Exec(`INSERT INTO outbox (topic, payload, msg_type, station_id) VALUES ($1, $2, $3, $4)`,
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

// RequeueOutbox resets retries to 0 on an unsent row.
func RequeueOutbox(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries=0 WHERE id=$1 AND sent_at IS NULL`, id)
	return err
}

// PurgeOldOutbox deletes sent or dead-lettered outbox rows older than the
// given duration. Returns the count of deleted rows.
func PurgeOldOutbox(db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format("2006-01-02 15:04:05")
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
