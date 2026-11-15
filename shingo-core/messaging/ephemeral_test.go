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
	"shingo/protocol/clock"
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

// THE RETRY PINS ARE STEPPED, NEVER TIMED. They used to stand in a broker that
// slept out a 100 ms deadline and a retry interval of 20 or 50 ms of real time,
// and to judge "at once" and "one per interval" by the wall clock, so a loaded
// machine failed them ("waited 77.316ms on an attempt, want at once",
// 2026-11-14). Now the pin answers each attempt itself (steppedBroker) and moves
// a manual clock itself (steppedClock): nothing sleeps, and the only wall-clock
// figure left is the 5 s a correct loop never comes near (within).

// steppedBroker stands in for the broker's answers to the topic's attempts, one
// at a time: an attempt says it has begun on started (its number) and waits for
// the pin's answer on answers (nil verifies). It counts the attempts. end
// releases an attempt still waiting when the pin is over.
type steppedBroker struct {
	mu       sync.Mutex
	attempts int
	started  chan int
	answers  chan error
	done     chan struct{}
	once     sync.Once
}

func newSteppedBroker(t *testing.T) *steppedBroker {
	b := &steppedBroker{started: make(chan int), answers: make(chan error), done: make(chan struct{})}
	t.Cleanup(b.end)
	return b
}

func (b *steppedBroker) end() { b.once.Do(func() { close(b.done) }) }

func (b *steppedBroker) attempt(_ []string, _ time.Duration, _ string) error {
	b.mu.Lock()
	b.attempts++
	n := b.attempts
	b.mu.Unlock()
	select {
	case b.started <- n:
	case <-b.done:
		return errors.New("test over")
	}
	select {
	case err := <-b.answers:
		return err
	case <-b.done:
		return errors.New("test over")
	}
}

func (b *steppedBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// answer hands the waiting attempt its result.
func (b *steppedBroker) answer(t *testing.T, err error) {
	t.Helper()
	select {
	case b.answers <- err:
	case <-time.After(5 * time.Second):
		t.Fatal("no attempt was waiting for its answer")
	}
}

// steppedClock is a manual clock that says each time the retry loop starts a
// wait on it (After), with the wait's length, so the pin moves time on only
// once the loop is waiting.
type steppedClock struct {
	*clock.Manual
	waits chan time.Duration
	done  chan struct{}
	once  sync.Once
}

func newSteppedClock(t *testing.T) *steppedClock {
	c := &steppedClock{Manual: clock.NewManual(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)),
		waits: make(chan time.Duration), done: make(chan struct{})}
	t.Cleanup(c.end)
	return c
}

func (c *steppedClock) end() { c.once.Do(func() { close(c.done) }) }

func (c *steppedClock) After(d time.Duration) <-chan time.Time {
	ch := c.Manual.After(d)
	select {
	case c.waits <- d:
	case <-c.done:
	}
	return ch
}

// within is the next value on ch, failing the pin if none comes in 5 s: a
// bound for a loop that has stopped or is waiting on the wrong thing, never a
// measure of how fast a correct one is.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not come", what)
	}
	var zero T
	return zero
}

// noAttempt fails the pin if an attempt has begun.
func noAttempt(t *testing.T, b *steppedBroker, when string) {
	t.Helper()
	select {
	case n := <-b.started:
		t.Errorf("attempt %d began %s", n, when)
	default:
	}
}

// heldEphemeralReader is a reader that reads nothing until it is closed, so a
// verified topic opens exactly one.
type heldEphemeralReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *heldEphemeralReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	<-r.closed
	return kafka.Message{}, errors.New("closed")
}

