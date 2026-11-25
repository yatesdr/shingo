package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	metadataAPI "github.com/segmentio/kafka-go/protocol/metadata"
	produceAPI "github.com/segmentio/kafka-go/protocol/produce"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
)

// TestHeartbeater_NodeListRequestQuotesTheCachedSceneRevision pins the Edge
// half of the geometry short-circuit: every node-list request names the scene
// revision the Edge already holds, so Core can leave the geometry off. An Edge
// with nothing cached — or one built before the field existed — sends the
// empty string, which Core reads as "send everything".
func TestHeartbeater_NodeListRequestQuotesTheCachedSceneRevision(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")

	if got := h.nodeListRequest().SceneRevision; got != "" {
		t.Errorf("no revision source wired, request carries %q — must be empty so Core sends the full scene", got)
	}

	h.SceneRevisionFn = func() string { return "rev-abc" }
	if got := h.nodeListRequest().SceneRevision; got != "rev-abc" {
		t.Errorf("request carries %q, want the cached revision rev-abc", got)
	}

	// The revision is read at SEND time, not captured at construction: a
	// cache replaced between two ticks must show up on the very next request.
	h.SceneRevisionFn = func() string { return "rev-def" }
	if got := h.nodeListRequest().SceneRevision; got != "rev-def" {
		t.Errorf("request carries %q after the cache moved to rev-def", got)
	}
}

// TestHeartbeater_CarriesTickLag: the periodic heartbeat carries the
// production-tick shipper's lag, read at send time, so Core can flag a station
// whose feed is stuck without a message of its own. Unwired, or a failed read,
// leaves the fields off the wire rather than reporting a zero nobody measured.
func TestHeartbeater_CarriesTickLag(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")
	if b := h.HeartbeatBody(); b.TickPending != nil || b.TickOldestUnsentAgeMS != nil {
		t.Errorf("unwired: lag fields = %v/%v, want absent", b.TickPending, b.TickOldestUnsentAgeMS)
	}
	h.TickLagFn = func() (int64, int64, bool) { return 3, 4200, true }
	b := h.HeartbeatBody()
	if b.TickPending == nil || *b.TickPending != 3 || b.TickOldestUnsentAgeMS == nil || *b.TickOldestUnsentAgeMS != 4200 {
		t.Errorf("lag fields = %v/%v, want 3/4200", b.TickPending, b.TickOldestUnsentAgeMS)
	}
	h.TickLagFn = func() (int64, int64, bool) { return 0, 0, false }
	if b := h.HeartbeatBody(); b.TickPending != nil {
		t.Errorf("failed read: lag fields present")
	}
}

// TestHeartbeater_FeedsAndTheReAsk: the heartbeat carries the feed digests the
// engine supplies, and the two-minute node and catalog re-ask is skipped only
// while Core answers feeds. Unwired, and with an older Core, the re-ask runs as
// it always did.
func TestHeartbeater_FeedsAndTheReAsk(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")
	if b := h.HeartbeatBody(); b.Feeds != nil {
		t.Errorf("unwired: feeds = %v, want nil (an older Edge's heartbeat)", b.Feeds)
	}
	if h.coreSpeaksFeeds() {
		t.Error("unwired: re-ask skipped, want it kept")
	}

	h.FeedsFn = func() map[string]string { return map[string]string{"catalog": "c1"} }
	if b := h.HeartbeatBody(); b.Feeds["catalog"] != "c1" {
		t.Errorf("wired: feeds = %v, want catalog c1", b.Feeds)
	}
	speaks := false
	h.CoreSpeaksFeedsFn = func() bool { return speaks }
	if h.coreSpeaksFeeds() {
		t.Error("older Core: re-ask skipped, want it kept")
	}
	speaks = true
	if !h.coreSpeaksFeeds() {
		t.Error("Core answers feeds: re-ask kept, want it skipped")
	}
}

