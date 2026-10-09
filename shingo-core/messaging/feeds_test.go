package messaging

import (
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
