package outbox

import (
	"log"
	"sync"
	"time"

	"shingo/protocol/types"
)

// MaxRetries is the number of delivery attempts before a message is dead-lettered.
const MaxRetries = 10

const (
	// PurgeCycleInterval is how often (in drain cycles) old messages are purged.
	PurgeCycleInterval = 100

	// MessageRetentionPeriod is how long sent messages are kept before purging.
	MessageRetentionPeriod = 24 * time.Hour
)

// Message represents a pending outbox message.
type Message struct {
	ID      int64
	Topic   string
	Payload []byte
	MsgType string
	Retries int
}

// Store is the database interface the drainer needs.
type Store interface {
	ListPendingOutbox(limit int) ([]Message, error)
	AckOutbox(id int64) error
	IncrementOutboxRetries(id int64) error
	PurgeOldOutbox(olderThan time.Duration) (int, error)
}

// Publisher is the messaging client interface the drainer needs.
type Publisher interface {
	Publish(topic string, payload []byte) error
	IsConnected() bool
}

// Drainer periodically sends pending outbox messages via a Publisher.
type Drainer struct {
	store     Store
	publisher Publisher
	topic     string
	interval  time.Duration
	limit     int
	stopChan  chan struct{}
	wg        sync.WaitGroup

	DebugLog types.DebugLogFunc
}

// NewDrainer creates a new outbox drainer.
// topic is the default topic for published messages (can be overridden per-message
// if the Store returns a non-empty Message.Topic).
// interval controls how often the drain cycle runs.
// limit caps the number of messages fetched per cycle.
func NewDrainer(store Store, publisher Publisher, topic string, interval time.Duration, limit int) *Drainer {
	if limit <= 0 {
		limit = 50
	}
	return &Drainer{
		store:     store,
		publisher: publisher,
		topic:     topic,
		interval:  interval,
		limit:     limit,
		stopChan:  make(chan struct{}),
	}
}

// Start begins the drain loop in a background goroutine.
func (d *Drainer) Start() {
	d.wg.Add(1)
	go d.run()
}

// Stop signals the drain loop to stop and waits for it to finish.
func (d *Drainer) Stop() {
	select {
	case <-d.stopChan:
	default:
		close(d.stopChan)
	}
	d.wg.Wait()
}

func (d *Drainer) run() {
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
			if cycles%PurgeCycleInterval == 0 {
				if n, err := d.store.PurgeOldOutbox(MessageRetentionPeriod); err != nil {
					log.Printf("outbox: purge old: %v", err)
				} else if n > 0 {
					log.Printf("outbox: purged %d old messages", n)
					d.DebugLog.Log("purged %d old outbox messages", n)
				}
			}
		}
	}
}

func (d *Drainer) drain() {
	if !d.publisher.IsConnected() {
		return
	}
	msgs, err := d.store.ListPendingOutbox(d.limit)
	if err != nil {
		log.Printf("outbox: list pending: %v", err)
		return
	}
	if len(msgs) > 0 {
		d.DebugLog.Log("drain: %d pending messages", len(msgs))
	}
	for _, msg := range msgs {
		topic := msg.Topic
		if topic == "" {
			topic = d.topic
		}
		if err := d.publisher.Publish(topic, msg.Payload); err != nil {
			d.store.IncrementOutboxRetries(msg.ID)
			if msg.Retries+1 >= MaxRetries {
				log.Printf("outbox: msg %d dead-lettered after %d retries (type=%s): %v", msg.ID, msg.Retries+1, msg.MsgType, err)
					d.DebugLog.Log("DEAD-LETTER: msg %d type=%s retries=%d err=%v", msg.ID, msg.MsgType, msg.Retries+1, err)
			} else {
				log.Printf("outbox: publish to %s failed (retry %d/%d): %v", topic, msg.Retries+1, MaxRetries, err)
				d.DebugLog.Log("retry: msg %d type=%s attempt=%d/%d err=%v", msg.ID, msg.MsgType, msg.Retries+1, MaxRetries, err)
			}
			continue
		}
		d.DebugLog.Log("published outbox msg %d type=%s", msg.ID, msg.MsgType)
		if err := d.store.AckOutbox(msg.ID); err != nil {
			log.Printf("outbox: ack msg %d: %v", msg.ID, err)
		}
	}
}
