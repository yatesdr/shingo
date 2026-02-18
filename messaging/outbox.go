package messaging

import (
	"log"
	"time"

	"warpath/store"
)

// OutboxDrainer periodically sends pending outbox messages.
type OutboxDrainer struct {
	db       *store.DB
	client   *Client
	interval time.Duration
	stopChan chan struct{}
}

func NewOutboxDrainer(db *store.DB, client *Client, interval time.Duration) *OutboxDrainer {
	return &OutboxDrainer{
		db:       db,
		client:   client,
		interval: interval,
		stopChan: make(chan struct{}),
	}
}

func (d *OutboxDrainer) Start() {
	go d.run()
}

func (d *OutboxDrainer) Stop() {
	select {
	case d.stopChan <- struct{}{}:
	default:
	}
}

func (d *OutboxDrainer) run() {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			d.drain()
		}
	}
}

func (d *OutboxDrainer) drain() {
	msgs, err := d.db.ListPendingOutbox(50)
	if err != nil {
		log.Printf("outbox: list pending: %v", err)
		return
	}
	for _, msg := range msgs {
		topic := msg.Topic
		if err := d.client.Publish(topic, msg.Payload); err != nil {
			log.Printf("outbox: publish to %s failed: %v", topic, err)
			d.db.IncrementOutboxRetries(msg.ID)
			continue
		}
		d.db.AckOutbox(msg.ID)
	}
}
