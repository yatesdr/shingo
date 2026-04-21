// Package outbox holds outbox-message persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the outbox CRUD out of the
// flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.OutboxMessage = outbox.Message`) and one-
// line delegate methods on *store.DB so external callers see no API
// change.
package outbox

import (
	"database/sql"
	"time"

	"shingoedge/store/internal/helpers"
)

// MaxRetries is the number of delivery attempts before a message is
// considered dead-lettered and skipped by the drainer.
const MaxRetries = 10

// Message is one outbox row.
type Message struct {
	ID        int64      `json:"id"`
	Payload   []byte     `json:"payload"`
	MsgType   string     `json:"msg_type"`
	Retries   int        `json:"retries"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at"`
}

// Enqueue inserts a new outbound message and returns its row id.
func Enqueue(db *sql.DB, payload []byte, msgType string) (int64, error) {
	res, err := db.Exec(`INSERT INTO outbox (topic, payload, msg_type) VALUES ('orders', ?, ?)`, payload, msgType)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListPending returns the next batch of un-sent messages whose retry
// count is below MaxRetries.
func ListPending(db *sql.DB, limit int) ([]Message, error) {
	rows, err := db.Query(`SELECT id, payload, msg_type, retries, created_at FROM outbox WHERE sent_at IS NULL AND retries < ? ORDER BY id LIMIT ?`, MaxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Payload, &m.MsgType, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = helpers.ScanTime(createdAt)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListDeadLetter returns un-sent messages that have hit MaxRetries.
func ListDeadLetter(db *sql.DB, limit int) ([]Message, error) {
	rows, err := db.Query(`SELECT id, payload, msg_type, retries, created_at FROM outbox WHERE sent_at IS NULL AND retries >= ? ORDER BY id LIMIT ?`, MaxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var m Message
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Payload, &m.MsgType, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = helpers.ScanTime(createdAt)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Ack marks a message as sent.
func Ack(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET sent_at = datetime('now') WHERE id = ?`, id)
	return err
}

// IncrementRetries bumps the retry counter on a message.
func IncrementRetries(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries = retries + 1 WHERE id = ?`, id)
	return err
}

// Requeue resets the retry counter so a dead-lettered message will be
// picked up by the drainer again.
func Requeue(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries = 0 WHERE id = ? AND sent_at IS NULL`, id)
	return err
}

// PurgeOld deletes sent messages older than the given duration, and
// dead-lettered messages (retries >= MaxRetries) older than the given
// duration.
func PurgeOld(db *sql.DB, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).Format(helpers.TimeLayout)
	res, err := db.Exec(`DELETE FROM outbox WHERE (sent_at IS NOT NULL AND sent_at < ?) OR (retries >= ? AND created_at < ?)`, cutoff, MaxRetries, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
