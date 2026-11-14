package messaging

// ephemeral.go — Core's side of the ephemeral topic (protocol/ephemeral.go):
// it creates the topic, reads its retention back and corrects it, and only then
// names it to the Edges (EphemeralTopic) and opens its writer and its reader.
//
// THE ORDER IS THE GUARANTEE. The plant brokers auto-create a topic on first
// touch with their 7-day default (study S2), so the topic must exist with its
// own settings before anything reads it: Core creates it before it advertises
// it, and an Edge touches it only once advertised. A topic that cannot be
// verified is not advertised, not read and not written, and the live robot
// feed is off until a later connect verifies it.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingo/protocol/backoff"
)

// ephemeralRetentionMS is the ephemeral topic's retention.ms AND segment.ms
// (LEAD-NOTES D1). Both, because Kafka deletes whole closed segments only:
// with retention.ms alone the data stays until a 1 GiB segment rolls, which on
// this topic is days (study S2). The broker's cleaner adds up to its check
// interval (5 minutes at the plants) on top.
const ephemeralRetentionMS = "60000"

// ephemeralTopicConfig is what Core creates the topic with and verifies.
var ephemeralTopicConfig = []kafka.ConfigEntry{
	{ConfigName: "retention.ms", ConfigValue: ephemeralRetentionMS},
	{ConfigName: "segment.ms", ConfigValue: ephemeralRetentionMS},
}

// ephemeralQueue is how many messages may wait for the ephemeral writer. A
// publish that finds it full is dropped: the broker is behind or gone, and a
// message held longer would be read stale or not at all. One robot poll puts
// one message per watching station on it, so 32 is two polls of 16 stations.
const ephemeralQueue = 32

// ephemeralLink is the verified topic and what Core holds open on it: the
// sender's queue, and the reader once a handler subscribes.
type ephemeralLink struct {
	topic string
	// out is the sender's queue (send). Written under the client's read lock,
	// closed under its write lock, so no send can follow the close.
	out chan kafka.Message
	// reader is nil until a handler subscribes (SubscribeEphemeral).
	reader ephemeralReader
}

// ephemeralWriter is the part of *kafka.Writer the sender drives.
type ephemeralWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// openEphemeralLink starts the sender for topic over w.
func openEphemeralLink(topic string, w ephemeralWriter) *ephemeralLink {
	l := &ephemeralLink{topic: topic, out: make(chan kafka.Message, ephemeralQueue)}
	go l.send(w)
	return l
}

// send writes each queued message once, giving it EphemeralTTL, and closes the
// writer when the queue is closed. A failure is dropped: there is no retry
// (owner ruling), and the message would be stale by the time one landed.
func (l *ephemeralLink) send(w ephemeralWriter) {
	defer func() { _ = w.Close() }()
	for m := range l.out {
		ctx, cancel := context.WithTimeout(context.Background(), protocol.EphemeralTTL)
		_ = w.WriteMessages(ctx, m)
		cancel()
	}
}

// enqueue hands m to the sender without waiting; false when the queue is full
// and m is dropped.
func (l *ephemeralLink) enqueue(m kafka.Message) bool {
	select {
	case l.out <- m:
		return true
	default:
		return false
	}
}

// close closes the reader (its loop then ends on the client's stop) and the
// sender's queue; the sender closes the writer when it has drained it, on its
// own goroutine, so nothing here waits on a broker. Nil-safe. Called with the
// client's write lock held.
func (l *ephemeralLink) close() {
	if l == nil {
		return
	}
	if l.reader != nil {
		_ = l.reader.Close()
	}
	close(l.out)
}

// ephemeralReader is the part of *kafka.Reader the ephemeral read loop drives.
// It has no CommitMessages: the loop reads with no consumer group, so there is
// no offset to commit, and leaving the method out of the type makes a commit
// impossible to write rather than merely absent.
type ephemeralReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	Close() error
}

// newEphemeralReader opens a reader of the topic's one partition with NO
// consumer group, positioned at the LAST offset: it sees only what is published
// after it starts, so a reader started again after an outage replays nothing.
//
// The reader positions itself (SetOffset): kafka-go reads the start offset in a
// reader's config only for a consumer group, and a reader without one starts
// at the FIRST offset unless told otherwise, which on this topic is up to six
// minutes of history.
func newEphemeralReader(brokers []string, topic string) *kafka.Reader {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:   brokers,
		Topic:     topic,
		Partition: 0,
	})
	// Cannot fail on a reader that has not started (no group, not closed).
	_ = r.SetOffset(kafka.LastOffset)
	return r
}

