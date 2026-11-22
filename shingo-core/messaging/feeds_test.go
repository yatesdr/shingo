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
	f.store("edge.a", protocol.FeedCatalog, feedMemoEntry{digest: "c1", at: t0}, f.dropCount())
	f.store("edge.a", protocol.FeedNodes, feedMemoEntry{digest: "n1", at: t0}, f.dropCount())

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
// lifetime, read from its coreFeeds row: one station's slot apart from
// another's for a per-station feed, one slot for the plant otherwise. Sourcing
// is never memoised. The last row is the hazard the table removed: a key with
// no row was memoised plant-wide for feedMemoPlant, so a per-station feed left
// out would have answered station B with station A's build. It now has no
// lifetime (never memoised), and feedCurrent does not serve it
// (TestAnswerFeeds_NoScopeRowIsAbsent).
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
		{protocol.FeedSourcing, true, 0},
		{"not-a-feed", false, 0},
	}
	for _, c := range cases {
		a, row := slotOf("edge.a", c.key)
		b, _ := slotOf("edge.b", c.key)
		if got := a != b; got != c.perStation {
			t.Errorf("%s: per-station %v, want %v", c.key, got, c.perStation)
		}
		if row.ttl != c.ttl {
			t.Errorf("%s: ttl %v, want %v", c.key, row.ttl, c.ttl)
		}
	}
}

// TestAnswerFeeds_NoScopeRowIsAbsent: a key with no coreFeeds row is not
// served and not memoised, the unknown key's answer
// (TestAnswerFeeds_UnknownKeyIsAbsent). feedCurrent refuses it before any
// build; for the keys protocol lists, TestCoreFeeds_EveryFeedHasARow keeps
// that case unreachable.
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

// TestCoreFeeds_EveryFeedHasARow: every feed key has one coreFeeds row, in
// protocol.AllFeeds' order, and no row names a key protocol does not list.
// Every row has a build, a carrier and a send: a row with no send would leave
// a due feed unsent with nothing to say so. Each subject the Edge asks or
// publishes a feed on is one Core's boot coverage registers
// (protocol.CoreInboundSubjects).
func TestCoreFeeds_EveryFeedHasARow(t *testing.T) {
	t.Parallel()
	inbound := map[string]bool{}
	for _, s := range protocol.CoreInboundSubjects() {
		inbound[s] = true
	}
	var rows []string
	for _, f := range coreFeeds {
		rows = append(rows, f.key)
		if f.build == nil || f.send == nil || f.carrier == "" {
			t.Errorf("%s: build set %v, send set %v, carrier %q; want all three", f.key, f.build != nil, f.send != nil, f.carrier)
		}
		if f.ttl < 0 {
			t.Errorf("%s: ttl %v; want a lifetime, or zero for never memoised", f.key, f.ttl)
		}
	}
	if !reflect.DeepEqual(rows, protocol.AllFeeds()) {
		t.Errorf("coreFeeds rows %v, want one per protocol.AllFeeds key in its order %v", rows, protocol.AllFeeds())
	}
	listed := map[string]bool{}
	for _, key := range protocol.AllFeeds() {
		listed[key] = true
		if ask, ok := coreFeedAsks[key]; ok && !inbound[ask] {
			t.Errorf("%s: ask subject %s is not in CoreInboundSubjects", key, ask)
		}
	}
	for key := range coreFeedAsks {
		if !listed[key] {
			t.Errorf("ask row %s is not in protocol.AllFeeds", key)
		}
	}
}

// TestCoreOwned_EveryFamilyHasARow: every Edge-owned family has one coreOwned
// row, in protocol.AllOwned's order, each with a read. A family with no row is
// left off every ack in silence, and the Edge then sends nothing for it.
func TestCoreOwned_EveryFamilyHasARow(t *testing.T) {
	t.Parallel()
	var rows []string
	for _, f := range coreOwned {
		rows = append(rows, f.key)
		if f.held == nil {
			t.Errorf("%s: no read of what Core holds", f.key)
		}
	}
	if !reflect.DeepEqual(rows, protocol.AllOwned()) {
		t.Errorf("coreOwned rows %v, want one per protocol.AllOwned key in its order %v", rows, protocol.AllOwned())
	}
}

