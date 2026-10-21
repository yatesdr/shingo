package messaging

import (
	"os"
	"regexp"
	"testing"

	"shingo/protocol"
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
	if b := h.heartbeatBody(); b.TickPending != nil || b.TickOldestUnsentAgeMS != nil {
		t.Errorf("unwired: lag fields = %v/%v, want absent", b.TickPending, b.TickOldestUnsentAgeMS)
	}
	h.TickLagFn = func() (int64, int64, bool) { return 3, 4200, true }
	b := h.heartbeatBody()
	if b.TickPending == nil || *b.TickPending != 3 || b.TickOldestUnsentAgeMS == nil || *b.TickOldestUnsentAgeMS != 4200 {
		t.Errorf("lag fields = %v/%v, want 3/4200", b.TickPending, b.TickOldestUnsentAgeMS)
	}
	h.TickLagFn = func() (int64, int64, bool) { return 0, 0, false }
	if b := h.heartbeatBody(); b.TickPending != nil {
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
	if b := h.heartbeatBody(); b.Feeds != nil {
		t.Errorf("unwired: feeds = %v, want nil (an older Edge's heartbeat)", b.Feeds)
	}
	if h.coreSpeaksFeeds() {
		t.Error("unwired: re-ask skipped, want it kept")
	}

	h.FeedsFn = func() map[string]string { return map[string]string{"catalog": "c1"} }
	if b := h.heartbeatBody(); b.Feeds["catalog"] != "c1" {
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
