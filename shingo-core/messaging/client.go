package messaging

import (
	"context"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingo/protocol/backoff"
	"shingocore/config"
)

type MessageHandler func(topic string, payload []byte)

// writerBatchTimeout bounds how long the Kafka writer waits to fill a batch
// before flushing it.
//
// kafka-go defaults this to 1 SECOND when left unset, and the outbox drainer
// (protocol/outbox) publishes synchronously, one message at a time — so the
// writer's batch never reaches BatchSize and every single Publish blocks for
// the full default timeout, capping the wire at ~1 msg/sec. See the matching
// constant in shingo-edge/messaging for the Hopkinsville incident that surfaced
// this (2026-07-28: orders wedged in "submitted" behind minutes of queued
// telemetry).
//
// Keep the writer SYNCHRONOUS. The drainer relies on WriteMessages returning
// the publish error to drive its retry and dead-letter path; setting
// Async: true would swallow that and silently drop messages.
const writerBatchTimeout = 10 * time.Millisecond

type Client struct {
	mu         sync.RWMutex
	cfg        *config.MessagingConfig
	kafka      *kafkaState
	handlers   map[string]MessageHandler
	stopChan   chan struct{}
	closeOnce  sync.Once
	SigningKey []byte // optional HMAC key; when set, outbound messages are signed
	DebugLog   func(string, ...any)

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
}

type kafkaState struct {
	readers map[string]*kafka.Reader
	writer  *kafka.Writer
}

func NewClient(cfg *config.MessagingConfig) *Client {
	return &Client{
		cfg:      cfg,
		handlers: make(map[string]MessageHandler),
		stopChan: make(chan struct{}),
	}
}

func (c *Client) dbg(format string, args ...any) {
	if fn := c.DebugLog; fn != nil {
		fn(format, args...)
	}
}

func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.cfg.Kafka.Brokers) == 0 {
		return fmt.Errorf("no kafka brokers configured")
	}

	// Verify at least one broker is reachable
	var conn *kafka.Conn
	var connErr error
	for _, broker := range c.cfg.Kafka.Brokers {
		c.dbg("connect: probing broker %s", broker)
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Kafka.DialTimeoutOr())
		conn, connErr = kafka.DialContext(ctx, "tcp", broker)
		cancel()
		if connErr == nil {
			log.Printf("messaging: kafka connected to %s", broker)
			c.dbg("connect: broker %s ok", broker)
			break
		}
		c.dbg("connect: broker %s failed: %v", broker, connErr)
	}
	if connErr != nil {
		return fmt.Errorf("kafka connect: %w", connErr)
	}

	// Ensure configured topics exist before setting up readers/writer
	c.ensureTopics(conn, c.cfg.OrdersTopic, c.cfg.DispatchTopic)
	conn.Close()

	c.kafka = &kafkaState{
		readers: make(map[string]*kafka.Reader),
		writer: &kafka.Writer{
			Addr:         kafka.TCP(c.cfg.Kafka.Brokers...),
			Balancer:     &kafka.Hash{},
			BatchTimeout: writerBatchTimeout,
		},
	}
	return nil
}

func (c *Client) Publish(topic string, payload []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.kafka == nil || c.kafka.writer == nil {
		return fmt.Errorf("kafka not connected")
	}

	// Sign outbound messages if signing key is configured
	if len(c.SigningKey) > 0 {
		signed, err := protocol.Sign(payload, c.SigningKey)
		if err != nil {
			return fmt.Errorf("sign message: %w", err)
		}
		payload = signed
	}

	c.dbg("publish: topic=%s size=%d", topic, len(payload))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.kafka.writer.WriteMessages(ctx, kafka.Message{
		Topic: topic,
		Key:   []byte(c.PartitionKey),
		Value: payload,
	})
}

// ensureTopics creates Kafka topics if they don't already exist.
// Requires a live connection to any broker; uses it to discover the
// controller and issue CreateTopics. Errors are logged but not fatal
// since the broker may have auto.create.topics.enable=true anyway.
func (c *Client) ensureTopics(conn *kafka.Conn, topics ...string) {
	if len(topics) == 0 {
		return
	}

	controller, err := conn.Controller()
	if err != nil {
		log.Printf("messaging: cannot find controller for topic creation: %v", err)
		return
	}

	controllerAddr := net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port))
	controllerConn, err := kafka.Dial("tcp", controllerAddr)
	if err != nil {
		log.Printf("messaging: cannot connect to controller: %v", err)
		return
	}
	defer controllerConn.Close()

	configs := make([]kafka.TopicConfig, len(topics))
	for i, t := range topics {
		configs[i] = kafka.TopicConfig{
			Topic:             t,
			NumPartitions:     1,
			ReplicationFactor: 1,
		}
	}

	if err := controllerConn.CreateTopics(configs...); err != nil {
		log.Printf("messaging: topic auto-create: %v", err)
	} else {
		log.Printf("messaging: ensured topics exist: %v", topics)
	}
}

func (c *Client) Subscribe(topic string, handler MessageHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.handlers[topic] = handler

	if c.kafka == nil {
		return fmt.Errorf("kafka not connected")
	}
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: c.cfg.Kafka.Brokers,
		Topic:   topic,
		GroupID: c.cfg.Kafka.GroupID,
	})
	c.kafka.readers[topic] = reader
	c.dbg("subscribe: topic=%s group=%s", topic, c.cfg.Kafka.GroupID)
	go c.readLoop(topic, reader, handler)
	return nil
}

