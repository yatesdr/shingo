package messaging

// ephemeral.go — the Edge's side of the ephemeral topic (protocol/ephemeral.go):
// a writer and a reader the engine opens only while Core names the topic
// (engine/robot_watch.go), never before. The plant brokers auto-create a topic
// on first touch with their 7-day default, so a reader opened before Core made
// the topic would make it wrongly; this file is never reached before then.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingo/protocol/backoff"
)

// ephemeralReader is the part of *kafkago.Reader the ephemeral read loop
// drives. It has no CommitMessages: the reader joins no consumer group, so
// there is no offset to commit, and a commit cannot be written against it.
type ephemeralReader interface {
	FetchMessage(ctx context.Context) (kafkago.Message, error)
	Close() error
}

// ephemeralWriter is the part of *kafkago.Writer the sender drives.
type ephemeralWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafkago.Message) error
	Close() error
}

// newEphemeralReader opens a reader of the topic's one partition with NO
// consumer group, positioned at the LAST offset: it sees only what is published
// after it starts, so an Edge that comes back from an outage replays nothing.
//
// The reader positions itself (SetOffset): kafka-go reads the start offset in a
// reader's config only for a consumer group, and a reader without one starts
// at the FIRST offset.
func newEphemeralReader(brokers []string, topic string) *kafkago.Reader {
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: 0,
	})
	// Cannot fail on a reader that has not started (no group, not closed).
	_ = r.SetOffset(kafkago.LastOffset)
	return r
}

// newEphemeralWriter is the topic's writer: one attempt per message, so a
// failed one is dropped, never retried (owner ruling: no outbox, no retry).
// Only the run's sender drives it, never a publisher: kafka-go looks up the
// topic's partitions inside WriteMessages, which against a broker that does
// not answer holds its caller for the transport's timeout. RequiredAcks is set
// because its zero value is RequireNone (client.go's writer says why that
// matters).
func newEphemeralWriter(brokers []string) *kafkago.Writer {
	return &kafkago.Writer{
		Addr:         kafkago.TCP(brokers...),
		Balancer:     &kafkago.Hash{},
		RequiredAcks: kafkago.RequireOne,
		BatchTimeout: writerBatchTimeout,
		MaxAttempts:  1,
	}
}

// ephemeralQueue is how many messages may wait for the sender. The Edge sends
// one watch every protocol.RobotWatchInterval and on a change, so a full queue
// is a broker that is gone, and what it would hold is stale.
const ephemeralQueue = 8

// ephemeralRun is the ephemeral topic while it is open.
type ephemeralRun struct {
	topic  string
	reader ephemeralReader
	// out is the sender's queue: written under the client's read lock and
	// closed under its write lock, so no send follows the close.
	out  chan kafkago.Message
	stop chan struct{}
}

// send writes each queued message once, giving it protocol.EphemeralTTL, and
// closes the writer when the queue is closed. A failure is dropped.
func (run *ephemeralRun) send(w ephemeralWriter) {
	defer func() { _ = w.Close() }()
	for m := range run.out {
		ctx, cancel := context.WithTimeout(context.Background(), protocol.EphemeralTTL)
		_ = w.WriteMessages(ctx, m)
		cancel()
	}
}

// errEphemeralClosed is PublishEphemeral's answer while the topic is not open,
// and errEphemeralBehind its answer when the sender's queue is full.
var (
	errEphemeralClosed = errors.New("the ephemeral topic is not open")
	errEphemeralBehind = errors.New("the ephemeral writer is behind; message dropped")
)

// StartEphemeral opens the ephemeral topic: its writer, and its reader at the
// last offset, handing every message read to deliver raw (deliver verifies and
// filters it, router.ReadEphemeral). The topic already open is left as it is;
// another closes it first. Only the engine calls this, and only with the name
// Core advertised.
func (c *Client) StartEphemeral(topic string, deliver func([]byte)) error {
	m := c.messaging() // outside c.mu: the config's lock is never taken under it
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(m.Kafka.Brokers) == 0 {
		return fmt.Errorf("no kafka brokers configured")
	}
	if c.eph != nil {
		if c.eph.topic == topic {
			return nil
		}
		c.closeEphemeralLocked()
	}
	run := &ephemeralRun{
		topic:  topic,
		reader: c.openEphemeral(m.Kafka.Brokers, topic),
		out:    make(chan kafkago.Message, ephemeralQueue),
		stop:   make(chan struct{}),
	}
	c.eph = run
	go run.send(c.openEphemeralWriter(m.Kafka.Brokers))
	go c.ephemeralReadLoop(run, deliver)
	return nil
}

// StopEphemeral closes the ephemeral topic's reader and writer. Idempotent.
func (c *Client) StopEphemeral() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeEphemeralLocked()
}

// closeEphemeralLocked ends the read loop, closes the reader and closes the
// sender's queue; the sender closes the writer once it has drained, on its own
// goroutine, so nothing here waits on a broker.
func (c *Client) closeEphemeralLocked() {
	if c.eph == nil {
		return
	}
	close(c.eph.stop)
	_ = c.eph.reader.Close()
	close(c.eph.out)
	c.eph = nil
}

// PublishEphemeral signs env and queues it for the ephemeral topic's sender:
// direct, never the outbox, one attempt, never waiting on the broker (a full
// queue drops it, errEphemeralBehind). It does not move LastPublish, which
// answers for the durable topic.
func (c *Client) PublishEphemeral(env *protocol.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	if len(c.SigningKey) > 0 {
		if data, err = protocol.Sign(data, c.SigningKey); err != nil {
			return fmt.Errorf("sign message: %w", err)
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.eph == nil {
		return errEphemeralClosed
	}
	select {
	case c.eph.out <- kafkago.Message{Topic: c.eph.topic, Key: []byte(c.PartitionKey), Value: data}:
		return nil
	default:
		return errEphemeralBehind
	}
}

// ephemeralReadLoop hands each message to deliver and commits nothing. A read
// error opens a new reader at the LAST offset after a backoff, so whatever was
// published meanwhile is skipped, not replayed. It ends when run is closed.
func (c *Client) ephemeralReadLoop(run *ephemeralRun, deliver func([]byte)) {
	bo := backoff.New(500*time.Millisecond, 5*time.Second)
	c.mu.RLock()
	reader := run.reader
	c.mu.RUnlock()
	for {
		msg, err := reader.FetchMessage(context.Background())
		if err != nil {
			timer := time.NewTimer(bo.Next())
			select {
			case <-run.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			m := c.messaging() // outside c.mu
			c.mu.Lock()
			if c.eph != run {
				c.mu.Unlock()
				return
			}
			_ = reader.Close()
			reader = c.openEphemeral(m.Kafka.Brokers, run.topic)
			run.reader = reader
			c.mu.Unlock()
			continue
		}
		bo.Reset()
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("PANIC kafka-ephemeral topic=%s: %v\n%s", run.topic, r, debug.Stack())
				}
			}()
			deliver(msg.Value)
		}()
	}
}

// EphemeralLink binds the client's ephemeral topic to one delivery, as the
// engine drives it (engine.EphemeralLink).
type EphemeralLink struct {
	Client  *Client
	Deliver func([]byte)
}

// Start opens the topic with the link's delivery.
func (l EphemeralLink) Start(topic string) error { return l.Client.StartEphemeral(topic, l.Deliver) }

// Stop closes it.
func (l EphemeralLink) Stop() { l.Client.StopEphemeral() }

// Publish sends one envelope on it.
func (l EphemeralLink) Publish(env *protocol.Envelope) error { return l.Client.PublishEphemeral(env) }
