package messaging

import (
	"time"

	"shingo/protocol/outbox"
	"shingoedge/config"
	"shingoedge/store"
)

// edgeOutboxStore adapts store.DB to the outbox.Store interface.
type edgeOutboxStore struct {
	db *store.DB
}

// ListPendingOutbox returns pending messages from the database.
func (s *edgeOutboxStore) ListPendingOutbox(limit int) ([]outbox.Message, error) {
	msgs, err := s.db.ListPendingOutbox(limit)
	if err != nil {
		return nil, err
	}
	result := make([]outbox.Message, len(msgs))
	for i, msg := range msgs {
		result[i] = outbox.Message{
			ID:      msg.ID,
			Topic:   "", // edge uses fixed topic from config
			Payload: msg.Payload,
			MsgType: msg.MsgType,
			Retries: msg.Retries,
		}
	}
	return result, nil
}

// AckOutbox marks a message as sent.
func (s *edgeOutboxStore) AckOutbox(id int64) error {
	return s.db.AckOutbox(id)
}

// IncrementOutboxRetries increments the retry count.
func (s *edgeOutboxStore) IncrementOutboxRetries(id int64) error {
	return s.db.IncrementOutboxRetries(id)
}

// PurgeOldOutbox removes old messages.
func (s *edgeOutboxStore) PurgeOldOutbox(olderThan time.Duration) (int, error) {
	n, err := s.db.PurgeOldOutbox(olderThan)
	return int(n), err
}

// DrainBatchSize is the maximum number of outbox messages drained per cycle.
const DrainBatchSize = 50

// NewOutboxDrainer creates a new outbox drainer.
func NewOutboxDrainer(db *store.DB, client *Client, cfg *config.MessagingConfig) *outbox.Drainer {
	store := &edgeOutboxStore{db: db}
	interval := cfg.OutboxDrainInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	drainer := outbox.NewDrainer(store, client, cfg.OrdersTopic, interval, DrainBatchSize)
	return drainer
}
