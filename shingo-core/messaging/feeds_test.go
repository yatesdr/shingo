package messaging

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
)

// The feed send rule, without a database: a differing digest sends, the same
// digest to the same station inside the guard does not, three sends in a row
// flag the key, and a heartbeat quoting the digest back ends the streak and
// clears the flag.
func TestFeedState_CompareGuardAndFlag(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	const st, key = "edge.test", protocol.FeedCatalog

	steps := []struct {
		name      string
		at        time.Duration
		current   string
		held      string
		wantSend  bool
		wantFlags []string
	}{
		{"differs: send", 0, "d1", "", true, []string{}},
		{"same digest inside the guard: held back", time.Minute, "d1", "", false, []string{}},
		{"guard elapsed: second send", 2 * time.Minute, "d1", "", true, []string{}},
		{"third send in a row flags", 4 * time.Minute, "d1", "", true, []string{"catalog not converging: sent 3 times"}},
		{"a new digest is not held back by the guard", 4*time.Minute + time.Second, "d2", "", true,
			[]string{"catalog not converging: sent 3 times"}},
		{"quoted back: no send, flag cleared", 5 * time.Minute, "d2", "d2", false, []string{}},
		{"differs again: streak starts over", 6 * time.Minute, "d3", "d2", true, []string{}},
	}
	for _, s := range steps {
		if got := f.compare(st, key, s.current, s.held, t0.Add(s.at)); got != s.wantSend {
			t.Errorf("%s: send = %v, want %v", s.name, got, s.wantSend)
		}
		if got := f.flagsFor(st); !reflect.DeepEqual(got, s.wantFlags) {
			t.Errorf("%s: flags = %v, want %v", s.name, got, s.wantFlags)
		}
	}
	if got := f.flagsFor("edge.other"); len(got) != 0 {
		t.Errorf("another station's flags = %v, want none", got)
	}
}

// A value that moves on every heartbeat, sent each time to an Edge that applied
// the last send, is converging: no streak, no flag. Found by the versioned-feeds
// proof (containment: destination added, bin moved in, bin moved out, on three
// heartbeats in a row, flagged both stations). An Edge that keeps quoting an
// older digest is still flagged on the third send.
func TestFeedState_StreakEndsWhenTheLastSendIsHeld(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	t0 := time.Date(2026, 10, 10, 7, 50, 0, 0, time.UTC)
	const st, key = "edge.test", protocol.FeedContainment
	held := "d0"
	for i, cur := range []string{"d1", "d2", "d3", "d4", "d5"} {
		if !f.compare(st, key, cur, held, t0.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("heartbeat %d: %s not sent to an Edge holding %s", i, cur, held)
		}
		held = cur // the Edge applied it before its next heartbeat
	}
	if got := f.flagsFor(st); len(got) != 0 {
		t.Errorf("flags = %v after five sends each applied, want none", got)
	}

	g := newFeedState()
	for i, cur := range []string{"e1", "e2", "e3"} {
		g.compare(st, key, cur, "e0", t0.Add(time.Duration(i)*time.Minute))
	}
	if got := g.flagsFor(st); !reflect.DeepEqual(got, []string{"containment not converging: sent 3 times"}) {
		t.Errorf("an Edge stuck on e0: flags = %v, want the not-converging flag", got)
	}
}

// An Edge that held a feed and restarts quotes "" on its first heartbeat:
// Core sends the same digest at once, inside the guard, instead of two minutes
// later. A heartbeat that predates that copy's arrival is held back. An Edge
// that never quoted the first send is waiting on it: held, as ever.
func TestFeedState_ARestartedEdgeGetsTheFeedOnItsFirstHeartbeat(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	const st, key = "edge.test", protocol.FeedCatalog

	f := newFeedState()
	if !f.compare(st, key, "d1", "", t0) {
		t.Fatal("differs: want a send")
	}
	if f.compare(st, key, "d1", "d1", t0.Add(20*time.Second)) {
		t.Fatal("quoted back: want no send")
	}
	if !f.compare(st, key, "d1", "", t0.Add(40*time.Second)) {
		t.Error("restarted Edge quotes \"\" inside the guard: want the feed at once")
	}
	if f.compare(st, key, "d1", "", t0.Add(50*time.Second)) {
		t.Error("that copy in flight: want the next heartbeat held back")
	}
	if got := f.flagsFor(st); len(got) != 0 {
		t.Errorf("flags = %v, want none", got)
	}

	g := newFeedState()
	g.compare(st, key, "d1", "", t0)
	if g.compare(st, key, "d1", "", t0.Add(40*time.Second)) {
		t.Error("never quoted back: want the guard to hold the same digest")
	}
}

