package messaging

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingocore/config"
)

// silentBroker accepts connections and never answers, the shape that would
// hold Connect if a step were unbounded.
func silentBroker(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, never answered
		}
	}()
	return ln.Addr().String()
}

// One attempt at the topic against a broker that accepts and never answers is
// held no longer than the probe timeout, as ensureTopics is
// (TestEnsureTopics_ASilentBrokerDoesNotHoldIt), and fails.
func TestAttemptEphemeral_ASilentBrokerDoesNotHoldIt(t *testing.T) {
	t.Parallel()
	done := make(chan error, 1)
	go func() { done <- attemptEphemeral([]string{silentBroker(t)}, 300*time.Millisecond, "shingo.ephemeral") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("an attempt against a silent broker verified the topic")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an attempt still waiting on a silent broker after 5 s; want it bounded by the 300 ms probe timeout")
	}
}

// ephemeralBroker stands in for the broker's answers to the topic's attempts:
// the first silent ones wait out the attempt's deadline and fail, the rest
// verify (or, with silent < 0, none ever does). It counts the attempts.
type ephemeralBroker struct {
	mu       sync.Mutex
	silent   int
	attempts int
}

func (b *ephemeralBroker) attempt(_ []string, timeout time.Duration, _ string) error {
	b.mu.Lock()
	b.attempts++
	n := b.attempts
	b.mu.Unlock()
	if b.silent < 0 || n <= b.silent {
		time.Sleep(timeout)
		return errors.New("i/o timeout")
	}
	return nil
}

