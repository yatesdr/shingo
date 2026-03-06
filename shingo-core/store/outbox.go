package store

import (
	"time"
)

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

func (db *DB) EnqueueOutbox(topic string, payload []byte, eventType, stationID string) error {
	_, err := db.Exec(db.Q(`INSERT INTO outbox (topic, payload, msg_type, station_id) VALUES (?, ?, ?, ?)`),
		topic, payload, eventType, stationID)
	return err
}

func (db *DB) ListPendingOutbox(limit int) ([]*OutboxMessage, error) {
	rows, err := db.Query(db.Q(`SELECT id, topic, payload, msg_type, station_id, retries, created_at FROM outbox WHERE sent_at IS NULL ORDER BY id LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []*OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		var createdAt any
		if err := rows.Scan(&m.ID, &m.Topic, &m.Payload, &m.MsgType, &m.StationID, &m.Retries, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTime(createdAt)
		msgs = append(msgs, &m)
	}
	return msgs, rows.Err()
}

func (db *DB) AckOutbox(id int64) error {
	_, err := db.Exec(db.Q(`UPDATE outbox SET sent_at=datetime('now') WHERE id=?`), id)
	return err
}

func (db *DB) IncrementOutboxRetries(id int64) error {
	_, err := db.Exec(db.Q(`UPDATE outbox SET retries=retries+1 WHERE id=?`), id)
	return err
}