// newEphemeralWriter is the topic's writer: one attempt per message, so a
// failed one is dropped, never retried (owner ruling: no outbox, no retry).
// It is driven by the link's sender alone, never by a publisher: even an
// asynchronous kafka-go writer looks up the topic's partitions inside
// WriteMessages, which against a broker that does not answer holds its caller
// for the transport's timeout (5 s, measured by
// TestPublishEphemeral_NeverHoldsThePollAndNeverRetries before the queue).
// RequiredAcks is set for the reason the durable writer's is (client.go): the
// zero value is RequireNone.
func newEphemeralWriter(brokers []string) *kafka.Writer {
	return &kafka.Writer{
		Addr:         kafka.TCP(brokers...),
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireOne,
		BatchTimeout: writerBatchTimeout,
		MaxAttempts:  1,
	}
}

// verifyEphemeralTopic creates the topic and checks it is fit to advertise:
// one partition (a reader with no group reads partition 0 only), and
// retention.ms and segment.ms as ephemeralTopicConfig sets them. An existing
// topic with other settings is altered and read again. Every round trip is
// bounded by ctx, so a broker that accepts and never answers cannot hold
// Connect, which holds the client's lock across this.
func verifyEphemeralTopic(ctx context.Context, admin *kafka.Client, topic string) error {
	created, err := admin.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{{
		Topic: topic, NumPartitions: 1, ReplicationFactor: 1, ConfigEntries: ephemeralTopicConfig,
	}}})
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if e := created.Errors[topic]; e != nil && !errors.Is(e, kafka.TopicAlreadyExists) {
		return fmt.Errorf("create: %w", e)
	}

	md, err := admin.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return fmt.Errorf("read partitions: %w", err)
	}
	partitions := -1
	for _, t := range md.Topics {
		if t.Name == topic {
			if t.Error != nil {
				return fmt.Errorf("read partitions: %w", t.Error)
			}
			partitions = len(t.Partitions)
		}
	}
	if partitions != 1 {
		return fmt.Errorf("it has %d partitions, and a reader with no consumer group reads partition 0 only; "+
			"delete the topic and Core will make it again", partitions)
	}

	got, err := describeEphemeral(ctx, admin, topic)
	if err != nil {
		return err
	}
	if ephemeralConfigOK(got) {
		return nil
	}
	set := make([]kafka.IncrementalAlterConfigsRequestConfig, len(ephemeralTopicConfig))
	for i, e := range ephemeralTopicConfig {
		set[i] = kafka.IncrementalAlterConfigsRequestConfig{Name: e.ConfigName, Value: e.ConfigValue,
			ConfigOperation: kafka.ConfigOperationSet}
	}
	altered, err := admin.IncrementalAlterConfigs(ctx, &kafka.IncrementalAlterConfigsRequest{
		Resources: []kafka.IncrementalAlterConfigsRequestResource{{
			ResourceType: kafka.ResourceTypeTopic, ResourceName: topic, Configs: set,
		}},
	})
	if err != nil {
		return fmt.Errorf("correct %v: %w", got, err)
	}
	for _, r := range altered.Resources {
		if r.Error != nil {
			return fmt.Errorf("correct %v: %w", got, r.Error)
		}
	}
	after, err := describeEphemeral(ctx, admin, topic)
	if err != nil {
		return err
	}
	if !ephemeralConfigOK(after) {
		return fmt.Errorf("corrected it, and it still reads %v", after)
	}
	log.Printf("messaging: ephemeral topic %s had %v; corrected to %s ms", topic, got, ephemeralRetentionMS)
	return nil
}

