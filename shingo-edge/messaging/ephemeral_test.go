package messaging

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingoedge/config"
)

// The Edge's ephemeral reader joins no consumer group and starts at the LAST
// offset: an Edge that comes back from an outage is handed nothing published
// while it was gone. kafka-go ignores the start offset in a reader's config
// without a group, so the offset is the reader's own.
func TestNewEphemeralReader_NoGroupFromTheLastOffset(t *testing.T) {
	t.Parallel()
	r := newEphemeralReader([]string{"127.0.0.1:1"}, "shingo.ephemeral")
	defer r.Close()
	if cfg := r.Config(); cfg.GroupID != "" || cfg.Topic != "shingo.ephemeral" || cfg.Partition != 0 {
		t.Errorf("reader config = group %q topic %q partition %d, want no group on shingo.ephemeral partition 0",
			cfg.GroupID, cfg.Topic, cfg.Partition)
	}
	if off := r.Offset(); off != kafkago.LastOffset {
		t.Errorf("reader offset = %d, want kafkago.LastOffset (%d)", off, kafkago.LastOffset)
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

func (w *stuckWriter) WriteMessages(ctx context.Context, msgs ...kafkago.Message) error {
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

// idleReader reads nothing until closed.
type idleReader struct{ closed chan struct{} }

func (r idleReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	<-r.closed
	return kafkago.Message{}, errors.New("closed")
}

func (r idleReader) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

// Nothing is sent before the engine opens the topic or after it closes it; a
// publish while open only queues, so a broker that never answers holds no
// caller, a full queue drops, and each message reaches the writer once, failed
// or not (one attempt, no retry); and none of it moves LastPublish, which
// answers for the durable topic.
func TestEphemeral_OpenOnlyBetweenStartAndStopAndNeverWaits(t *testing.T) {
	t.Parallel()
	if w := newEphemeralWriter([]string{"127.0.0.1:1"}); w.MaxAttempts != 1 || w.RequiredAcks != kafkago.RequireOne {
		t.Fatalf("writer attempts=%d acks=%v, want one attempt at RequireOne", w.MaxAttempts, w.RequiredAcks)
	}
	stuck := &stuckWriter{release: make(chan struct{}), closed: make(chan struct{})}
	c := NewClient(func() config.MessagingConfig {
		return config.MessagingConfig{Kafka: config.KafkaConfig{Brokers: []string{"127.0.0.1:1"}}}
	})
	c.openEphemeral = func([]string, string) ephemeralReader { return idleReader{closed: make(chan struct{})} }
	c.openEphemeralWriter = func([]string) ephemeralWriter { return stuck }
	env, err := protocol.NewEnvelope(protocol.TypeRobotWatch, protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"},
		protocol.Address{Role: protocol.RoleCore}, &protocol.RobotWatch{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PublishEphemeral(env); !errors.Is(err, errEphemeralClosed) {
		t.Fatalf("publish before the topic is open = %v, want errEphemeralClosed", err)
	}
	if err := c.StartEphemeral("shingo.ephemeral", func([]byte) {}); err != nil {
		t.Fatalf("start: %v", err)
	}
	start := time.Now()
	accepted, dropped := 0, 0
	for range 30 {
		switch err := c.PublishEphemeral(env); {
		case err == nil:
			accepted++
		case errors.Is(err, errEphemeralBehind):
			dropped++
		default:
			t.Fatalf("publish while open: %v", err)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("30 publishes behind a broker that never answers took %s, want them queued or dropped at once", d)
	}
	if accepted > ephemeralQueue+1 || dropped == 0 {
		t.Errorf("accepted %d and dropped %d, want at most the queue (%d) plus the one being written", accepted, dropped, ephemeralQueue)
	}
	c.StopEphemeral()
	c.StopEphemeral()
	if err := c.PublishEphemeral(env); !errors.Is(err, errEphemeralClosed) {
		t.Errorf("publish after the topic closed = %v, want errEphemeralClosed", err)
	}
	close(stuck.release)
	select {
	case <-stuck.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer was not closed once its queue drained")
	}
	stuck.mu.Lock()
	if stuck.writes != accepted {
		t.Errorf("the writer was handed %d messages for %d accepted, want each exactly once", stuck.writes, accepted)
	}
	stuck.mu.Unlock()
	if _, _, ever := c.LastPublish(); ever {
		t.Error("an ephemeral publish moved LastPublish, which answers for the durable topic")
	}
}
