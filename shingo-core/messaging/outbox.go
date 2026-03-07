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
	wg       sync.WaitGroup
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
	d.wg.Add(1)
	go d.run()
}

func (d *OutboxDrainer) Stop() {
	select {
	case <-d.stopChan:
	default:
		close(d.stopChan)
	}
	d.wg.Wait()
}

func (d *OutboxDrainer) run() {
	defer d.wg.Done()

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	cycles := 0
	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			d.drain()
			cycles++
			// Purge old sent/dead-lettered messages every ~100 cycles
			if cycles%100 == 0 {
				if n, err := d.db.PurgeOldOutbox(24 * time.Hour); err != nil {
					log.Printf("outbox: purge old: %v", err)
				} else if n > 0 {
					log.Printf("outbox: purged %d old messages", n)
					d.dbg("purged %d old outbox messages", n)
				}
			}
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
			d.db.IncrementOutboxRetries(msg.ID)
			if msg.Retries+1 >= store.MaxOutboxRetries {
				log.Printf("outbox: msg %d dead-lettered after %d retries (type=%s): %v", msg.ID, msg.Retries+1, msg.MsgType, err)
			} else {
				log.Printf("outbox: publish to %s failed (retry %d/%d): %v", topic, msg.Retries+1, store.MaxOutboxRetries, err)
			}
			d.dbg("drain fail: id=%d topic=%s retries=%d error=%v", msg.ID, topic, msg.Retries+1, err)
			continue
		}
		d.dbg("drain ok: id=%d topic=%s msg_type=%s", msg.ID, topic, msg.MsgType)
		if err := d.db.AckOutbox(msg.ID); err != nil {
			log.Printf("outbox: ack msg %d: %v", msg.ID, err)
		}
	}
}