// describeEphemeral reads the topic's value of each ephemeralTopicConfig key.
func describeEphemeral(ctx context.Context, admin *kafka.Client, topic string) (map[string]string, error) {
	names := make([]string, len(ephemeralTopicConfig))
	for i, e := range ephemeralTopicConfig {
		names[i] = e.ConfigName
	}
	resp, err := admin.DescribeConfigs(ctx, &kafka.DescribeConfigsRequest{
		Resources: []kafka.DescribeConfigRequestResource{{
			ResourceType: kafka.ResourceTypeTopic, ResourceName: topic, ConfigNames: names,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	got := map[string]string{}
	for _, r := range resp.Resources {
		if r.Error != nil {
			return nil, fmt.Errorf("read config: %w", r.Error)
		}
		for _, e := range r.ConfigEntries {
			got[e.ConfigName] = e.ConfigValue
		}
	}
	return got, nil
}

// ephemeralConfigOK reports whether every ephemeralTopicConfig key reads as
// set.
func ephemeralConfigOK(got map[string]string) bool {
	for _, e := range ephemeralTopicConfig {
		if got[e.ConfigName] != e.ConfigValue {
			return false
		}
	}
	return true
}

// connectEphemeralLocked verifies the configured ephemeral topic and, when it
// verifies, opens its writer and (when a handler is subscribed) its reader.
// Called by Connect with c.mu held; c.kafka is set. An empty
// messaging.ephemeral_topic turns the feed off with nothing said; a topic that
// does not verify is logged once per connect and left alone.
func (c *Client) connectEphemeralLocked() {
	topic := c.cfg.EphemeralTopic
	if topic == "" {
		return
	}
	timeout := c.cfg.Kafka.DialTimeoutOr()
	transport := &kafka.Transport{DialTimeout: timeout}
	defer transport.CloseIdleConnections()
	admin := &kafka.Client{Addr: kafka.TCP(c.cfg.Kafka.Brokers...), Timeout: timeout, Transport: transport}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := verifyEphemeralTopic(ctx, admin, topic); err != nil {
		log.Printf("messaging: ephemeral topic %s not verified (%v): the live robot feed is off until Core next connects", topic, err)
		return
	}
	log.Printf("messaging: ephemeral topic %s verified (1 partition, retention.ms and segment.ms %s)", topic, ephemeralRetentionMS)
	c.kafka.ephemeral = openEphemeralLink(topic, newEphemeralWriter(c.cfg.Kafka.Brokers))
	c.startEphemeralReaderLocked()
}

// EphemeralTopic is the ephemeral topic this connection verified, "" when
// there is none (not connected, not configured, or not verified). It is what
// the registration and every heartbeat ack advertise.
func (c *Client) EphemeralTopic() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.kafka == nil || c.kafka.ephemeral == nil {
		return ""
	}
	return c.kafka.ephemeral.topic
}

// errNoEphemeralTopic is PublishEphemeral's answer with no verified topic, and
// errEphemeralBehind its answer when the sender's queue is full.
var (
	errNoEphemeralTopic = errors.New("no verified ephemeral topic")
	errEphemeralBehind  = errors.New("the ephemeral writer is behind; message dropped")
)

// PublishEphemeral signs env and queues it for the ephemeral topic's sender:
// direct, never through the outbox, one attempt. It never waits on the broker,
// so it cannot hold the robot poll that calls it: a full queue drops the
// message (errEphemeralBehind), and a write that fails is dropped by the sender.
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
	if c.kafka == nil || c.kafka.ephemeral == nil {
		return errNoEphemeralTopic
	}
	if !c.kafka.ephemeral.enqueue(kafka.Message{Topic: c.kafka.ephemeral.topic, Value: data}) {
		return errEphemeralBehind
	}
	return nil
}

// SubscribeEphemeral hands every message read off the ephemeral topic to
// handler, raw: the handler verifies and filters it (protocol.DecodeEphemeral).
// It is remembered like Subscribe's, so a reconnect that verifies the topic
// again reads it again; with no verified topic it reads nothing.
func (c *Client) SubscribeEphemeral(handler func(data []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ephemeralHandler = handler
	if c.kafka != nil {
		c.startEphemeralReaderLocked()
	}
}

// startEphemeralReaderLocked starts the read loop once per verified link.
func (c *Client) startEphemeralReaderLocked() {
	link := c.kafka.ephemeral
	if link == nil || link.reader != nil || c.ephemeralHandler == nil {
		return
	}
	link.reader = c.openEphemeral(c.cfg.Kafka.Brokers, link.topic)
	go c.ephemeralReadLoop(link, c.ephemeralHandler, c.stopChan)
}

// ephemeralReadLoop hands each message to handler and commits nothing. A read
// error closes the reader and opens a new one at the LAST offset after a
// backoff, so whatever was published meanwhile is skipped, not replayed. It
// ends when the client closes (stop) or a reconnect replaces the link.
func (c *Client) ephemeralReadLoop(link *ephemeralLink, handler func([]byte), stop <-chan struct{}) {
	bo := backoff.New(500*time.Millisecond, 5*time.Second)
	c.mu.RLock()
	reader := link.reader
	c.mu.RUnlock()
	for {
		msg, err := reader.FetchMessage(context.Background())
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			timer := time.NewTimer(bo.Next())
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			c.mu.Lock()
			if c.kafka == nil || c.kafka.ephemeral != link {
				c.mu.Unlock()
				return
			}
			_ = reader.Close()
			reader = c.openEphemeral(c.cfg.Kafka.Brokers, link.topic)
			link.reader = reader
			c.mu.Unlock()
			continue
		}
		bo.Reset()
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("kafka ephemeral handler panic: topic=%s: %v\n%s", link.topic, r, debug.Stack())
				}
			}()
			handler(msg.Value)
		}()
	}
}