// TestHeartbeater_BinWatchRidesTheBootBurst: SendBinWatch (called by Start and
// by a Kafka reconnect) hands BinWatchFn a publish; with none wired it does
// nothing.
func TestHeartbeater_BinWatchRidesTheBootBurst(t *testing.T) {
	t.Parallel()
	h := NewHeartbeater(nil, "edge.test", "v-test", "inst-1", "shingo.orders")
	h.SendBinWatch() // unwired: no panic, nothing sent
	called := 0
	h.BinWatchFn = func(publish func(*protocol.BinWatchList) error) { called++ }
	h.SendBinWatch()
	if called != 1 {
		t.Errorf("BinWatchFn called %d times, want 1", called)
	}
}

// fakeBroker stands under a Client's writer as its kafka-go transport. Down, it
// refuses every round trip, as an unreachable broker fails a publish at its
// first, the metadata request. Up, it answers the metadata and takes every
// produce, keeping the subject of each envelope it carried.
type fakeBroker struct {
	up       atomic.Bool
	refused  atomic.Int64
	mu       sync.Mutex
	subjects []string
}

func (b *fakeBroker) RoundTrip(_ context.Context, _ net.Addr, req kafkago.Request) (kafkago.Response, error) {
	if !b.up.Load() {
		b.refused.Add(1)
		return nil, errors.New("fake broker: connection refused")
	}
	switch r := req.(type) {
	case *metadataAPI.Request:
		res := &metadataAPI.Response{}
		for _, name := range r.TopicNames {
			res.Topics = append(res.Topics, metadataAPI.ResponseTopic{Name: name, Partitions: []metadataAPI.ResponsePartition{{}}})
		}
		return res, nil
	case *produceAPI.Request:
		res := &produceAPI.Response{}
		for _, topic := range r.Topics {
			rt := produceAPI.ResponseTopic{Topic: topic.Topic}
			for _, p := range topic.Partitions {
				for {
					rec, err := p.RecordSet.Records.ReadRecord()
					if err != nil {
						break
					}
					var env struct {
						P struct {
							Subject string `json:"subject"`
						} `json:"p"`
					}
					// A record the fake cannot read is kept as a subject
					// that names why, so a count over the subjects fails
					// with the reason instead of reading as an empty one.
					subject := ""
					if raw, err := io.ReadAll(rec.Value); err != nil {
						subject = fmt.Sprintf("<unreadable record: %v>", err)
					} else if err := json.Unmarshal(raw, &env); err != nil {
						subject = fmt.Sprintf("<undecodable record: %v>", err)
					} else {
						subject = env.P.Subject
					}
					b.mu.Lock()
					b.subjects = append(b.subjects, subject)
					b.mu.Unlock()
				}
				rt.Partitions = append(rt.Partitions, produceAPI.ResponsePartition{Partition: p.Partition})
			}
			res.Topics = append(res.Topics, rt)
		}
		return res, nil
	}
	return nil, fmt.Errorf("fake broker: no answer for %T", req)
}

// took is how many envelopes of each subject the broker took.
func (b *fakeBroker) took() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := map[string]int{}
	for _, s := range b.subjects {
		n[s]++
	}
	return n
}

