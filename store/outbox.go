package store

import (
	"time"
)

type OutboxMessage struct {
	ID        int64
	Topic     string
	Payload   []byte
	MsgType   string
	ClientID  string
	Retries   int
	CreatedAt time.Time
	SentAt    *time.Time
}

func (db *DB) EnqueueOutbox(topic string, payload []byte, msgType, clientID string) error {
	_, err := db.Exec(db.Q(`INSERT INTO outbox (topic, payload, msg_type, client_id) VALUES (?, ?, ?, ?)`),
		topic, payload, msgType, clientID)
	return err
}

func (db *DB) ListPendingOutbox(limit int) ([]*OutboxMessage, error) {
	rows, err := db.Query(db.Q(`SELECT id, topic, payload, msg_type, client_id, retries, created_at FROM outbox WHERE sent_at IS NULL ORDER BY id LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []*OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Topic, &m.Payload, &m.MsgType, &m.ClientID, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

func (db *DB) AckOutbox(id int64) error {
	_, err := db.Exec(db.Q(`UPDATE outbox SET sent_at=datetime('now','localtime') WHERE id=?`), id)
	return err
}

func (db *DB) IncrementOutboxRetries(id int64) error {
	_, err := db.Exec(db.Q(`UPDATE outbox SET retries=retries+1 WHERE id=?`), id)
	return err
}
