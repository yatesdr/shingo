package messaging

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingo/protocol/backoff"
	"shingo/protocol/types"
	"shingoedge/config"
)

// DebugLogFunc is a nil-safe debug logging function.
type DebugLogFunc = types.DebugLogFunc

// writerBatchTimeout bounds how long the Kafka writer waits to fill a batch
// before flushing it.
//
// kafka-go defaults this to 1 SECOND when left unset, and the outbox drainer
// (protocol/outbox) publishes synchronously, one message at a time — so the
// writer's batch never reaches BatchSize and every single Publish blocks for
// the full default timeout. That capped the Edge->Core wire at ~1 msg/sec.
// Hopkinsville generates ~0.7-0.9 msg/sec of telemetry, i.e. 70-90% of that
// budget, so bursts queued for minutes: order.complex_request measured a 252s
// mean and a 446s worst case in the edge outbox on 2026-07-28, which the
// operator saw as orders wedged in "submitted".
//
// Keep the writer SYNCHRONOUS. The drainer relies on WriteMessages returning
// the publish error to drive its retry and dead-letter path; setting
// Async: true would swallow that and silently drop messages.
const writerBatchTimeout = 10 * time.Millisecond

// Client is the Kafka messaging client.
type Client struct {
	mu         sync.RWMutex
	cfg        *config.MessagingConfig
	kafkaW     *kafkago.Writer
	kafkaR     *kafkago.Reader
	stopChan   chan struct{}
	SigningKey []byte // optional HMAC key; when set, outbound messages are signed

	// lastPublish is the most recent Publish outcome, for LastPublish().
	// Separate from the mutex above so /status never contends with a publish.
	lastPublish atomic.Pointer[publishOutcome]

	// PartitionKey is stamped on every outbound message as the Kafka record
	// key. On the Edge it is the station uid; on Core it is the destination
	// station where the publish path knows one.
	//
	// TWO HONEST CAVEATS, because "add a key" is stated as a fix more often
	// than it is one:
	//
	//  1. Topics are created with NumPartitions=1
	//     (shingo-core/messaging/client.go ensureTopics), and one partition
	//     makes every partitioning scheme identical. This buys nothing until
	//     the partition count is raised, which is a broker-side operation and
	//     not part of this change.
	//  2. The writer's balancer used to be kafka.LeastBytes, which NEVER READS
	//     msg.Key — it routes by accumulated bytes per partition (verified in
	//     kafka-go v0.4.50 balancer.go: Balance() touches Key only to add
	//     len(Key) to a counter). So a key under LeastBytes is inert even with
	//     many partitions. The balancer is kafka.Hash below for that reason;
	//     under one partition the switch is provably a no-op, which is what
	//     makes it safe to make now rather than during the change that needs it.
	//
	// What it does buy today: the key travels with the record, so a message
	// dumped off the topic carries its station without being decoded, and the
	// day partitions are added the per-station ordering and per-station
	// consumer assignment are already correct.
	PartitionKey string

	DebugLog DebugLogFunc
}

// NewClient creates a messaging client based on config.
func NewClient(cfg *config.MessagingConfig) *Client {
	return &Client{
		cfg:      cfg,
		stopChan: make(chan struct{}),
	}
}

// Connect establishes the Kafka connection.
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.cfg.Kafka.Brokers) == 0 {
		return fmt.Errorf("no kafka brokers configured")
	}

	c.kafkaW = &kafkago.Writer{
		Addr:         kafkago.TCP(c.cfg.Kafka.Brokers...),
		Balancer:     &kafkago.Hash{},
		RequiredAcks: kafkago.RequireOne,
		BatchTimeout: writerBatchTimeout,
	}
	c.DebugLog.Log("connected to brokers %v", c.cfg.Kafka.Brokers)
	return nil
}

// Reconnect closes the existing writer and creates a new one using the
// current config values. This is needed after broker addresses are changed
// at runtime because kafkago.TCP resolves the address at creation time.
func (c *Client) Reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.cfg.Kafka.Brokers) == 0 {
		return fmt.Errorf("no kafka brokers configured")
	}

	if c.kafkaW != nil {
		c.kafkaW.Close()
	}

	c.kafkaW = &kafkago.Writer{
		Addr:         kafkago.TCP(c.cfg.Kafka.Brokers...),
		Balancer:     &kafkago.Hash{},
		RequiredAcks: kafkago.RequireOne,
		BatchTimeout: writerBatchTimeout,
	}

	log.Printf("kafka writer reconnected to %v", c.cfg.Kafka.Brokers)
	c.DebugLog.Log("reconnected to brokers %v", c.cfg.Kafka.Brokers)
	return nil
}

// Publish sends a message to the given topic.
func (c *Client) Publish(topic string, payload []byte) (err error) {
	// Registered first so it runs LAST, after the read lock is released.
	// Every exit path is recorded, not just the WriteMessages result: the
	// field answers "did the last publish attempt work", and an unset writer
	// or a signing failure are attempts that did not.
	defer func() { c.recordPublish(err) }()

	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.kafkaW == nil {
		return fmt.Errorf("kafka writer not initialized")
	}

	// Sign outbound messages if signing key is configured
	if len(c.SigningKey) > 0 {
		signed, err := protocol.Sign(payload, c.SigningKey)
		if err != nil {
			return fmt.Errorf("sign message: %w", err)
		}
		payload = signed
	}

	c.DebugLog.Log("publish topic=%s len=%d", topic, len(payload))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.kafkaW.WriteMessages(ctx, kafkago.Message{
		Topic: topic,
		Key:   []byte(c.PartitionKey),
		Value: payload,
	})
}