// TestHeartbeater_ABootBurstThatMetNoBrokerGoesOutWhenItAnswers is docket B14:
// an Edge whose broker is down when it boots sends its boot burst (register,
// node list and catalog requests) when the broker answers again, once. The
// burst goes out only at Start and on a Kafka reconnect; the loop sends
// heartbeats, and the node list and catalog only to a Core that does not
// answer feeds. So a register refused at boot was never sent again, and Core,
// which asks for one only from a station it does not know, kept the Edge's
// previous run (the plant incident at main.go's reconnect hook).
//
// A real Heartbeater on a real Client, over a fake broker: one try a send (a
// sender with its stop closed, so a refusal costs no 2+4+8 s backoff), a 5 ms
// interval, and a Core that answers feeds, so the loop re-asks nothing.
func TestHeartbeater_ABootBurstThatMetNoBrokerGoesOutWhenItAnswers(t *testing.T) {
	t.Parallel()
	broker := &fakeBroker{}
	c := NewClient(func() config.MessagingConfig { return config.MessagingConfig{} })
	c.kafkaW = &kafkago.Writer{Addr: kafkago.TCP("broker.test:9092"), Transport: broker, Balancer: &kafkago.Hash{},
		RequiredAcks: kafkago.RequireOne, BatchTimeout: time.Millisecond}
	t.Cleanup(c.Close)
	h := NewHeartbeater(c, "edge.b14", "v-test", "inst-1", "shingo.orders")
	oneTry := make(chan struct{})
	close(oneTry)
	h.sender = NewDataSender(c, "shingo.orders", oneTry)
	h.interval = 5 * time.Millisecond
	h.CoreSpeaksFeedsFn = func() bool { return true }
	var afterBursts atomic.Int64
	h.AfterOwedBurst = func() error { afterBursts.Add(1); return nil }

	h.Start()
	t.Cleanup(h.Stop)
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("waited 10 s for %s", what)
			}
		}
	}
	// The burst's four sends and two of the loop's heartbeats, refused.
	waitFor("six refused sends", func() bool { return broker.refused.Load() >= 6 })
	if n := len(broker.took()); n != 0 {
		t.Fatalf("the broker took %d subjects while down", n)
	}

	broker.up.Store(true)
	waitFor("six heartbeats taken", func() bool { return broker.took()[protocol.SubjectEdgeHeartbeat] >= 6 })
	took := broker.took()
	for _, s := range []string{protocol.SubjectEdgeRegister, protocol.SubjectNodeListRequest, protocol.SubjectCatalogPayloadsRequest} {
		if took[s] != 1 {
			t.Errorf("after the broker came back it took %d %s, want 1: the boot burst it refused goes out "+
				"with the loop's first heartbeat that gets out, once", took[s], s)
		}
	}
	// What main owes after the burst (its boot reconcile), once, after it.
	if n := afterBursts.Load(); n != 1 {
		t.Errorf("AfterOwedBurst ran %d times, want once, after the late burst", n)
	}
}

