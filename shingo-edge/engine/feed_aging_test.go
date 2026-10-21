package engine

import (
	"testing"
	"time"

	"shingo/protocol"
)

// feed_aging_test.go — a held feed's "as of" reaches an idle board.
//
// A station view says "Bins as of <time>" (and "Catalog as of") once Core has
// not confirmed the copy for FeedAsOfAfter, and the containment page says its
// "as of" the same way. Nothing is written when that threshold passes: it is
// time, not data. The board no longer polls, so the Edge's heartbeat tick (the
// one clock it already has: FeedDigests) and every confirmation or arrival
// compare each copy's age against the last state they saw, in memory, and
// announce a crossing either way, once.

// agingEvents counts board nudges and containment announcements.
func agingEvents(e *Engine) (nudges, containment *int) {
	n, c := 0, 0
	e.Events.Subscribe(func(evt Event) {
		switch evt.Type {
		case EventBoardNudge:
			n++
		case EventContainmentUpdated:
			c++
		}
	})
	return &n, &c
}

func TestFeedAging_AStaleCrossingNudgesOnceEachWay(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	base := time.Now()
	clock := base
	e.feeds.now = func() time.Time { return clock }
	e.feeds.reconcile = func() error { return nil } // the long silence below is an ack gap
	e.holdFeed(protocol.FeedBins, "bins-1", "")
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: "bins-1"}})
	nudges, _ := agingEvents(e)

	steps := []struct {
		at   time.Duration
		ack  bool
		want int
		say  string
	}{
		{60 * time.Second, false, 0, "a tick while the copy is current"},
		{120 * time.Second, false, 0, "another, still inside FeedAsOfAfter"},
		{180 * time.Second, false, 1, "the tick after Core stopped confirming for longer than FeedAsOfAfter"},
		{240 * time.Second, false, 1, "a tick while it stays stale"},
		{600 * time.Second, false, 1, "and another"},
		{610 * time.Second, true, 2, "Core confirms it again"},
		{660 * time.Second, false, 2, "a tick after, current"},
	}
	for _, s := range steps {
		clock = base.Add(s.at)
		if s.ack {
			e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: "bins-1"}})
		} else {
			e.FeedDigests()
		}
		if *nudges != s.want {
			t.Fatalf("at +%v (%s): %d board nudges, want %d", s.at, s.say, *nudges, s.want)
		}
	}
}

// The containment page reloads on its own event, not on the board's nudge, so
// its copy's crossing is announced on that.
func TestFeedAging_ContainmentCrossingIsAnnouncedToItsPage(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	base := time.Now()
	clock := base
	e.feeds.now = func() time.Time { return clock }
	e.feeds.reconcile = func() error { return nil }
	e.holdFeed(protocol.FeedContainment, "cont-1", "")
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedContainment: "cont-1"}})
	_, containment := agingEvents(e)

	clock = base.Add(100 * time.Second)
	e.FeedDigests()
	if *containment != 0 {
		t.Fatalf("a current containment copy announced %d times", *containment)
	}
	clock = base.Add(200 * time.Second)
	e.FeedDigests()
	e.FeedDigests()
	if *containment != 1 {
		t.Fatalf("containment crossing into stale: %d announcements, want 1", *containment)
	}
	clock = base.Add(260 * time.Second)
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedContainment: "cont-1"}})
	if *containment != 2 {
		t.Fatalf("containment confirmed again: %d announcements, want 2", *containment)
	}
}

// Nothing held is nothing to age: an Edge that never received a feed says so
// by another sign ("not known yet"), and the tick announces nothing for it.
func TestFeedAging_NothingHeldIsNeverACrossing(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	clock := time.Now()
	e.feeds.now = func() time.Time { return clock }
	nudges, containment := agingEvents(e)
	for i := 0; i < 5; i++ {
		clock = clock.Add(time.Minute)
		e.FeedDigests()
	}
	if *nudges != 0 || *containment != 0 {
		t.Fatalf("with nothing held: %d nudges, %d containment announcements; want none", *nudges, *containment)
	}
}

// TestFeedAging_CrossingsTakeTheLaterTime pins the feed book's own as-of rule
// (crossingsLocked), the fifth copy beside the four the www package pins in
// TestAsOfCopies_Characterised: a copy is stale when the later of its arrival
// and Core's last confirmation is older than FeedAsOfAfter.
func TestFeedAging_CrossingsTakeTheLaterTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := now.Add(-FeedAsOfAfter - time.Second)
	fresh := now.Add(-FeedAsOfAfter + time.Second)
	cases := []struct {
		name                string
		received, confirmed time.Time
		stale               bool
	}{
		{"confirmed recently over an old arrival", old, fresh, false},
		{"arrived recently, never confirmed", fresh, time.Time{}, false},
		{"arrived long ago, never confirmed", old, time.Time{}, true},
		{"old arrival, old confirmation", old, old, true},
		{"arrived recently over an old confirmation", fresh, old, false},
	}
	for _, c := range cases {
		b := &feedBook{held: map[string]*feedHold{
			protocol.FeedBins: {digest: "d", receivedAt: c.received, confirmedAt: c.confirmed},
		}}
		b.crossingsLocked(now)
		if got := b.stale[protocol.FeedBins]; got != c.stale {
			t.Errorf("%s: stale %v, want %v", c.name, got, c.stale)
		}
	}
}
