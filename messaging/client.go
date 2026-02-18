package messaging

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/segmentio/kafka-go"

	"shingocore/config"
)

type MessageHandler func(topic string, payload []byte)

type Client struct {
	mu       sync.RWMutex
	cfg      *config.MessagingConfig
	mqtt     mqtt.Client
	kafka    *kafkaState
	backend  string
	handlers map[string]MessageHandler
}

type kafkaState struct {
	readers map[string]*kafka.Reader
	writer  *kafka.Writer
}

func NewClient(cfg *config.MessagingConfig) *Client {
	return &Client{
		cfg:      cfg,
		backend:  cfg.Backend,
		handlers: make(map[string]MessageHandler),
	}
}

func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.backend {
	case "mqtt":
		return c.connectMQTT()
	case "kafka":
		return c.connectKafka()
	default:
		return fmt.Errorf("unsupported messaging backend: %s", c.backend)
	}
}

func (c *Client) connectMQTT() error {
	broker := fmt.Sprintf("tcp://%s:%d", c.cfg.MQTT.Broker, c.cfg.MQTT.Port)
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(c.cfg.MQTT.ClientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			log.Printf("messaging: mqtt connection lost: %v", err)
		}).
		SetOnConnectHandler(func(client mqtt.Client) {
			log.Printf("messaging: mqtt connected to %s", broker)
			c.mu.RLock()
			defer c.mu.RUnlock()
			for topic, handler := range c.handlers {
				h := handler
				client.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
					h(msg.Topic(), msg.Payload())
				})
			}
		})

	c.mqtt = mqtt.NewClient(opts)
	token := c.mqtt.Connect()
	if ok := token.WaitTimeout(5 * time.Second); !ok {
		return fmt.Errorf("mqtt connect timeout (will retry in background)")
	}
	return token.Error()
}

func (c *Client) connectKafka() error {
	if len(c.cfg.Kafka.Brokers) == 0 {
		return fmt.Errorf("no kafka brokers configured")
	}

	// Verify at least one broker is reachable
	var conn *kafka.Conn
	var connErr error
	for _, broker := range c.cfg.Kafka.Brokers {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, connErr = kafka.DialContext(ctx, "tcp", broker)
		cancel()
		if connErr == nil {
			conn.Close()
			log.Printf("messaging: kafka connected to %s", broker)
			break
		}
	}
	if connErr != nil {
		return fmt.Errorf("kafka connect: %w", connErr)
	}

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

	switch c.backend {
	case "mqtt":
		if c.mqtt == nil {
			return fmt.Errorf("mqtt not connected")
		}
		token := c.mqtt.Publish(topic, 1, false, payload)
		token.Wait()
		return token.Error()
	case "kafka":
		if c.kafka == nil || c.kafka.writer == nil {
			return fmt.Errorf("kafka not connected")
		}
		return c.kafka.writer.WriteMessages(context.Background(), kafka.Message{
			Topic: topic,
			Value: payload,
		})
	default:
		return fmt.Errorf("unsupported backend: %s", c.backend)
	}
}

func (c *Client) Subscribe(topic string, handler MessageHandler) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.handlers[topic] = handler

	switch c.backend {
	case "mqtt":
		if c.mqtt == nil || !c.mqtt.IsConnected() {
			return nil // will subscribe on connect
		}
		token := c.mqtt.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
			handler(msg.Topic(), msg.Payload())
		})
		token.Wait()
		return token.Error()
	case "kafka":
		if c.kafka == nil {
			return fmt.Errorf("kafka not connected")
		}
		reader := kafka.NewReader(kafka.ReaderConfig{
			Brokers: c.cfg.Kafka.Brokers,
			Topic:   topic,
			GroupID: c.cfg.MQTT.ClientID,
		})
		c.kafka.readers[topic] = reader
		go func() {
			for {
				msg, err := reader.ReadMessage(context.Background())
				if err != nil {
					return
				}
				handler(msg.Topic, msg.Value)
			}
		}()
		return nil
	default:
		return fmt.Errorf("unsupported backend: %s", c.backend)
	}
}

func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	switch c.backend {
	case "mqtt":
		return c.mqtt != nil && c.mqtt.IsConnected()
	case "kafka":
		return c.kafka != nil
	default:
		return false
	}
}

// Reconfigure closes the existing connection and reconnects with new config.
func (c *Client) Reconfigure(cfg *config.MessagingConfig) error {
	c.Close()
	c.mu.Lock()
	c.cfg = cfg
	c.backend = cfg.Backend
	c.mu.Unlock()
	return c.Connect()
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.backend {
	case "mqtt":
		if c.mqtt != nil {
			c.mqtt.Disconnect(1000)
		}
	case "kafka":
		if c.kafka != nil {
			for _, r := range c.kafka.readers {
				r.Close()
			}
			if c.kafka.writer != nil {
				c.kafka.writer.Close()
			}
		}
	}
}