// The guard is per station: the same digest going to a second station is not
// held back by the first station's send.
func TestFeedState_GuardIsPerStation(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if !f.compare("edge.a", protocol.FeedCatalog, "d1", "", now) {
		t.Fatal("first station: want a send")
	}
	if !f.compare("edge.b", protocol.FeedCatalog, "d1", "", now) {
		t.Error("second station: want a send, the guard is per station")
	}
}

// Plant-wide feeds share one memo across stations for ten seconds; the node
// list and scene are memoised per station for two minutes; drop forgets a
// plant-wide memo at once.
func TestFeedState_Memo(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f.store("edge.a", protocol.FeedCatalog, feedMemoEntry{digest: "c1", at: t0})
	f.store("edge.a", protocol.FeedNodes, feedMemoEntry{digest: "n1", at: t0})

	cases := []struct {
		name    string
		station string
		key     string
		at      time.Duration
		want    bool
	}{
		{"catalog, other station, inside 10 s", "edge.b", protocol.FeedCatalog, 9 * time.Second, true},
		{"catalog at 10 s", "edge.a", protocol.FeedCatalog, 10 * time.Second, false},
		{"nodes, same station, inside 2 min", "edge.a", protocol.FeedNodes, 119 * time.Second, true},
		{"nodes, other station", "edge.b", protocol.FeedNodes, time.Second, false},
		{"nodes at 2 min", "edge.a", protocol.FeedNodes, 2 * time.Minute, false},
	}
	for _, c := range cases {
		if _, got := f.lookup(c.station, c.key, t0.Add(c.at)); got != c.want {
			t.Errorf("%s: hit = %v, want %v", c.name, got, c.want)
		}
	}
	f.drop(protocol.FeedCatalog)
	if _, ok := f.lookup("edge.a", protocol.FeedCatalog, t0); ok {
		t.Error("catalog memo survived drop")
	}
}

// A key this Core does not serve is left out of the answer and nothing is sent
// for it — what a newer Edge's unknown key meets on an older Core.
func TestAnswerFeeds_UnknownKeyIsAbsent(t *testing.T) {
	t.Parallel()
	resp := &captureFeedsResponder{}
	s := &CoreDataService{resp: resp, feeds: newFeedState()}
	got := s.answerFeeds("edge.test", map[string]string{"no-such-feed": ""})
	if len(got) != 0 {
		t.Errorf("answer = %v, want {}", got)
	}
	if len(resp.sent) != 0 {
		t.Errorf("sent %v, want nothing", resp.sent)
	}
}

// captureFeedsResponder records sends for the feed tests that need no database.
type captureFeedsResponder struct {
	sent []string
}

func (r *captureFeedsResponder) dbg(string, ...any) {}
func (r *captureFeedsResponder) replyData(_ *protocol.Envelope, subject string, _ any) {
	r.sent = append(r.sent, "reply "+subject)
}
func (r *captureFeedsResponder) sendData(subject, station string, _ any) {
	r.sent = append(r.sent, "send "+subject+" -> "+station)
}