func (r *heldEphemeralReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// retryingClient is a client whose topic attempts go to broker and whose waits
// between them are clk's, at the production interval, with a reader counter.
func retryingClient(t *testing.T, broker *steppedBroker, clk *steppedClock) (c *Client, readers func() int) {
	t.Helper()
	c = NewClient(&config.MessagingConfig{
		Kafka:          config.KafkaConfig{Brokers: []string{"b:1"}, DialTimeout: 100 * time.Millisecond},
		EphemeralTopic: "shingo.ephemeral",
	})
	c.attemptEphemeral = broker.attempt
	c.ephemeralClock = clk
	var mu sync.Mutex
	opened := 0
	c.openEphemeral = func([]string, string) ephemeralReader {
		mu.Lock()
		opened++
		mu.Unlock()
		return &heldEphemeralReader{closed: make(chan struct{})}
	}
	c.SubscribeEphemeral(func([]byte) {})
	// The stand-ins let go first, so a pin that fails with an attempt or a
	// wait outstanding still closes the client.
	t.Cleanup(func() { broker.end(); clk.end(); c.Close() })
	return c, func() int { mu.Lock(); defer mu.Unlock(); return opened }
}

// connect is what Connect does for the topic, against a connection already
// made: it starts the retry loop (startEphemeralLocked) and returns.
func connect(c *Client) {
	c.mu.Lock()
	c.kafka = &kafkaState{readers: map[string]kafkaReader{}}
	c.startEphemeralLocked()
	c.mu.Unlock()
}

// runLoop is the retry loop for a connection already made, as
// startEphemeralLocked starts it, run here so the pin sees it end: the
// returned channel closes when it returns.
func runLoop(c *Client) <-chan struct{} {
	c.mu.Lock()
	state := &kafkaState{readers: map[string]kafkaReader{}}
	c.kafka = state
	stop := c.stopChan
	c.mu.Unlock()
	ended := make(chan struct{})
	go func() {
		c.ensureEphemeral(state, c.cfg.EphemeralTopic, c.cfg.Kafka.Brokers, c.cfg.Kafka.DialTimeoutOr(), stop)
		close(ended)
	}()
	return ended
}

// A broker that is silent at Core's connect and answers later ends with the
// topic advertised, read and written, and nothing waited on it: Connect is not
// held, and while an attempt is waiting on the broker the robot poll's publish
// and the ack's read of the topic answer. It costs one attempt at once, then
// one each time the 60 s interval passes while unverified, and none once
// verified.
func TestEnsureEphemeral_ASilentThenAnsweringBrokerEndsAdvertised(t *testing.T) {
	t.Parallel()
	env, err := protocol.NewEnvelope(protocol.TypeRobotPositions, protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}, &protocol.RobotPositions{})
	if err != nil {
		t.Fatal(err)
	}

	// NOTHING WAITS ON AN ATTEMPT. Connect returns, and the ack's read and the
	// poll's publish answer, while the first attempt is still waiting on the
	// broker: had either waited on it, it would wait for good, since the pin
	// never answers this attempt.
	held := newSteppedBroker(t)
	hc, _ := retryingClient(t, held, newSteppedClock(t))
	connected := make(chan struct{})
	go func() { connect(hc); close(connected) }()
	if n := within(t, held.started, "the attempt at connect"); n != 1 {
		t.Fatalf("the attempt at connect is attempt %d, want 1", n)
	}
	within(t, connected, "Connect, while the attempt waits on the broker")
	var topic string
	var perr error
	read := make(chan struct{})
	go func() { topic, perr = hc.EphemeralTopic(), hc.PublishEphemeral(env); close(read) }()
	within(t, read, "the ack's read and the poll's publish, while the attempt waits on the broker")
	if topic != "" || !errors.Is(perr, errNoEphemeralTopic) {
		t.Errorf("during a silent attempt: topic %q, publish %v; want none named and errNoEphemeralTopic", topic, perr)
	}

	// THE CADENCE, stepped: two silent attempts, then one that verifies.
	broker, clk := newSteppedBroker(t), newSteppedClock(t)
	c, readers := retryingClient(t, broker, clk)
	ended := runLoop(c)
	within(t, broker.started, "the attempt at connect")
	broker.answer(t, errors.New("i/o timeout"))
	if d := within(t, clk.waits, "the wait after a silent attempt"); d != time.Minute {
		t.Errorf("the wait between attempts is %s, want 60 s", d)
	}
	clk.Advance(time.Minute - time.Nanosecond)
	noAttempt(t, broker, "before the interval had passed")
	clk.Advance(time.Nanosecond)
	if n := within(t, broker.started, "the attempt once the interval passed"); n != 2 {
		t.Errorf("the attempt after one interval is attempt %d, want 2", n)
	}
	broker.answer(t, errors.New("i/o timeout"))
	within(t, clk.waits, "the wait after the second silent attempt")
	clk.Advance(time.Minute)
	within(t, broker.started, "the third attempt")
	broker.answer(t, nil)
	within(t, ended, "the loop's end once the topic verified")

	if got := c.EphemeralTopic(); got != "shingo.ephemeral" {
		t.Fatalf("after %d attempts the topic is %q, want it named on the third", broker.count(), got)
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
	clk.Advance(10 * time.Minute)
	if n := broker.count(); n != 3 {
		t.Errorf("attempts = %d ten intervals after it verified, want still 3: none once verified", n)
	}
}

// A broker that never verifies the topic means it is never named, read or
// written, at one attempt at connect and one each time the 60 s interval
// passes, and the attempts stop when the client closes.
func TestEnsureEphemeral_NeverVerifiedMeansNoReaderNoWriterNoName(t *testing.T) {
	t.Parallel()
	broker, clk := newSteppedBroker(t), newSteppedClock(t)
	c, readers := retryingClient(t, broker, clk)
	ended := runLoop(c)
	for i := 1; i <= 3; i++ {
		if n := within(t, broker.started, "an attempt"); n != i {
			t.Fatalf("attempt %d began where attempt %d was due", n, i)
		}
		broker.answer(t, errors.New("i/o timeout"))
		if d := within(t, clk.waits, "the wait after a silent attempt"); d != time.Minute {
			t.Errorf("the wait between attempts is %s, want 60 s", d)
		}
		if i < 3 {
			clk.Advance(time.Minute - time.Nanosecond)
			noAttempt(t, broker, "before the interval had passed")
			clk.Advance(time.Nanosecond)
		}
	}
	if n := broker.count(); n != 3 {
		t.Errorf("%d attempts at connect and over two intervals, want 3: one at connect and one per interval", n)
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
	within(t, ended, "the loop's end once the client closed")
	clk.Advance(10 * time.Minute)
	if after := broker.count(); after != 3 {
		t.Errorf("%d attempts after the client closed, want none", after-3)
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
