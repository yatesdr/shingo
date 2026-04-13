package store

import "time"

// MaxOutboxRetries is the number of delivery attempts before a message is
// considered dead-lettered and skipped by the drainer.
const MaxOutboxRetries = 10

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
	_, err := db.Exec(`INSERT INTO outbox (topic, payload, msg_type, station_id) VALUES ($1, $2, $3, $4)`,
		topic, payload, eventType, stationID)
	return err
}

func (db *DB) ListPendingOutbox(limit int) ([]*OutboxMessage, error) {
	rows, err := db.Query(`SELECT id, topic, payload, msg_type, station_id, retries, created_at, sent_at FROM outbox WHERE sent_at IS NULL AND retries < $1 ORDER BY id LIMIT $2`, MaxOutboxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (db *DB) ListDeadLetterOutbox(limit int) ([]*OutboxMessage, error) {
	rows, err := db.Query(`SELECT id, topic, payload, msg_type, station_id, retries, created_at, sent_at FROM outbox WHERE sent_at IS NULL AND retries >= $1 ORDER BY id LIMIT $2`, MaxOutboxRetries, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

func (db *DB) AckOutbox(id int64) error {
	_, err := db.Exec(`UPDATE outbox SET sent_at=NOW() WHERE id=$1`, id)
	return err
}

func (db *DB) IncrementOutboxRetries(id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries=retries+1 WHERE id=$1`, id)
	return err
}

func (db *DB) RequeueOutbox(id int64) error {
	_, err := db.Exec(`UPDATE outbox SET retries=0 WHERE id=$1 AND sent_at IS NULL`, id)
	return err
}

// PurgeOldOutbox deletes sent messages older than the given duration,
// and dead-lettered messages (retries >= max) older than the given duration.
func (db *DB) PurgeOldOutbox(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format("2006-01-02 15:04:05")
	res, err := db.Exec(`DELETE FROM outbox WHERE (sent_at IS NOT NULL AND sent_at < $1) OR (retries >= $2 AND created_at < $3)`, cutoff, MaxOutboxRetries, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