// readLoop reads messages from Kafka, reconnecting on errors with
// exponential backoff (500ms base, capped at 5s, with ±20% jitter).
func (c *Client) readLoop(topic string, reader *kafka.Reader, handler MessageHandler) {
	bo := backoff.New(500*time.Millisecond, 5*time.Second)

	// Capture our stop channel once under the lock. Reconfigure swaps c.stopChan
	// (under the lock) and closes the old one; selecting on this local instead of
	// re-reading the field every iteration both removes the data race and makes
	// this loop exit on ITS stop signal rather than silently following the swap
	// to the new channel and never stopping.
	c.mu.RLock()
	stop := c.stopChan
	c.mu.RUnlock()

	for {
		msg, err := reader.ReadMessage(context.Background())
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}

			jittered := bo.Next()
			log.Printf("kafka read error: topic=%s: %v, reconnecting in %v", topic, err, jittered.Round(time.Millisecond))
			c.dbg("read error: topic=%s error=%v backoff=%v", topic, err, jittered.Round(time.Millisecond))

			timer := time.NewTimer(jittered)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}

			// Recreate the reader
			c.mu.Lock()
			reader.Close()
			reader = kafka.NewReader(kafka.ReaderConfig{
				Brokers: c.cfg.Kafka.Brokers,
				Topic:   topic,
				GroupID: c.cfg.Kafka.GroupID,
			})
			if c.kafka != nil {
				c.kafka.readers[topic] = reader
			}
			c.mu.Unlock()

			bo.Reset()
			continue
		}

		bo.Reset()
		c.dbg("received: topic=%s size=%d", msg.Topic, len(msg.Value))
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("kafka handler panic: topic=%s: %v\n%s", topic, r, debug.Stack())
				}
			}()
			handler(msg.Topic, msg.Value)
		}()
	}
}

// PublishEnvelope encodes and publishes a protocol envelope to the given topic.
func (c *Client) PublishEnvelope(topic string, env interface{ Encode() ([]byte, error) }) error {
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	c.dbg("publish envelope: topic=%s size=%d", topic, len(data))
	return c.Publish(topic, data)
}

func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.kafka != nil
}

// EnsureConnected keeps retrying Connect() in the background until it succeeds
// (or the client is Closed), then starts readers for every handler that was
// registered via Subscribe while the connection was down.
//
// This closes the startup ordering race behind the Springfield "Kafka down"
// wedge (2026-07-24): when the box reboots, shingo-core and its co-located
// Kafka broker restart together, and core boots first — the initial Connect()
// gets `connection refused`, Subscribe() then returns "kafka not connected"
// without creating a reader, and nothing retries. The process runs Kafka-dead
// (no dispatch out, no ingest in) until someone restarts it by hand. With this,
// core reconnects the moment the broker is up and restores its subscriptions.
//
// Only the INITIAL connect needs this. Once a reader exists, readLoop's own
// backoff keeps it alive across later broker blips, and kafka.Writer manages
// its own reconnects. No-op when already connected. Idempotent-safe: guarded by
// IsConnected so a second call while the first goroutine is still retrying just
// returns.
func (c *Client) EnsureConnected() {
	if c.IsConnected() {
		return
	}
	// Capture the stop channel once, like readLoop, so a Reconfigure swap can't
	// make this goroutine follow the new channel and never stop.
	c.mu.RLock()
	stop := c.stopChan
	c.mu.RUnlock()

	go func() {
		bo := backoff.New(1*time.Second, 30*time.Second)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := c.Connect(); err != nil {
				d := bo.Next()
				log.Printf("messaging: kafka connect failed (%v); retrying in %v", err, d.Round(time.Millisecond))
				timer := time.NewTimer(d)
				select {
				case <-stop:
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			log.Printf("messaging: kafka connected on retry — restoring subscriptions")
			c.restoreSubscriptions()
			return
		}
	}()
}

// restoreSubscriptions starts a reader for every handler registered via
// Subscribe. Called after a successful late Connect() so subscriptions that
// no-op'd while the broker was down (Subscribe stores the handler but skips the
// reader when not connected) come to life. Snapshots under the lock, then calls
// Subscribe (which locks itself) outside it.
func (c *Client) restoreSubscriptions() {
	c.mu.RLock()
	handlers := make(map[string]MessageHandler, len(c.handlers))
	for topic, h := range c.handlers {
		handlers[topic] = h
	}
	c.mu.RUnlock()
	for topic, handler := range handlers {
		if err := c.Subscribe(topic, handler); err != nil {
			log.Printf("messaging: restore subscription %s: %v", topic, err)
		}
	}
}

// Reconfigure closes the existing connection and reconnects with new config.
// All previously registered subscriptions are automatically restored.
func (c *Client) Reconfigure(cfg *config.MessagingConfig) error {
	c.Close()
	c.mu.Lock()
	c.cfg = cfg
	c.stopChan = make(chan struct{})
	c.closeOnce = sync.Once{}
	// Snapshot handlers before releasing lock
	handlers := make(map[string]MessageHandler, len(c.handlers))
	for k, v := range c.handlers {
		handlers[k] = v
	}
	c.mu.Unlock()

	if err := c.Connect(); err != nil {
		return err
	}

	// Re-subscribe all previously registered handlers
	for topic, handler := range handlers {
		if err := c.Subscribe(topic, handler); err != nil {
			log.Printf("messaging: re-subscribe %s after reconfigure: %v", topic, err)
		}
	}
	return nil
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closeOnce.Do(func() {
		close(c.stopChan)
	})

	if c.kafka != nil {
		for _, r := range c.kafka.readers {
			r.Close()
		}
		if c.kafka.writer != nil {
			c.kafka.writer.Close()
		}
		c.kafka = nil
	}
}