// TestCoreFeeds_OneSendPerCarrier: two feeds due on one carrier go in one
// send (the node list carries the nodes and the scene), each other feed in its
// own, in key order.
func TestCoreFeeds_OneSendPerCarrier(t *testing.T) {
	t.Parallel()
	var calls []string
	resp := &captureFeedsResponder{}
	s := &CoreDataService{resp: resp, feeds: newFeedState()}
	due := map[string]feedMemoEntry{}
	for _, key := range []string{protocol.FeedNodes, protocol.FeedScene, protocol.FeedCatalog, protocol.FeedContainment} {
		due[key] = feedMemoEntry{digest: key + "-d", value: key + "-v"}
	}
	current := func(key string) (feedMemoEntry, bool) {
		calls = append(calls, key)
		return feedMemoEntry{}, false // the node list cannot be read: its send sends nothing
	}
	s.sendFeeds("edge.test", due, current, map[string]string{})
	want := []string{
		"send " + protocol.SubjectCatalogPayloadsResponse + " -> edge.test",
		"send " + protocol.SubjectContainmentSnapshot + " -> edge.test",
	}
	if !reflect.DeepEqual(resp.sent, want) {
		t.Errorf("sent %v, want %v", resp.sent, want)
	}
	if !reflect.DeepEqual(calls, []string{protocol.FeedNodes}) {
		t.Errorf("the node list's send read %v, want the node list once (the scene's row rode the same send)", calls)
	}
}

// TestAsksFor_TheAckAdvertisesTheKinds: an Edge that speaks feeds is told the
// ask kinds this Core answers; an older Edge (no Feeds) and a Core with none
// registered advertise nothing, which an Edge reads as "use the HTTP doors".
func TestAsksFor_TheAckAdvertisesTheKinds(t *testing.T) {
	t.Parallel()
	s := &CoreDataService{}
	if got := s.asksFor(); got != nil {
		t.Errorf("no kinds injected: asks = %v, want nil", got)
	}
	s.SetAskKinds(func() []string { return nil })
	if got := s.asksFor(); got != nil {
		t.Errorf("none registered: asks = %v, want nil", got)
	}
	s.SetAskKinds(func() []string { return []string{protocol.AskBinClear, protocol.AskBinCount} })
	if got := s.asksFor(); !reflect.DeepEqual(got, []string{protocol.AskBinClear, protocol.AskBinCount}) {
		t.Errorf("asks = %v", got)
	}
}

// sentKeptPin is how many sends a record keeps since the last proved one
// (protocol/resend). The positions below are read from it.
const sentKeptPin = 256

// pushN records n pushes, p1..pn, a second apart.
func pushN(f *feedState, st, key string, n int, from time.Time) {
	for i := 1; i <= n; i++ {
		f.recordPush(st, key, fmt.Sprintf("p%d", i), from.Add(time.Duration(i)*time.Second))
	}
}

// A quote is a statement about the moment the heartbeat left: a copy Core sent
// since the last one the Edge proved ends the streak, however many newer copies
// were on their way when it was compared, up to the newest sentKeptPin of them.
func TestFeedState_AQuoteOfACopySentSinceTheLastProvedConverges(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		pushes, quoted int
		kept           bool
	}{
		{1, 1, true}, {2, 1, true}, {4, 2, true}, {22, 1, true}, {85, 64, true},
		{sentKeptPin, 1, true}, {sentKeptPin, sentKeptPin, true},
		{sentKeptPin + 1, 1, false}, {sentKeptPin + 1, 2, true},
		{300, 300 - sentKeptPin, false}, {300, 300 - sentKeptPin + 1, true},
	}
	for _, c := range cases {
		f := newFeedState()
		f.compare(st, key, "c1", "", t0)
		f.compare(st, key, "c2", "", t0.Add(time.Minute)) // two sends in a row
		pushN(f, st, key, c.pushes, t0.Add(time.Minute))
		if !f.compare(st, key, "c3", fmt.Sprintf("p%d", c.quoted), t0.Add(8*time.Minute)) {
			t.Fatalf("%d pushes, quote %d: the newer value was not sent", c.pushes, c.quoted)
		}
		if got := len(f.flagsFor(st)) == 0; got != c.kept {
			t.Errorf("%d pushes, then a quote of push %d: converging %v, want %v", c.pushes, c.quoted, got, c.kept)
		}
	}
}

