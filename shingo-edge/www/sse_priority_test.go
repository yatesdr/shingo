package www

import (
	"testing"
	"time"
)

// waitForType reads a client's queues until a frame of the named type arrives
// or the timeout elapses.
func waitForType(c *sseClient, typ string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-c.events:
			if evt.Type == typ {
				return true
			}
		case evt := <-c.lossy:
			if evt.Type == typ {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestSSE_LossyFloodDoesNotEvictStateChanges is the regression guard for the
// queue split. Every topic used to share one 64-slot queue, so a client that
// fell behind lost whatever arrived next. counter-read and debug-log were the
// bulk of the traffic — 4,512 of 5,929 drops in 24 h at one plant — so what
// they evicted was everything else, including order-update. counter-read is
// no longer broadcast at all; the flood here is the debug feed, to a client
// that asked for it.
func TestSSE_LossyFloodDoesNotEvictStateChanges(t *testing.T) {
	h := NewEventHub()
	h.Start()
	defer h.Stop()

	c := &sseClient{
		events: make(chan SSEEvent, 64),
		lossy:  make(chan SSEEvent, 16),
		debug:  true,
	}
	h.register(c)
	defer h.unregister(c)

	// Well past both queues' capacity, with nothing reading.
	for i := range 1000 {
		h.Broadcast(SSEEvent{Type: "debug-log", Data: i})
		if i%16 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)

	h.Broadcast(SSEEvent{Type: "order-update", Data: "landed"})

	if !waitForType(c, "order-update", 2*time.Second) {
		t.Fatal("order-update was dropped after a diagnostic flood — the two " +
			"classes are sharing capacity again, so logging and PLC polling " +
			"can evict the frames the operator's board is edge-triggered on")
	}
}

// TestSSE_LossyDropsDoNotEvictClient pins the eviction rule. A client behind on
// diagnostics is not stuck, so shedding those frames must not evict it (a
// durable drop does, at once) — otherwise a debug-log burst disconnects a
// healthy tab. The client asked for debug (?debug=1), so the frames reach its
// lossy queue and overflow it rather than being filtered out before.
func TestSSE_LossyDropsDoNotEvictClient(t *testing.T) {
	h := NewEventHub()
	h.Start()
	defer h.Stop()

	c := &sseClient{
		events: make(chan SSEEvent, 64),
		lossy:  make(chan SSEEvent, 1),
		debug:  true,
	}
	h.register(c)
	defer h.unregister(c)

	for i := range 100 {
		h.Broadcast(SSEEvent{Type: "debug-log", Data: i})
	}
	time.Sleep(100 * time.Millisecond)

	if !registered(h, c) {
		t.Fatal("client was evicted by a diagnostic flood — lossy drops must " +
			"not evict; only a durable drop says a client lost state")
	}
	if c.queued.Load() != 0 {
		t.Fatalf("queued = %d, want 0 — a lossy frame must not move the seq the heartbeat reports", c.queued.Load())
	}
}

// TestSSE_RobotPosFamilyIsOneStationExactly pins how the lossy rule takes the
// robot panel's per-station family (robotPosTopicPrefix): the prefix and a
// positive decimal station id as strconv writes it, and nothing wider. Every
// near miss stays durable, as every other name does.
func TestSSE_RobotPosFamilyIsOneStationExactly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		typ     string
		station int64
	}{
		{"robot-pos.1", 1},
		{"robot-pos.12", 12},
		{"robot-pos.9223372036854775807", 9223372036854775807},
		{"robot-pos", 0},
		{"robot-pos.", 0},
		{"robot-pos.0", 0},
		{"robot-pos.-3", 0},
		{"robot-pos.+3", 0},
		{"robot-pos.007", 0},
		{"robot-pos.1.2", 0},
		{"robot-pos.12a", 0},
		{"robot-pos. 12", 0},
		{"robot-pos.9223372036854775808", 0},
		{"robot-positions", 0},
		{"Robot-pos.1", 0},
		{"order-update", 0},
	} {
		id, ok := robotPosStation(c.typ)
		if ok != (c.station != 0) || id != c.station {
			t.Errorf("robotPosStation(%q) = %d, %v; want %d, %v", c.typ, id, ok, c.station, c.station != 0)
		}
		if got := lossySSETopic(c.typ); got != (c.station != 0) {
			t.Errorf("lossySSETopic(%q) = %v; only the family and debug-log are lossy", c.typ, got)
		}
		if c.station != 0 && robotPosTopic(c.station) != c.typ {
			t.Errorf("robotPosTopic(%d) = %q, want %q", c.station, robotPosTopic(c.station), c.typ)
		}
	}
	if !lossySSETopic("debug-log") {
		t.Error("debug-log left the lossy set")
	}
}

// TestSSE_RobotPosDropsDoNotEvictClient: the family is shed like the debug
// feed. A panel behind on positions is not stuck; the next position supersedes
// the one it lost, so a flood must not end its stream.
func TestSSE_RobotPosDropsDoNotEvictClient(t *testing.T) {
	h := NewEventHub()
	h.Start()
	defer h.Stop()

	c := &sseClient{
		events: make(chan SSEEvent, 64),
		lossy:  make(chan SSEEvent, 1),
		topics: map[string]bool{"robot-pos.4": true},
	}
	h.register(c)
	defer h.unregister(c)

	for i := range 100 {
		h.Broadcast(SSEEvent{Type: "robot-pos.4", Data: i})
	}
	time.Sleep(100 * time.Millisecond)

	if !registered(h, c) {
		t.Fatal("a robot-pos flood evicted the panel's client: the family must be shed, never evict")
	}
	if len(c.events) != 0 || c.queued.Load() != 0 {
		t.Fatalf("robot-pos frames reached the durable queue (%d queued, seq %d): they ride the lossy one",
			len(c.events), c.queued.Load())
	}
}

// TestSSE_PLCStatusStaysDurable guards the classification. plc-status is keyed
// by plcName and plc-health-alert is an alarm; neither may be shed as
// diagnostics just because the PLC feed is chatty.
func TestSSE_PLCStatusStaysDurable(t *testing.T) {
	for _, typ := range []string{"plc-status", "plc-health-alert", "counter-update", "order-update"} {
		if lossySSETopics[typ] {
			t.Errorf("%s is classified lossy — it carries state the operator or "+
				"engine acts on and must keep its reserved capacity", typ)
		}
	}
}