// TestHeartbeater_ASyncPressSendsOnceAndAnswersWhetherKafkaTookIt is docket
// R72. A Sync button's press (www apiSyncCoreNodes, apiSyncPayloadCatalog)
// asks through the engine's door (RequestNodeSync, RequestCatalogSync), which
// calls the hook main.go sets from this heartbeater. With Kafka down the press
// went through the sender's three tries (2+4+8 s of backoff) and then answered
// ok, though nothing was sent. A press should send once and answer at once: ok
// only when Kafka took the ask, refused when it did not. The boot burst, the
// loop and the resyncs Core asks for keep the sender's tries (B14's test above).
//
// A real engine (built as the www door test builds one) and a real Heartbeater
// on a real Client over the fake broker, wired as main.go's
// setupKafkaSubscribers wires them, with the sender NewHeartbeater makes (its
// stop open, so a second try waits out its backoff). "At once" is read as an
// order, not a time: the answer comes before any second try.
func TestHeartbeater_ASyncPressSendsOnceAndAnswersWhetherKafkaTookIt(t *testing.T) {
	t.Parallel()
	for _, door := range []struct {
		name, subject string
		press         func(*engine.Engine) error
	}{
		{"node sync", protocol.SubjectNodeListRequest, (*engine.Engine).RequestNodeSync},
		{"catalog sync", protocol.SubjectCatalogPayloadsRequest, (*engine.Engine).RequestCatalogSync},
	} {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			broker := &fakeBroker{}
			c := NewClient(func() config.MessagingConfig { return config.MessagingConfig{} })
			c.kafkaW = &kafkago.Writer{Addr: kafkago.TCP("broker.test:9092"), Transport: broker, Balancer: &kafkago.Hash{},
				RequiredAcks: kafkago.RequireOne, BatchTimeout: time.Millisecond}
			t.Cleanup(c.Close)
			h := NewHeartbeater(c, "edge.r72", "v-test", "inst-1", "shingo.orders")
			t.Cleanup(h.Stop)
			cfg := config.Defaults()
			cfg.StationUID = "edge.r72"
			eng := engine.New(engine.Config{AppConfig: cfg, DB: testdb.Open(t), LogFunc: func(string, ...any) {}})
			eng.SetNodeSyncFunc(h.RequestNodeSync)
			eng.SetCatalogSyncFunc(h.RequestCatalogSync)

			// Kafka down: the answer, or a second try, whichever comes first.
			answer := make(chan error, 1)
			go func() { answer <- door.press(eng) }()
			triedAgain, watched := make(chan struct{}), make(chan struct{})
			defer close(watched)
			go func() {
				for broker.refused.Load() < 2 {
					select {
					case <-watched:
						return
					case <-time.After(time.Millisecond):
					}
				}
				close(triedAgain)
			}()
			var err error
			select {
			case err = <-answer:
			case <-triedAgain:
				t.Errorf("Kafka down: the press tried a second time before it answered: a press sends once, " +
					"with no retry ladder, and answers at once")
				h.Stop() // the sender's backoff ends at its stop
				err = <-answer
			case <-time.After(30 * time.Second):
				t.Fatal("Kafka down: the press neither answered nor tried again in 30 s")
			}
			if err == nil || !strings.Contains(err.Error(), "Kafka did not take it") {
				t.Errorf("Kafka down: the press answered %v, want refused with \"Kafka did not take it\": "+
					"the ask was not sent, and ok is only for an ask Kafka took", err)
			}
			if n := broker.refused.Load(); n != 1 {
				t.Errorf("Kafka down: the press tried %d times, want 1", n)
			}

			// Kafka up: the ask taken once, and the press answered ok.
			broker.up.Store(true)
			if err := door.press(eng); err != nil {
				t.Errorf("Kafka up: the press answered %v, want ok", err)
			}
			if n := broker.took()[door.subject]; n != 1 {
				t.Errorf("Kafka up: the broker took %d %s, want 1", n, door.subject)
			}
		})
	}
}

// TestHeartbeater_TheBootBurstEndsWithAHeartbeat is a source-shape pin (the
// sender has no seam, as TestFeedsPin_HeartbeatLoopReasksEverySecondTick): the
// burst Start and a Kafka reconnect send is the register, the node list and
// catalog requests, the bins watch, and then one heartbeat, so the first ack
// is one round trip away instead of the loop's first tick. Start sends the
// burst before the loop begins.
func TestHeartbeater_TheBootBurstEndsWithAHeartbeat(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("heartbeat.go")
	if err != nil {
		t.Fatalf("read heartbeat.go: %v", err)
	}
	for _, c := range []struct {
		name string
		re   *regexp.Regexp
	}{
		{"the burst, in order, ending with a heartbeat", regexp.MustCompile(
			`func \(h \*Heartbeater\) SendBootBurst\(\) \{\s*h\.sendRegister\(\)\s*h\.sendNodeListRequest\(\)\s*` +
				`h\.sendCatalogRequest\(\)\s*h\.SendBinWatch\(\)\s*h\.sendHeartbeat\(\)\s*\}`)},
		{"Start sends the burst, then the loop", regexp.MustCompile(
			`func \(h \*Heartbeater\) Start\(\) \{\s*h\.SendBootBurst\(\)\s*go h\.loop\(\)\s*\}`)},
	} {
		if !c.re.Match(src) {
			t.Errorf("%s: heartbeat.go does not match", c.name)
		}
	}
}