// An Edge that stopped applying quotes the same digest every heartbeat. Wherever
// that digest stands among the sends (first, middle, newest, at the bound, past
// it), never sent, or empty, it is believed once at most: the third send flags.
func TestFeedState_AFrozenQuoteStillFlags(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	want := []string{"bins not converging: sent 3 times"}
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 2, 22, sentKeptPin, sentKeptPin + 1, 300} {
		quotes := []string{"", "never-sent"}
		at := map[int]bool{}
		for _, p := range []int{1, n / 2, n - sentKeptPin, n - sentKeptPin + 1, n - 1, n} {
			if p >= 1 && !at[p] {
				at[p] = true
				quotes = append(quotes, fmt.Sprintf("p%d", p))
			}
		}
		for _, frozen := range quotes {
			for _, pushing := range []bool{false, true} {
				f := newFeedState()
				pushN(f, st, key, n, t0)
				for i := 0; i < 3; i++ {
					at := t0.Add(time.Duration(10+i) * time.Minute)
					f.compare(st, key, fmt.Sprintf("c%d", i), frozen, at)
					var due []string
					if i == 2 {
						due = want
					}
					if got := f.flagsFor(st); len(got) != len(due) || (len(due) > 0 && !reflect.DeepEqual(got, due)) {
						t.Errorf("%d pushes, frozen at %q, still pushing %v, heartbeat %d: flags = %v, want %v",
							n, frozen, pushing, i+1, got, due)
					}
					if pushing {
						f.recordPush(st, key, fmt.Sprintf("q%d", i), at.Add(time.Second))
					}
				}
			}
		}
	}
}

// Proving a copy discards what came before it: a quote of an older one after
// that is an Edge that went backwards, and counts.
func TestFeedState_AQuoteOfADiscardedCopyCounts(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for _, older := range []string{"p1", "p2", "p3"} {
		f := newFeedState()
		pushN(f, st, key, 5, t0)
		f.compare(st, key, "c0", "p3", t0.Add(time.Minute)) // proves p3
		f.compare(st, key, "c1", older, t0.Add(2*time.Minute))
		f.compare(st, key, "c2", older, t0.Add(3*time.Minute))
		if got := f.flagsFor(st); len(got) != 1 {
			t.Errorf("quote went back to %s after p3 was proved: flags = %v, want the flag", older, got)
		}
	}
}

// A value that goes A, B, A repeats a digest. An Edge applying every copy quotes
// the same A on each heartbeat while B is in flight and is converging; one
// frozen at A is believed once, for the newest A sent, not once for each.
func TestFeedState_AValueThatGoesBackAndForth(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedSourcing
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	f := newFeedState()
	f.compare(st, key, "A", "", t0)
	for i := 1; i <= 5; i++ {
		at := t0.Add(time.Duration(i) * 3 * time.Minute)
		f.recordPush(st, key, "A", at.Add(-2*time.Second))
		f.recordPush(st, key, "B", at.Add(-time.Second))
		f.compare(st, key, "B", "A", at) // the heartbeat left between the two
		if got := f.flagsFor(st); len(got) != 0 {
			t.Errorf("heartbeat %d of an Edge applying every copy: flags = %v, want none", i, got)
		}
	}

	for _, rounds := range []int{1, 3, sentKeptPin} {
		g := newFeedState()
		for i := 0; i < rounds; i++ {
			g.recordPush(st, key, "A", t0)
			g.recordPush(st, key, "B", t0)
		}
		g.recordPush(st, key, "A", t0)
		for i, cur := range []string{"C", "D", "E"} {
			g.compare(st, key, cur, "A", t0.Add(time.Duration(i+1)*time.Minute))
		}
		if got := g.flagsFor(st); !reflect.DeepEqual(got, []string{"sourcing not converging: sent 3 times"}) {
			t.Errorf("frozen at A after %d rounds of A, B: flags = %v, want the flag on the third send", rounds, got)
		}
	}
}