// PublishEnvelope encodes and publishes a protocol envelope to the given topic.
func (c *Client) PublishEnvelope(topic string, env interface{ Encode() ([]byte, error) }) error {
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	return c.Publish(topic, data)
}

// Subscribe registers a handler for messages on the given topic.
// The consumer goroutine automatically reconnects on errors with
// exponential backoff capped at 5 seconds.
func (c *Client) Subscribe(topic string, handler func(payload []byte)) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.kafkaW == nil {
		return fmt.Errorf("kafka not connected")
	}
	c.kafkaR = kafkago.NewReader(kafkago.ReaderConfig{
		Brokers: c.cfg.Kafka.Brokers,
		Topic:   topic,
		GroupID: c.cfg.Kafka.GroupID,
	})
	c.DebugLog.Log("subscribed to topic=%s group=%s", topic, c.cfg.Kafka.GroupID)
	go c.readLoop(topic, handler)
	return nil
}

// readLoop reads messages from Kafka, reconnecting on errors with
// exponential backoff (500ms base, capped at 5s, with ±20% jitter).
func (c *Client) readLoop(topic string, handler func(payload []byte)) {
	bo := backoff.New(500*time.Millisecond, 5*time.Second)

	for {
		c.mu.RLock()
		reader := c.kafkaR
		c.mu.RUnlock()

		if reader == nil {
			return
		}

		msg, err := reader.ReadMessage(context.Background())
		if err != nil {
			// Check if we're shutting down
			select {
			case <-c.stopChan:
				return
			default:
			}

			jittered := bo.Next()
			log.Printf("kafka read error: %v, reconnecting in %v", err, jittered.Round(time.Millisecond))

			timer := time.NewTimer(jittered)
			select {
			case <-c.stopChan:
				timer.Stop()
				return
			case <-timer.C:
			}

			// Recreate the reader
			c.mu.Lock()
			if c.kafkaR != nil {
				c.kafkaR.Close()
			}
			c.kafkaR = kafkago.NewReader(kafkago.ReaderConfig{
				Brokers: c.cfg.Kafka.Brokers,
				Topic:   topic,
				GroupID: c.cfg.Kafka.GroupID,
			})
			c.mu.Unlock()
			c.DebugLog.Log("reader reconnected for topic=%s", topic)
			continue
		}

		// Reset backoff on successful read
		bo.Reset()
		c.DebugLog.Log("recv topic=%s len=%d", topic, len(msg.Value))

		// Wrap the handler call in defer recover() so a panic in any
		// downstream handler doesn't kill the consumer goroutine.
		//
		// IMPORTANT: kafka-go's ReadMessage with a consumer-group
		// config auto-commits the offset on successful return. A panic
		// here does NOT cause the message to replay — the offset has
		// already advanced. We just log and continue. If anyone
		// switches to manual commit mode (CommitMessages), THIS
		// WRAPPER BECOMES AN INFINITE-REPLAY WEDGE. Update the wrapper
		// to advance the offset before continuing.
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("PANIC kafka-readLoop topic=%s: %v\n%s",
						topic, r, debug.Stack())
				}
			}()
			handler(msg.Value)
		}()
	}
}

// IsConnected reports that a Kafka WRITER EXISTS. It is not broker
// reachability, and callers that want that must use LastPublish.
//
// Connect() performs no I/O — kafkago.TCP resolves lazily — so it cannot fail
// except on an empty broker list, and this returns true from the first Connect
// until Close regardless of whether the broker has been reachable since. The
// drainer nonetheless keys its opening guard off this, and must: a false here
// stops the drain entirely, so making it mean "reachable" would stop retrying
// exactly when retrying is the point.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.kafkaW != nil
}

// publishOutcome is the result of the most recent Publish attempt. Stored
// behind a single atomic pointer so the flag and its timestamp are always read
// as one value — reading two atomics could report a fresh time against a stale
// verdict.
type publishOutcome struct {
	ok bool
	at time.Time
}

func (c *Client) recordPublish(err error) {
	c.lastPublish.Store(&publishOutcome{ok: err == nil, at: time.Now()})
}

// LastPublish reports the outcome of the most recent Publish attempt.
//
// ever is false until something has actually been published, so a freshly
// booted Edge does not report a failure it never had. This is the field that
// answers "is Kafka reachable" — IsConnected does not.
func (c *Client) LastPublish() (ok bool, at time.Time, ever bool) {
	o := c.lastPublish.Load()
	if o == nil {
		return false, time.Time{}, false
	}
	return o.ok, o.at, true
}

// Close shuts down the messaging connection.
func (c *Client) Close() {
	// Signal readLoop to stop
	select {
	case <-c.stopChan:
	default:
		close(c.stopChan)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kafkaW != nil {
		c.kafkaW.Close()
		c.kafkaW = nil
	}
	if c.kafkaR != nil {
		c.kafkaR.Close()
		c.kafkaR = nil
	}
}
