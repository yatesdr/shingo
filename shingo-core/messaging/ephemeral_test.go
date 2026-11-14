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

// A broker that accepts and never answers holds the ephemeral topic's
// verification no longer than the probe timeout, as it holds ensureTopics
// (TestEnsureTopics_ASilentBrokerDoesNotHoldIt), and a topic it could not
// verify is not advertised, so no Edge is told to touch it.
func TestEnsureEphemeralTopic_ASilentBrokerDoesNotHoldItAndIsNotAdvertised(t *testing.T) {
	t.Parallel()
	c := NewClient(&config.MessagingConfig{
		Kafka:          config.KafkaConfig{Brokers: []string{silentBroker(t)}, DialTimeout: 300 * time.Millisecond},
		EphemeralTopic: "shingo.ephemeral",
	})
	c.kafka = &kafkaState{readers: map[string]kafkaReader{}}
	c.SubscribeEphemeral(func([]byte) { t.Error("a reader was started on a topic that never verified") })
	done := make(chan struct{})
	go func() {
		c.mu.Lock()
		c.connectEphemeralLocked()
		c.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ephemeral topic's verification still waiting on a silent broker after 5 s; want it bounded by the 300 ms probe timeout")
	}
	if got := c.EphemeralTopic(); got != "" {
		t.Errorf("EphemeralTopic = %q after a verification that could not run, want \"\" (not advertised)", got)
	}
	env, err := protocol.NewEnvelope(protocol.TypeRobotPositions, protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}, &protocol.RobotPositions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PublishEphemeral(env); !errors.Is(err, errNoEphemeralTopic) {
		t.Errorf("PublishEphemeral with no verified topic = %v, want errNoEphemeralTopic", err)
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