// A push (a send outside the heartbeat compare) leaves the resend guard alone:
// the heartbeat after a lost push sends at once. The guard still holds back a
// digest the compare itself sent inside two minutes. lastSent is whichever of
// the two went last.
func TestFeedState_PushLeavesTheGuardAlone(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	const st, key = "edge.test", protocol.FeedBins

	if !f.compare(st, key, "d1", "", t0) {
		t.Fatal("first heartbeat: want a send")
	}
	f.recordPush(st, key, "d2", t0.Add(10*time.Second))
	if got := f.lastSent(st, key); got != "d2" {
		t.Errorf("lastSent after the push = %q, want d2", got)
	}
	if !f.compare(st, key, "d2", "d1", t0.Add(time.Minute)) {
		t.Error("heartbeat after a lost push: want the push's digest sent at once")
	}
	if got := f.lastSent(st, key); got != "d2" {
		t.Errorf("lastSent after the heartbeat's send = %q, want d2", got)
	}
	if f.compare(st, key, "d2", "d1", t0.Add(2*time.Minute)) {
		t.Error("the compare's own send inside the guard: want it held back")
	}
	if !f.compare(st, key, "d2", "d1", t0.Add(3*time.Minute+time.Second)) {
		t.Error("the guard elapsed: want a send")
	}
}

// An Edge that quotes what a push sent it has converged: the streak ends, so a
// bins feed pushed between heartbeats is not flagged.
func TestFeedState_StreakEndsWhenThePushIsHeld(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	const st, key = "edge.test", protocol.FeedBins
	held := "d0"
	for i := 0; i < 3; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		cur := fmt.Sprintf("h%d", i)
		if !f.compare(st, key, cur, held, at) {
			t.Fatalf("heartbeat %d: %s not sent", i, cur)
		}
		pushed := fmt.Sprintf("p%d", i)
		f.recordPush(st, key, pushed, at.Add(30*time.Second))
		held = pushed // the Edge applied the push before its next heartbeat
	}
	if got := f.flagsFor(st); len(got) != 0 {
		t.Errorf("flags = %v for an Edge that held every push, want none", got)
	}
}

// A bins key is answered only for an Edge whose watch list Core holds.
func TestAnswerFeeds_BinsWithNoListIsUnknown(t *testing.T) {
	t.Parallel()
	resp := &captureFeedsResponder{}
	s := &CoreDataService{resp: resp, feeds: newFeedState(), bins: newBinsFeed()}
	got := s.answerFeeds("edge.test", map[string]string{protocol.FeedBins: ""})
	if len(got) != 0 || len(resp.sent) != 0 {
		t.Errorf("answer = %v, sent %v; want neither", got, resp.sent)
	}
}

// TestFeedMemo_ScopeAndTTLCharacterised pins each feed's memo scope and
// lifetime, read from its feedScopes row. The last row is the hazard the table
// removed: a key with no row was memoised plant-wide for feedMemoPlant, so a
// per-station feed left out would have answered station B with station A's
// build. It now has no lifetime (never memoised), and feedCurrent does not
// serve it (TestAnswerFeeds_NoScopeRowIsAbsent).
func TestFeedMemo_ScopeAndTTLCharacterised(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key        string
		perStation bool
		ttl        time.Duration
	}{
		{protocol.FeedContainment, false, feedMemoPlant},
		{protocol.FeedRefusals, false, feedMemoPlant},
		{protocol.FeedNodes, true, feedMemoStation},
		{protocol.FeedScene, true, feedMemoStation},
		{protocol.FeedCatalog, false, feedMemoPlant},
		{protocol.FeedBins, true, feedMemoBins},
		{"not-a-feed", false, 0},
	}
	for _, c := range cases {
		if got := memoKey("edge.a", c.key) != memoKey("edge.b", c.key); got != c.perStation {
			t.Errorf("%s: per-station %v, want %v", c.key, got, c.perStation)
		}
		if got := memoTTL(c.key); got != c.ttl {
			t.Errorf("%s: ttl %v, want %v", c.key, got, c.ttl)
		}
	}
}

