package messaging

import (
	"log"
	"sync"
	"time"

	"shingoedge/config"
	"shingoedge/store"
)

// OutboxDrainer periodically sends pending outbox messages.
type OutboxDrainer struct {
	db       *store.DB
	client   *Client
	cfg      *config.MessagingConfig
	stopChan chan struct{}
	wg       sync.WaitGroup

	DebugLog func(string, ...any)
}

func (d *OutboxDrainer) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// NewOutboxDrainer creates a new outbox drainer.
func NewOutboxDrainer(db *store.DB, client *Client, cfg *config.MessagingConfig) *OutboxDrainer {
	return &OutboxDrainer{
		db:       db,
		client:   client,
		cfg:      cfg,
		stopChan: make(chan struct{}),
	}
}

// Start begins the outbox drain loop.
func (d *OutboxDrainer) Start() {
	d.wg.Add(1)
	go d.drainLoop()
}

// Stop stops the outbox drain loop.
func (d *OutboxDrainer) Stop() {
	select {
	case <-d.stopChan:
	default:
		close(d.stopChan)
	}
	d.wg.Wait()
}

func (d *OutboxDrainer) drainLoop() {
	defer d.wg.Done()

	interval := d.cfg.OutboxDrainInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
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
					log.Printf("purge old outbox: %v", err)
				} else if n > 0 {
					log.Printf("purged %d old outbox messages", n)
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
		log.Printf("list pending outbox: %v", err)
		return
	}

	if len(msgs) > 0 {
		d.dbg("drain cycle: %d pending messages", len(msgs))
	}

	for _, msg := range msgs {
		topic := d.cfg.OrdersTopic
		if err := d.client.Publish(topic, msg.Payload); err != nil {
			d.db.IncrementOutboxRetries(msg.ID)
			if msg.Retries+1 >= store.MaxOutboxRetries {
				log.Printf("outbox msg %d dead-lettered after %d retries (type=%s): %v", msg.ID, msg.Retries+1, msg.MsgType, err)
			} else {
				log.Printf("publish outbox msg %d (retry %d/%d): %v", msg.ID, msg.Retries+1, store.MaxOutboxRetries, err)
			}
			continue
		}
		if err := d.db.AckOutbox(msg.ID); err != nil {
			log.Printf("ack outbox msg %d: %v", msg.ID, err)
		} else {
			d.dbg("published outbox msg %d type=%s", msg.ID, msg.MsgType)
		}
	}
}
