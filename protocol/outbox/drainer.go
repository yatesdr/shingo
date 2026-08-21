package outbox

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"shingo/protocol/types"
)

// MaxRetries is the number of delivery attempts before a message is dead-lettered.
const MaxRetries = 10

const (
	// PurgeCycleInterval is how often (in TICKER cycles) old messages are
	// purged. Wake-driven drains deliberately do not count: the purge cadence
	// is PurgeCycleInterval x interval, so counting them would silently speed
	// housekeeping up in proportion to message rate.
	PurgeCycleInterval = 100

	// MessageRetentionPeriod is how long sent messages are kept before purging.
	MessageRetentionPeriod = 24 * time.Hour
)

// wakeSettle is how long a wake waits before draining.
//
// Note what it is NOT for: an instantaneous burst is already collapsed by the
// wake channel's capacity of 1, which holds at most one pending signal no
// matter how many enqueues raise it. Measured, 200 back-to-back notifications
// produce a single drain with this set to zero.
//
// What it actually buys is two things. It bounds the drain rate under
// SUSTAINED enqueues, where the channel refills the moment a drain finishes —
// without it the loop would drain as fast as the queries return. That matters
// because a drain retries every pending row, so an unbounded drain rate would
// burn the retry budget far faster than the tick ever did.
//
// And it lets a transaction commit. EnqueueOutbox can run inside the
// transaction that produced the message, so the notification arrives before
// the row is visible to another connection. Draining instantly would query too
// early and find nothing. This is best-effort for that case, not a guarantee —
// the ticker remains the backstop, and a row missed here simply waits for it
// as it does today.
//
// Variable rather than const so tests can compress it and exercise many wake
// cycles without the wall-clock cost, mirroring www's sseKeepaliveInterval.
var wakeSettle = 50 * time.Millisecond

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
	// MarkOutboxExhausted forces a row into the implicit dead-letter
	// state (retries >= MaxRetries) in a single UPDATE. Used by the
	// per-message panic boundary in drain() to prevent a poison-pill
	// message from looping forever. reason is logged at the panic
	// site and may or may not be persisted by the implementation.
	MarkOutboxExhausted(id int64, reason string) error
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
	// wake is the doorbell: capacity 1, so a signal already pending needs no
	// second one — the drain that answers it reads every pending row anyway.
	wake chan struct{}
	wg   sync.WaitGroup

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
		wake:      make(chan struct{}, 1),
	}
}

// Notify tells the drainer a message was just enqueued, so it drains on the
// next settle rather than waiting out the tick.
//
// Without it the drain loop was a bare ticker with nothing to tell it work had
// arrived, so every message waited a uniform 0-interval before its first send
// attempt — measured at Springfield on a 5s interval: mean 2.839s, p95 5.009s,
// with zero rows ever backlogged. A core->edge->core round trip paid it twice.
//
// Never blocks, so an enqueue is never slowed by a busy drainer. Safe to call
// on a stopped drainer.
func (d *Drainer) Notify() {
	select {
	case d.wake <- struct{}{}:
	default:
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
		case <-d.wake:
			// Settle before draining: coalesce the rest of a burst, and give a
			// transactional enqueue time to commit. The ticker arm below still
			// runs on its own schedule and is the backstop for anything this
			// pass misses.
			settle := time.NewTimer(wakeSettle)
			select {
			case <-d.stopChan:
				settle.Stop()
				return
			case <-settle.C:
			}
			d.drain()
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
		// Per-message panic boundary. Without this a panic in
		// d.publisher.Publish (or any nested call) would kill the
		// drainer goroutine — silent stop, the worst possible
		// failure mode. The recover handler logs the panic with
		// stack and forces the message into the implicit
		// dead-letter state so a poison-pill payload doesn't loop
		// forever. Subsequent messages in this drain pass continue
		// processing normally.
		d.publishOne(msg)
	}
}

func (d *Drainer) publishOne(msg Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC outbox-publish msg=%d type=%s: %v\n%s",
				msg.ID, msg.MsgType, r, debug.Stack())
			reason := fmt.Sprintf("panic during publish: %v", r)
			if err := d.store.MarkOutboxExhausted(msg.ID, reason); err != nil {
				log.Printf("outbox: exhaust mark for msg %d: %v", msg.ID, err)
			}
		}
	}()
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
		return
	}
	d.DebugLog.Log("published outbox msg %d type=%s", msg.ID, msg.MsgType)
	if err := d.store.AckOutbox(msg.ID); err != nil {
		log.Printf("outbox: ack msg %d: %v", msg.ID, err)
	}
}
