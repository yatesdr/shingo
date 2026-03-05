package messaging

import (
	"log"
	"sync"
	"time"

	"shingocore/store"
)

// OutboxDrainer periodically sends pending outbox messages.
type OutboxDrainer struct {
	db       *store.DB
	client   *Client
	interval time.Duration
	stopChan chan struct{}
	stopOnce sync.Once
	DebugLog func(string, ...any)
}

func NewOutboxDrainer(db *store.DB, client *Client, interval time.Duration) *OutboxDrainer {
	return &OutboxDrainer{
		db:       db,
		client:   client,
		interval: interval,
		stopChan: make(chan struct{}),
	}
}

func (d *OutboxDrainer) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

func (d *OutboxDrainer) Start() {
	go d.run()
}

func (d *OutboxDrainer) Stop() {
	d.stopOnce.Do(func() { close(d.stopChan) })
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
	if !d.client.IsConnected() {
		return
	}
	msgs, err := d.db.ListPendingOutbox(50)
	if err != nil {
		log.Printf("outbox: list pending: %v", err)
		return
	}
	if len(msgs) > 0 {
		d.dbg("drain: %d pending messages", len(msgs))
	}
	for _, msg := range msgs {
		topic := msg.Topic
		if err := d.client.Publish(topic, msg.Payload); err != nil {
			log.Printf("outbox: publish to %s failed: %v", topic, err)
			d.dbg("drain fail: id=%d topic=%s retries=%d error=%v", msg.ID, topic, msg.Retries+1, err)
			d.db.IncrementOutboxRetries(msg.ID)
			continue
		}
		d.dbg("drain ok: id=%d topic=%s msg_type=%s", msg.ID, topic, msg.MsgType)
		d.db.AckOutbox(msg.ID)
	}
}
