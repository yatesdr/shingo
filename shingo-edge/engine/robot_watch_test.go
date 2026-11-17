package engine

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
)

// fakeEphemeralLink records what the robot watch loop does to the link.
type fakeEphemeralLink struct {
	mu      sync.Mutex
	starts  []string
	stops   int
	watches []protocol.RobotWatch
	open    bool
}

func (l *fakeEphemeralLink) Start(topic string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts = append(l.starts, topic)
	l.open = true
	return nil
}

func (l *fakeEphemeralLink) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stops++
	l.open = false
}

func (l *fakeEphemeralLink) Publish(env *protocol.Envelope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open {
		return fmt.Errorf("published on a closed link")
	}
	if env.Type != protocol.TypeRobotWatch {
		return fmt.Errorf("published %s", env.Type)
	}
	var w protocol.RobotWatch
	if err := env.DecodePayload(&w); err != nil {
		return err
	}
	l.watches = append(l.watches, w)
	return nil
}

func (l *fakeEphemeralLink) state() (starts []string, stops int, watches []protocol.RobotWatch) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.starts), l.stops, slices.Clone(l.watches)
}

// robotWatchEngine is an engine with only what the feed reads, its loop
// running at a resend interval of every, and a log the test can read.
func robotWatchEngine(t *testing.T, configured string, every time.Duration) (*Engine, *fakeEphemeralLink, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	e := &Engine{
		cfg: &config.Config{StationUID: "edge.a", Messaging: config.MessagingConfig{EphemeralTopic: configured}},
		logFn: func(f string, a ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(f, a...))
			mu.Unlock()
		},
		debugFn:  func(string, ...any) {},
		stopChan: make(chan struct{}),
	}
	e.startRobotWatchLoop(every)
	t.Cleanup(func() { close(e.stopChan) })
	link := &fakeEphemeralLink{}
	e.SetEphemeralLink(link)
	return e, link, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

// eventually waits up to 2 s for cond, the loop being on its own goroutine.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("after 2 s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRobotWatch_SentOnlyWhileShownAndNamed pins the Edge's sender: no topic
// from Core, no link and no watch; a named topic opens the link; the watch is
// the union across operator stations, sent at once on a change and again
// every interval; an empty union sends nothing; a later ack without the name
// closes the link. Nothing of it is logged.
func TestRobotWatch_SentOnlyWhileShownAndNamed(t *testing.T) {
	t.Parallel()
	e, link, logged := robotWatchEngine(t, "shingo.ephemeral", 30*time.Millisecond)

	e.SetRobotWatch(1, []string{"u-2", "u-1", "", "u-2"})
	time.Sleep(100 * time.Millisecond)
	if starts, _, watches := link.state(); len(starts) != 0 || len(watches) != 0 {
		t.Fatalf("with no topic named by Core: starts %v, %d watches; want none of either", starts, len(watches))
	}

	e.OfferEphemeralTopic("shingo.ephemeral")
	eventually(t, "the named topic opened and one watch went", func() bool {
		starts, _, watches := link.state()
		return len(starts) == 1 && len(watches) >= 1
	})
	if !e.RobotPositionsAvailable() {
		t.Error("the link is open and RobotPositionsAvailable says not")
	}
	_, _, watches := link.state()
	if w := watches[0]; w.StationID != "edge.a" || !slices.Equal(w.OrderUUIDs, []string{"u-1", "u-2"}) {
		t.Errorf("first watch = %+v, want edge.a watching [u-1 u-2]", w)
	}

	// A second operator station: the union, at once.
	e.SetRobotWatch(2, []string{"u-3", "u-1"})
	eventually(t, "the union [u-1 u-2 u-3] went", func() bool {
		_, _, ws := link.state()
		return slices.Equal(ws[len(ws)-1].OrderUUIDs, []string{"u-1", "u-2", "u-3"})
	})
	// And again on the interval while shown.
	_, _, before := link.state()
	time.Sleep(150 * time.Millisecond)
	_, _, after := link.state()
	if len(after)-len(before) < 2 {
		t.Errorf("in 150 ms at a 30 ms interval %d watches were resent, want several", len(after)-len(before))
	}

	// Nothing shown: nothing sent.
	e.SetRobotWatch(1, nil)
	e.SetRobotWatch(2, []string{})
	time.Sleep(60 * time.Millisecond)
	_, _, cleared := link.state()
	time.Sleep(150 * time.Millisecond)
	if _, _, later := link.state(); len(later) != len(cleared) {
		t.Errorf("with nothing shown %d more watches were sent, want none (Core's lease lapses)", len(later)-len(cleared))
	}

	// Core stops naming the topic: the link closes.
	e.OfferEphemeralTopic("")
	eventually(t, "the link closed", func() bool { _, stops, _ := link.state(); return stops == 1 })
	if e.RobotPositionsAvailable() {
		t.Error("the link is closed and RobotPositionsAvailable says open")
	}
	if l := logged(); len(l) != 0 {
		t.Errorf("the feed logged %q; with nothing wrong it says nothing", l)
	}
}