func (b *ephemeralBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// retryingClient is a connected client whose topic attempts go to broker,
// retried every 20 ms under a 100 ms deadline, with a reader counter.
func retryingClient(t *testing.T, broker *ephemeralBroker) (c *Client, readers func() int) {
	t.Helper()
	c = NewClient(&config.MessagingConfig{
		Kafka:          config.KafkaConfig{Brokers: []string{"b:1"}, DialTimeout: 100 * time.Millisecond},
		EphemeralTopic: "shingo.ephemeral",
	})
	c.attemptEphemeral = broker.attempt
	c.ephemeralRetry = 20 * time.Millisecond
	var mu sync.Mutex
	opened := 0
	c.openEphemeral = func([]string, string) ephemeralReader {
		mu.Lock()
		opened++
		mu.Unlock()
		return newFakeEphemeralReader()
	}
	c.SubscribeEphemeral(func([]byte) {})
	t.Cleanup(c.Close)
	return c, func() int { mu.Lock(); defer mu.Unlock(); return opened }
}

// connect is what Connect does for the topic, against a connection already
// made; it reports how long Connect was held.
func connect(c *Client) time.Duration {
	start := time.Now()
	c.mu.Lock()
	c.kafka = &kafkaState{readers: map[string]kafkaReader{}}
	c.startEphemeralLocked()
	c.mu.Unlock()
	return time.Since(start)
}

// A broker that is silent at Core's connect and answers later ends with the
// topic advertised, read and written, and nothing waited on it: Connect is not
// held, and while an attempt is waiting out its deadline the robot poll's
// publish and the ack's read of the topic answer at once. It costs one attempt
// per retry interval while unverified and none once verified.
func TestEnsureEphemeral_ASilentThenAnsweringBrokerEndsAdvertised(t *testing.T) {
	t.Parallel()
	broker := &ephemeralBroker{silent: 2}
	c, readers := retryingClient(t, broker)
	if d := connect(c); d > 50*time.Millisecond {
		t.Errorf("Connect was held %s for the ephemeral topic, want not at all", d)
	}
	env, err := protocol.NewEnvelope(protocol.TypeRobotPositions, protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}, &protocol.RobotPositions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // inside the first attempt's 100 ms deadline
	start := time.Now()
	topic, perr := c.EphemeralTopic(), c.PublishEphemeral(env)
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Errorf("the ack's read and the poll's publish waited %s on an attempt, want at once", d)
	}
	if topic != "" || !errors.Is(perr, errNoEphemeralTopic) {
		t.Errorf("during a silent attempt: topic %q, publish %v; want none named and errNoEphemeralTopic", topic, perr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for c.EphemeralTopic() == "" {
		if time.Now().After(deadline) {
			t.Fatalf("after %d attempts the topic is still not named, want it named on the third", broker.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := readers(); n != 1 {
		t.Errorf("readers opened = %d once verified, want 1", n)
	}
	if err := c.PublishEphemeral(env); err != nil {
		t.Errorf("publish once verified: %v", err)
	}
	if n := broker.count(); n != 3 {
		t.Errorf("attempts = %d, want 3: two silent, one that verified", n)
	}
	time.Sleep(150 * time.Millisecond) // more than seven retry intervals
	if n := broker.count(); n != 3 {
		t.Errorf("attempts = %d after it verified, want still 3: none once verified", n)
	}
}

// A broker that never verifies the topic means it is never named, read or
// written, at one attempt per retry interval, and the attempts stop when the
// client closes.
func TestEnsureEphemeral_NeverVerifiedMeansNoReaderNoWriterNoName(t *testing.T) {
	t.Parallel()
	broker := &ephemeralBroker{silent: -1}
	c, readers := retryingClient(t, broker)
	c.ephemeralRetry = 50 * time.Millisecond
	c.cfg.Kafka.DialTimeout = time.Millisecond
	start := time.Now()
	connect(c)
	time.Sleep(330 * time.Millisecond)
	elapsed := time.Since(start)
	n := broker.count()
	if most := 1 + int(elapsed/c.ephemeralRetry); n < 3 || n > most {
		t.Errorf("%d attempts in %s at one every %s, want one at connect and one per interval (3 to %d)",
			n, elapsed.Round(time.Millisecond), c.ephemeralRetry, most)
	}
	if c.EphemeralTopic() != "" || readers() != 0 {
		t.Errorf("never verified: topic %q, %d readers; want none of either", c.EphemeralTopic(), readers())
	}
	c.mu.RLock()
	link := c.kafka.ephemeral
	c.mu.RUnlock()
	if link != nil {
		t.Error("never verified, and a writer's link is open")
	}
	c.Close()
	closed := broker.count()
	time.Sleep(150 * time.Millisecond)
	if after := broker.count(); after > closed+1 {
		t.Errorf("%d attempts after the client closed, want none (an attempt already running may finish)", after-closed)
	}
}

// The ephemeral reader joins no consumer group and starts at the LAST offset,
// so a reader opened after an outage replays nothing. kafka-go ignores the
// start offset in a reader's config without a group, so the offset is the
// reader's own.
func TestNewEphemeralReader_NoGroupFromTheLastOffset(t *testing.T) {
	t.Parallel()
	r := newEphemeralReader([]string{"127.0.0.1:1"}, "shingo.ephemeral")
	defer r.Close()
	if cfg := r.Config(); cfg.GroupID != "" || cfg.Topic != "shingo.ephemeral" || cfg.Partition != 0 {
		t.Errorf("reader config = group %q topic %q partition %d, want no group on shingo.ephemeral partition 0",
			cfg.GroupID, cfg.Topic, cfg.Partition)
	}
	if off := r.Offset(); off != kafka.LastOffset {
		t.Errorf("reader offset = %d, want kafka.LastOffset (%d): a reader at the first offset replays the topic", off, kafka.LastOffset)
	}
}

// stuckWriter is a writer whose broker does not answer: every write waits
// until released (then fails) or its deadline, and is counted.
type stuckWriter struct {
	mu      sync.Mutex
	writes  int
	release chan struct{}
	closed  chan struct{}
}

func (w *stuckWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	w.writes += len(msgs)
	w.mu.Unlock()
	select {
	case <-w.release:
		return errors.New("broker error")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *stuckWriter) Close() error { close(w.closed); return nil }

// A publish never holds its caller (the robot poll) and is tried once: it
// only queues, so a broker that never answers costs the poll nothing; a full
// queue drops; and each message reaches the writer once, failed or not. The
// writer itself is one attempt per message.
func TestPublishEphemeral_NeverHoldsThePollAndNeverRetries(t *testing.T) {
	t.Parallel()
	if w := newEphemeralWriter([]string{"127.0.0.1:1"}); w.MaxAttempts != 1 || w.RequiredAcks != kafka.RequireOne {
		t.Fatalf("writer attempts=%d acks=%v, want one attempt at RequireOne", w.MaxAttempts, w.RequiredAcks)
	}
	w := &stuckWriter{release: make(chan struct{}), closed: make(chan struct{})}
	c := NewClient(&config.MessagingConfig{})
	c.SigningKey = []byte("k")
	c.kafka = &kafkaState{ephemeral: openEphemeralLink("shingo.ephemeral", w)}
	env, err := protocol.NewEnvelope(protocol.TypeRobotPositions, protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}, &protocol.RobotPositions{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	accepted, dropped := 0, 0
	for range 100 {
		switch err := c.PublishEphemeral(env); {
		case err == nil:
			accepted++
		case errors.Is(err, errEphemeralBehind):
			dropped++
		default:
			t.Fatalf("publish: %v", err)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("100 publishes behind a broker that never answers took %s, want them queued or dropped at once", d)
	}
	if accepted > ephemeralQueue+1 || dropped == 0 {
		t.Errorf("accepted %d and dropped %d of 100, want at most the queue (%d) plus the one being written, the rest dropped",
			accepted, dropped, ephemeralQueue)
	}

	close(w.release)
	c.Close()
	select {
	case <-w.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer was not closed after the client closed and the queue drained")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writes != accepted {
		t.Errorf("the writer was handed %d messages for %d accepted, want each exactly once: no retry", w.writes, accepted)
	}
}

// fakeEphemeralReader is one reader the loop reads from: its messages, then
// a read error, then blocks until closed.
type fakeEphemeralReader struct {
	msgs   chan kafka.Message
	closed chan struct{}
	once   sync.Once
}

func newFakeEphemeralReader(values ...string) *fakeEphemeralReader {
	r := &fakeEphemeralReader{msgs: make(chan kafka.Message, len(values)+1), closed: make(chan struct{})}
	for _, v := range values {
		r.msgs <- kafka.Message{Value: []byte(v)}
	}
	return r
}

func (r *fakeEphemeralReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	select {
	case m := <-r.msgs:
		return m, nil
	case <-r.closed:
		return kafka.Message{}, errors.New("closed")
	default:
	}
	return kafka.Message{}, errors.New("broker gone")
}

func (r *fakeEphemeralReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// The read loop hands each message on and, on a read error, opens a NEW
// reader (at the last offset: newEphemeralReader) rather than resuming, so
// what was published while it could not read is skipped. The reader type has
// no CommitMessages, so the loop commits nothing.
func TestEphemeralReadLoop_ReopensRatherThanResumes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var opened []string
	var got []string
	readers := []*fakeEphemeralReader{newFakeEphemeralReader("a", "b"), newFakeEphemeralReader("c")}
	c := NewClient(&config.MessagingConfig{Kafka: config.KafkaConfig{Brokers: []string{"b:1"}}, EphemeralTopic: "e"})
	c.openEphemeral = func(_ []string, topic string) ephemeralReader {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, topic)
		if len(opened) <= len(readers) {
			return readers[len(opened)-1]
		}
		return newFakeEphemeralReader()
	}
	c.kafka = &kafkaState{ephemeral: openEphemeralLink("e", &stuckWriter{release: make(chan struct{}), closed: make(chan struct{})})}
	c.SubscribeEphemeral(func(v []byte) {
		mu.Lock()
		got = append(got, string(v))
		mu.Unlock()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("handed %v, want [a b c]: the first reader's two, then the reopened reader's", got)
	}
	if len(opened) < 2 {
		t.Errorf("opened %d readers, want a new one after the read error", len(opened))
	}
}