// TestAnswerFeeds_NoScopeRowIsAbsent: a key with no feedScopes row is not
// served and not memoised, the unknown key's answer
// (TestAnswerFeeds_UnknownKeyIsAbsent). feedCurrent refuses it before the
// build, so a build arm whose row was forgotten is not served either; for the
// keys protocol lists, TestFeedScopes_CoverAllFeeds keeps that case unreachable.
func TestAnswerFeeds_NoScopeRowIsAbsent(t *testing.T) {
	t.Parallel()
	resp := &captureFeedsResponder{}
	s := &CoreDataService{resp: resp, feeds: newFeedState()}
	if _, ok := s.feedCurrent("edge.test", "no-such-feed", time.Now()); ok {
		t.Error("feedCurrent served a key with no scope row")
	}
	if len(s.feeds.memo) != 0 || len(resp.sent) != 0 {
		t.Errorf("memo %v, sent %v; want neither", s.feeds.memo, resp.sent)
	}
}

// coreFeedAsks is the Core-inbound subject the Edge asks for or publishes each
// feed on, where one exists: the boot burst's node-list and catalog requests,
// the Edge's own refusals (a table both sides write), the bins watch list.
// Containment has none: the Edge only receives it.
var coreFeedAsks = map[string]string{
	protocol.FeedRefusals: protocol.SubjectSupplyRefusal,
	protocol.FeedNodes:    protocol.SubjectNodeListRequest,
	protocol.FeedScene:    protocol.SubjectNodeListRequest,
	protocol.FeedCatalog:  protocol.SubjectCatalogPayloadsRequest,
	protocol.FeedBins:     protocol.SubjectBinsWatch,
}

// TestFeedScopes_CoverAllFeeds: every feed key has a scope row and no row
// names a key protocol does not list, and each subject the Edge asks or
// publishes a feed on is one Core's boot coverage registers
// (protocol.CoreInboundSubjects).
func TestFeedScopes_CoverAllFeeds(t *testing.T) {
	t.Parallel()
	inbound := map[string]bool{}
	for _, s := range protocol.CoreInboundSubjects() {
		inbound[s] = true
	}
	listed := map[string]bool{}
	for _, key := range protocol.AllFeeds() {
		listed[key] = true
		if sc, ok := feedScopes[key]; !ok || (sc.ttl <= 0) == !sc.noMemo {
			t.Errorf("%s: scope row %+v (present %v), want a row with a lifetime or noMemo, not both", key, sc, ok)
		}
		if ask, ok := coreFeedAsks[key]; ok && !inbound[ask] {
			t.Errorf("%s: ask subject %s is not in CoreInboundSubjects", key, ask)
		}
	}
	for key := range feedScopes {
		if !listed[key] {
			t.Errorf("scope row %s is not in protocol.AllFeeds", key)
		}
	}
	for key := range coreFeedAsks {
		if !listed[key] {
			t.Errorf("ask row %s is not in protocol.AllFeeds", key)
		}
	}
}

// TestAsksFor_TheAckAdvertisesTheKinds: an Edge that speaks feeds is told the
// ask kinds this Core answers; an older Edge (no Feeds) and a Core with none
// registered advertise nothing, which an Edge reads as "use the HTTP doors".
func TestAsksFor_TheAckAdvertisesTheKinds(t *testing.T) {
	t.Parallel()
	s := &CoreDataService{}
	feeds := &protocol.EdgeHeartbeat{Feeds: map[string]string{}}
	if got := s.asksFor(feeds); got != nil {
		t.Errorf("no kinds injected: asks = %v, want nil", got)
	}
	s.SetAskKinds(func() []string { return nil })
	if got := s.asksFor(feeds); got != nil {
		t.Errorf("none registered: asks = %v, want nil", got)
	}
	s.SetAskKinds(func() []string { return []string{protocol.AskBinClear, protocol.AskBinCount} })
	if got := s.asksFor(feeds); !reflect.DeepEqual(got, []string{protocol.AskBinClear, protocol.AskBinCount}) {
		t.Errorf("asks = %v", got)
	}
	if got := s.asksFor(&protocol.EdgeHeartbeat{}); got != nil {
		t.Errorf("an Edge without feeds: asks = %v, want nil", got)
	}
}
