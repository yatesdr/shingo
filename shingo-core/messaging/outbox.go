package messaging

import (
	"time"

	"shingo/protocol/outbox"
	"shingo/protocol/types"
	"shingocore/store"
)

// coreOutboxStore adapts store.DB to the outbox.Store interface.
type coreOutboxStore struct {
	db *store.DB
}

// ListPendingOutbox converts store.OutboxMessage to outbox.Message.
func (a *coreOutboxStore) ListPendingOutbox(limit int) ([]outbox.Message, error) {
	dbMsgs, err := a.db.ListPendingOutbox(limit)
	if err != nil {
		return nil, err
	}
	msgs := make([]outbox.Message, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = outbox.Message{
			ID:      m.ID,
			Topic:   m.Topic,
			Payload: m.Payload,
			MsgType: m.MsgType,
			Retries: m.Retries,
		}
	}
	return msgs, nil
}

// AckOutbox marks a message as sent.
func (a *coreOutboxStore) AckOutbox(id int64) error {
	return a.db.AckOutbox(id)
}

// IncrementOutboxRetries increments the retry count.
func (a *coreOutboxStore) IncrementOutboxRetries(id int64) error {
	return a.db.IncrementOutboxRetries(id)
}

// PurgeOldOutbox removes old messages.
func (a *coreOutboxStore) PurgeOldOutbox(olderThan time.Duration) (int, error) {
	n, err := a.db.PurgeOldOutbox(olderThan)
	return int(n), err
}

// NewOutboxDrainer creates a new outbox drainer with the given parameters.
func NewOutboxDrainer(db *store.DB, client *Client, interval time.Duration) *outbox.Drainer {
	adapter := &coreOutboxStore{db: db}
	drainer := outbox.NewDrainer(adapter, client, "", interval, 50)
	// Wrap DebugLog from client
	drainer.DebugLog = types.DebugLogFunc(client.DebugLog)
	return drainer
}