// An empty quote is an Edge holding nothing: it is never one of the sends.
func TestFeedState_AnEmptyQuoteIsNeverASend(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	f := newFeedState()
	for i := 0; i < 3; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		f.compare(st, key, fmt.Sprintf("c%d", i), "", at)
		f.recordPush(st, key, fmt.Sprintf("p%d", i), at.Add(time.Second))
	}
	if got := f.flagsFor(st); len(got) != 1 {
		t.Errorf("three sends to an Edge quoting nothing: flags = %v, want the flag", got)
	}
}

// A restarted Edge quotes nothing and is sent at once, inside the guard, whether
// the copy it held came by a compare or a push; that send is then in flight.
func TestFeedState_ARestartAfterPushesIsHealedAtOnce(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 22, sentKeptPin, 300} {
		f := newFeedState()
		f.compare(st, key, "d1", "", t0)
		pushN(f, st, key, n, t0)
		newest := fmt.Sprintf("p%d", n)
		if f.compare(st, key, newest, newest, t0.Add(20*time.Second)) {
			t.Fatalf("%d pushes: sent to an Edge holding the newest", n)
		}
		if !f.compare(st, key, "d1", "", t0.Add(40*time.Second)) {
			t.Errorf("%d pushes: restarted Edge quotes nothing inside the guard: want the copy at once", n)
		}
		if f.compare(st, key, "d1", "", t0.Add(50*time.Second)) {
			t.Errorf("%d pushes: that copy is in flight: want it held back", n)
		}
		if got := f.flagsFor(st); len(got) != 0 {
			t.Errorf("%d pushes: flags = %v after a restart, want none", n, got)
		}
	}
}

// Only a quote of the last send says nothing is in flight. An Edge quoting a
// push Core sent before the compare's copy, or before a newer push, still has
// a copy on the way: the guard holds the compare's repeat.
func TestFeedState_ACopyInFlightIsNotSentTwice(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

	f := newFeedState()
	f.recordPush(st, key, "p1", t0)
	if !f.compare(st, key, "c1", "p0", t0.Add(time.Second)) {
		t.Fatal("differs: want a send")
	}
	if f.compare(st, key, "c1", "p1", t0.Add(20*time.Second)) {
		t.Error("quote of the push before c1, c1 in flight: want it held back")
	}

	g := newFeedState()
	if !g.compare(st, key, "c1", "p0", t0) {
		t.Fatal("differs: want a send")
	}
	pushN(g, st, key, 2, t0)
	if g.compare(st, key, "c1", "p1", t0.Add(20*time.Second)) {
		t.Error("quote of a push with a newer one in flight: want the guard to hold")
	}
}

// What was last sent is still known after the Edge proved it: a push of the
// same digest is not made, and a door compares its push with it.
func TestFeedState_TheLastSendOutlivesItsProof(t *testing.T) {
	t.Parallel()
	const st, key = "edge.test", protocol.FeedBins
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 22, 300} {
		f := newFeedState()
		pushN(f, st, key, n, t0)
		newest, at := fmt.Sprintf("p%d", n), t0.Add(10*time.Minute)
		f.compare(st, key, newest, newest, at)
		if got := f.lastSent(st, key); got != newest {
			t.Errorf("%d pushes, the newest proved: lastSent = %q, want %q", n, got, newest)
		}
		if got, speaks := f.lastKnown(st, key); got != newest || !speaks {
			t.Errorf("%d pushes, the newest proved: lastKnown = %q speaks %v, want %q", n, got, speaks, newest)
		}
		f.recordPush(st, key, "after", at.Add(time.Second))
		if got, _ := f.lastKnown(st, key); got != "after" {
			t.Errorf("%d pushes, then one after the quote: lastKnown = %q, want after", n, got)
		}
	}
}
