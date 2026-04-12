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

type Client struct {
	mu         sync.RWMutex
	cfg        *config.MessagingConfig
	kafka      *kafkaState
	handlers   map[string]MessageHandler
	stopChan   chan struct{}
	closeOnce  sync.Once
	SigningKey []byte // optional HMAC key; when set, outbound messages are signed
	DebugLog   func(string, ...any)
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			Addr:     kafka.TCP(c.cfg.Kafka.Brokers...),
			Balancer: &kafka.LeastBytes{},
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

	for {
		msg, err := reader.ReadMessage(context.Background())
		if err != nil {
			select {
			case <-c.stopChan:
				return
			default:
			}

			jittered := bo.Next()
			log.Printf("kafka read error: topic=%s: %v, reconnecting in %v", topic, err, jittered.Round(time.Millisecond))
			c.dbg("read error: topic=%s error=%v backoff=%v", topic, err, jittered.Round(time.Millisecond))

			timer := time.NewTimer(jittered)
			select {
			case <-c.stopChan:
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
	c.closeOnce.Do(func() {
		close(c.stopChan)
	})

	c.mu.Lock()
	defer c.mu.Unlock()

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
