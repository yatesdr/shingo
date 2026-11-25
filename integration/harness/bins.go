package harness

import (
	"testing"
	"time"

	"shingo/protocol"

	coremessaging "shingocore/messaging"

	edgeharness "shingoedge/testharness"
)

// BinsFeed is Core's bins feed as a plant runs it, for a scenario whose Core
// subscribes the feed and routes what it carries (testharness.NewCore with
// the feed's hooks, bins.watch and edge.heartbeat routed): the Edge publishes
// its watch list through its own door, Core answers it with a snapshot
// (HandleBinWatch), Core pushes after the changes it is told of (BinsChanged,
// BinsOrderStarted, BinsOrderEnded), and the Edge's copy is confirmed by
// Core's ack to the Edge's heartbeat. Nothing is built, applied or acked
// here.
//
// NO WAIT AND NO CLOCK. Core arms each push on a 300 ms timer; at the end of
// every pump the Bus runs what is armed now instead (FlushPushes) and pumps
// what it sent, until nothing is armed. A heartbeat goes at the start, with
// the watch list (the boot burst's two), and again once a heartbeat interval
// of wall time has passed, as the heartbeater's loop sends one: a copy is
// decided from only while the last ack is younger than
// engine.ActableAckWithin.
//
// A write Core is not told of (a fixture's own write) reaches the Edge when
// the scenario asks (Bus.PushBins): the Edge sends its watch list again, as a
// reconnect's boot burst does, and Core answers it with a fresh snapshot.
type BinsFeed struct {
	t    *testing.T
	core *coremessaging.CoreDataService
	edge *edgeharness.Edge
	bus  *Bus
	rec  *Recorder
	beat time.Time
}

// heartbeatEvery is the heartbeater's interval (shingoedge messaging
// NewHeartbeater).
const heartbeatEvery = 60 * time.Second

// StartBinsFeed puts bus on Core's real bins feed: the Edge sends its watch
// list and a heartbeat, and the snapshot and the ack land. It fails the test
// when the Edge's copy is not then actable.
func StartBinsFeed(t *testing.T, bus *Bus, core *coremessaging.CoreDataService, edge *edgeharness.Edge) *BinsFeed {
	t.Helper()
	f := &BinsFeed{t: t, core: core, edge: edge, bus: bus, rec: RecorderFor(t)}
	bus.feed = f
	if err := edge.SendBinWatch(); err != nil {
		t.Fatalf("harness: the Edge's watch list: %v", err)
	}
	f.heartbeat()
	bus.PumpAll()
	if ok, why := edge.Engine.BinsActable(); !ok {
		t.Fatalf("harness: the Edge's bins copy is not actable after its boot (%s): Core must route %s and %s, "+
			"subscribe the feed (testharness.HookBinsFeed) and answer the asks (HookAskKinds)",
			why, protocol.SubjectBinsWatch, protocol.SubjectEdgeHeartbeat)
	}
	return f
}

func (f *BinsFeed) heartbeat() {
	f.t.Helper()
	if err := f.edge.Heartbeat(); err != nil {
		f.t.Fatalf("harness: the Edge's heartbeat: %v", err)
	}
	f.beat = time.Now()
}

// settle runs what Core has armed and pumps what it sent, until a flush runs
// nothing, a heartbeat first when one is due; pump false runs the pushes and
// leaves what they sent in Core's outbox (PumpEdgeOutbox). It returns the
// messages it delivered.
func (f *BinsFeed) settle(caller string, pump bool) int {
	n := 0
	if pump && time.Since(f.beat) >= heartbeatEvery {
		f.heartbeat()
		n += f.bus.pumpBoth()
	}
	for i := 0; i < 100; i++ {
		start := time.Now()
		ran := f.core.FlushPushes()
		f.rec.feedPushes("flush-"+caller, ran, time.Since(start))
		if ran == 0 || !pump {
			return n
		}
		n += f.bus.pumpBoth()
	}
	panic("harness: Core's bins pushes did not settle after 100 flushes (a push that arms another?)")
}

// republish is Bus.PushBins on the real feed: the watch list again, and its
// snapshot pumped.
func (f *BinsFeed) republish() {
	f.t.Helper()
	if err := f.edge.SendBinWatch(); err != nil {
		f.t.Fatalf("harness: the Edge's watch list: %v", err)
	}
	f.bus.PumpAll()
}