// TestRobotWatch_AnotherTopicThanConfiguredStaysClosed pins the Edge's own
// config: a topic Core names that this Edge is not configured for is never
// opened, said once however many acks repeat it; an Edge configured off says
// nothing at all.
func TestRobotWatch_AnotherTopicThanConfiguredStaysClosed(t *testing.T) {
	t.Parallel()
	e, link, logged := robotWatchEngine(t, "shingo.ephemeral", 30*time.Millisecond)
	e.SetRobotWatch(1, []string{"u-1"})
	for range 3 {
		e.OfferEphemeralTopic("plant.other")
		e.OfferEphemeralTopic("")
	}
	e.OfferEphemeralTopic("plant.other")
	time.Sleep(100 * time.Millisecond)
	if starts, _, watches := link.state(); len(starts) != 0 || len(watches) != 0 {
		t.Fatalf("a topic this Edge is not configured for: starts %v, %d watches; want none", starts, len(watches))
	}
	if l := logged(); len(l) != 1 || !strings.Contains(l[0], "plant.other") {
		t.Errorf("logged %q, want one line naming the mismatch", l)
	}

	off, offLink, offLogged := robotWatchEngine(t, "", 30*time.Millisecond)
	off.SetRobotWatch(1, []string{"u-1"})
	off.OfferEphemeralTopic("shingo.ephemeral")
	time.Sleep(100 * time.Millisecond)
	if starts, _, _ := offLink.state(); len(starts) != 0 || len(offLogged()) != 0 {
		t.Errorf("an Edge configured off opened %v and logged %q; want nothing", starts, offLogged())
	}
}

// TestHandleRobotPositions_LatestPerShownOrderInMemory pins the intake: only
// the orders shown are kept, the latest per order (an older poll does not
// overwrite a newer, nor reach the subscriber), the subscriber gets the
// message cut to them, nothing shown drops the message whole, and none of it
// touches SQLite.
func TestHandleRobotPositions_LatestPerShownOrderInMemory(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	e := &Engine{cfg: &config.Config{StationUID: "edge.a"}, db: db, logFn: func(string, ...any) {}}
	var got []protocol.RobotPositions
	e.SetRobotPositionsSubscriber(func(p protocol.RobotPositions) { got = append(got, p) }, nil)
	e.SetRobotWatch(1, []string{"u-1", "u-2"})

	t0 := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	msg := func(at time.Duration, x float64, uuids ...string) *protocol.RobotPositions {
		p := &protocol.RobotPositions{T: t0.Add(at), IntervalMS: 2000}
		for _, u := range uuids {
			p.Orders = append(p.Orders, protocol.RobotPosition{OrderUUID: u, RobotID: "AMR-01", X: x, Path: []string{"LM7", "LM31"}})
		}
		return p
	}
	counter.Reset()
	e.HandleRobotPositions(nil, msg(2*time.Second, 1, "u-1", "u-9"))
	e.HandleRobotPositions(nil, msg(0, 0, "u-1"))
	e.HandleRobotPositions(nil, msg(4*time.Second, 2, "u-2"))

	if h, ok := e.RobotPosition("u-1"); !ok || h.Position.X != 1 || !h.T.Equal(t0.Add(2*time.Second)) {
		t.Errorf("u-1 held %+v (%v), want the poll at +2 s (x 1): an older poll must not overwrite it", h, ok)
	}
	if _, ok := e.RobotPosition("u-9"); ok {
		t.Error("u-9 is not shown and a position for it was kept")
	}
	if len(got) != 3 || len(got[0].Orders) != 1 || got[0].Orders[0].OrderUUID != "u-1" {
		t.Errorf("subscriber got %+v, want three messages, the first cut to u-1", got)
	}
	if len(got) == 3 && len(got[1].Orders) != 0 {
		t.Errorf("the poll older than u-1's held one reached the subscriber with %+v, want no orders (a robot never steps back)", got[1].Orders)
	}

	e.SetRobotWatch(1, []string{"u-2"})
	if _, ok := e.RobotPosition("u-1"); ok {
		t.Error("u-1 is no longer shown and its position is still held")
	}
	e.SetRobotWatch(1, nil)
	e.HandleRobotPositions(nil, msg(6*time.Second, 3, "u-2"))
	if len(got) != 3 {
		t.Errorf("with nothing shown the subscriber got a message (%d in all), want it dropped", len(got))
	}
	if n := counter.Count(); n != 0 {
		t.Errorf("the intake ran %d SQLite statements, want none: positions live in memory only", n)
	}
}
