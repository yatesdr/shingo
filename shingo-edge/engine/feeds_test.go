package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
)

// Whether Core speaks feeds is read off the ack's Feeds map: nil is an older
// Core, and the Edge keeps its own timers for it; {} is a newer Core that was
// asked about nothing. Before the first ack the Edge assumes the older Core.
func TestOnCoreAck_CoreSpeaksFeeds(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	if e.CoreSpeaksFeeds() {
		t.Fatal("before any ack: CoreSpeaksFeeds = true, want false")
	}
	cases := []struct {
		name  string
		feeds map[string]string
		want  bool
	}{
		{"newer Core, empty map", map[string]string{}, true},
		{"older Core, no map", nil, false},
		{"newer Core again", map[string]string{protocol.FeedCatalog: "c1"}, true},
	}
	for _, c := range cases {
		e.OnCoreAck(&protocol.EdgeHeartbeatAck{StationID: "edge.test", ServerTS: time.Now().UTC(), Feeds: c.feeds})
		if got := e.CoreSpeaksFeeds(); got != c.want {
			t.Errorf("%s: CoreSpeaksFeeds = %v, want %v", c.name, got, c.want)
		}
	}
}

// Only an equal digest confirms. A different one, or a key Core left out
// because it could not read it, leaves the confirmation where it was.
func TestOnCoreAck_ConfirmsOnlyAnEqualDigest(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	e.holdFeed(protocol.FeedCatalog, "c1", "")
	if _, confirmed := e.FeedTimes(protocol.FeedCatalog); !confirmed.IsZero() {
		t.Fatalf("confirmed before any ack: %v", confirmed)
	}

	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedCatalog: "c2"}})
	if _, confirmed := e.FeedTimes(protocol.FeedCatalog); !confirmed.IsZero() {
		t.Errorf("a different digest confirmed the copy at %v", confirmed)
	}
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if _, confirmed := e.FeedTimes(protocol.FeedCatalog); !confirmed.IsZero() {
		t.Errorf("a key Core left out confirmed the copy at %v", confirmed)
	}
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedCatalog: "c1"}})
	if _, confirmed := e.FeedTimes(protocol.FeedCatalog); confirmed.IsZero() {
		t.Error("an equal digest did not confirm the copy")
	}
}

// A restart keeps the containment and catalog digests, whose data is kept
// too, and only the times of the node list, whose data is memory-only: a
// persisted nodes digest would tell Core the Edge still holds a list it lost.
func TestFeedCopies_SurviveARestartOnlyWhereTheDataDoes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	first := testEngine(t, db)
	first.holdFeed(protocol.FeedContainment, "k1", `{"flags":[]}`)
	first.holdFeed(protocol.FeedCatalog, "c1", "")
	first.holdFeed(protocol.FeedNodes, "n1", "")

	second := testEngine(t, db)
	second.loadFeedCopies()
	cases := []struct {
		key        string
		wantDigest string
	}{
		{protocol.FeedContainment, "k1"},
		{protocol.FeedCatalog, "c1"},
		{protocol.FeedNodes, ""},
	}
	for _, c := range cases {
		if got := second.heldDigest(c.key); got != c.wantDigest {
			t.Errorf("%s after restart: digest %q, want %q", c.key, got, c.wantDigest)
		}
		if received, _ := second.FeedTimes(c.key); received.IsZero() {
			t.Errorf("%s after restart: received time lost", c.key)
		}
	}
	if got := second.feeds.held[protocol.FeedContainment].body; got != `{"flags":[]}` {
		t.Errorf("containment body after restart = %q", got)
	}
}

// A confirmation alone is written at most every feedConfirmPersistEvery, not
// on every ack: the row's confirmed time moves only once the last write is
// that old.
func TestOnCoreAck_ConfirmationWritesAreBounded(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	e.holdFeed(protocol.FeedCatalog, "c1", "")
	ack := &protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedCatalog: "c1"}}

	persisted := func() time.Time {
		rows, err := db.ListFeedCopies()
		if err != nil {
			t.Fatalf("list feed copies: %v", err)
		}
		for _, r := range rows {
			if r.Feed == protocol.FeedCatalog {
				return r.ConfirmedAt
			}
		}
		t.Fatal("no catalog row")
		return time.Time{}
	}

	e.OnCoreAck(ack)
	if got := persisted(); !got.IsZero() {
		t.Errorf("first confirmation, right after the receive's write: persisted %v, want none yet", got)
	}
	e.feeds.mu.Lock()
	e.feeds.held[protocol.FeedCatalog].persistedAt = time.Now().Add(-feedConfirmPersistEvery)
	e.feeds.mu.Unlock()
	e.OnCoreAck(ack)
	if got := persisted(); got.IsZero() {
		t.Error("confirmation after the bound: not persisted")
	}
}

// The Edge's resend guard for the keys it owns follows Core's rule: the same
// digest inside two minutes is held back, three sends in a row flag the key,
// and Core quoting it back clears the flag.
func TestSendDue_GuardAndFlag(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	const key = "claims:PR1"
	if !e.sendDue(key, "d1", "") {
		t.Fatal("first send: held back")
	}
	if e.sendDue(key, "d1", "") {
		t.Error("same digest inside the guard: sent")
	}
	if !e.sendDue(key, "d2", "") || !e.sendDue(key, "d3", "") {
		t.Fatal("new digests: held back")
	}
	if got := e.FeedFlags(); len(got) != 1 || got[0] != "claims:PR1 not converging: sent 3 times" {
		t.Errorf("flags after three sends = %v", got)
	}
	e.converged(key)
	if got := e.FeedFlags(); len(got) != 0 {
		t.Errorf("flags after convergence = %v, want none", got)
	}
}

// A key whose value moves on every heartbeat, with Core quoting back each
// previous send, is converging: no flag. Core still quoting the first digest
// while three new ones go out is the stuck case and flags.
func TestSendDue_StreakEndsWhenCoreHoldsTheLastSend(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	const key = "claims:PR1"
	quoted := "d0"
	for _, d := range []string{"d1", "d2", "d3", "d4"} {
		if !e.sendDue(key, d, quoted) {
			t.Fatalf("%s held back with Core holding %s", d, quoted)
		}
		quoted = d // Core took it before the next ack
	}
	if got := e.FeedFlags(); len(got) != 0 {
		t.Errorf("flags = %v after four sends Core took, want none", got)
	}
	for _, d := range []string{"d5", "d6", "d7"} {
		e.sendDue(key, d, "d4")
	}
	if got := e.FeedFlags(); len(got) != 1 || got[0] != "claims:PR1 not converging: sent 3 times" {
		t.Errorf("Core stuck on d4: flags = %v, want the not-converging flag", got)
	}
}

// TestSendDue_FollowsTheBookClock: the Edge's resend guard reads the feed
// book's clock (feeds.now), as Core's compare reads its own, not the wall
// clock. Run the book's clock past the guard and the same digest goes again;
// on the wall clock the second send would still be held back.
func TestSendDue_FollowsTheBookClock(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	e.feeds.now = func() time.Time { return clock }
	const key = "claims:PR1"
	if !e.sendDue(key, "d1", "") {
		t.Fatal("first send: held back")
	}
	clock = clock.Add(time.Minute)
	if e.sendDue(key, "d1", "") {
		t.Error("one book minute later: sent, want held back by the guard")
	}
	clock = clock.Add(time.Minute + time.Second)
	if !e.sendDue(key, "d1", "") {
		t.Error("the book's clock past the guard: held back, want a send")
	}
}

// TestOnCoreAck_StampsTheEdgeClock: an ack is stamped with this Edge's clock
// and carries Core's, so with the Edge's clock run ahead of or behind Core's
// the pair LastCoreAck returns differs by exactly the shift — the offset
// /status reports (www/core_link_status.go). Injected because the rig cannot
// shift one container's wall clock.
func TestOnCoreAck_StampsTheEdgeClock(t *testing.T) {
	t.Parallel()
	coreNow := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	for _, shift := range []time.Duration{0, 2 * time.Minute, 6 * time.Minute, -2 * time.Minute} {
		e := testEngine(t, testEngineDB(t))
		e.feeds.now = func() time.Time { return coreNow.Add(shift) }
		e.OnCoreAck(&protocol.EdgeHeartbeatAck{ServerTS: coreNow, Feeds: map[string]string{}})
		local, server := e.LastCoreAck()
		if !local.Equal(coreNow.Add(shift)) || !server.Equal(coreNow) {
			t.Errorf("shift %v: ack stamped local %v, server %v; want %v and %v", shift, local, server, coreNow.Add(shift), coreNow)
		}
		if got := server.Sub(local); got != -shift {
			t.Errorf("shift %v: server - local = %v, want %v", shift, got, -shift)
		}
	}
}

// TestOnCoreAck_TellsTheObserverAfterTheAckMoved: the observer is told once
// per ack, after LastCoreAck holds the ack it is told about, and with the
// book's lock free (it reads LastCoreAck itself); removed, it is told nothing.
func TestOnCoreAck_TellsTheObserverAfterTheAckMoved(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	e.feeds.now = func() time.Time { return at }
	var seen []time.Time
	e.SetCoreAckObserver(func() {
		local, _ := e.LastCoreAck()
		seen = append(seen, local)
	})
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	at = at.Add(time.Minute)
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	want := []time.Time{time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 9, 1, 0, 0, time.UTC)}
	if len(seen) != 2 || !seen[0].Equal(want[0]) || !seen[1].Equal(want[1]) {
		t.Fatalf("the observer saw %v, want each ack's own time %v", seen, want)
	}
	e.SetCoreAckObserver(nil)
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if len(seen) != 2 {
		t.Errorf("a removed observer was told %d more times", len(seen)-2)
	}
}

// The station's name is the one the last ack that carried it named: nothing
// before the first such ack, kept through an ack that carries none (an older
// Core, or no registry answer), "" when Core holds none, and a new one when
// Core renames the station.
func TestOnCoreAck_HoldsTheStationName(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	name := func(s string) *string { return &s }
	steps := []struct {
		say  string
		sent *string
		want string
	}{
		{"an ack with no name before any", nil, ""},
		{"the name", name("EDGE 7"), "EDGE 7"},
		{"an ack with no name keeps it", nil, "EDGE 7"},
		{"the same name again", name("EDGE 7"), "EDGE 7"},
		{"a rename on Core", name("PRESS 4 EDGE"), "PRESS 4 EDGE"},
		{"Core holding none", name(""), ""},
		{"an ack with no name after none", nil, ""},
	}
	if got := e.StationName(); got != "" {
		t.Fatalf("before any ack: StationName = %q, want none", got)
	}
	for _, s := range steps {
		e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, DisplayName: s.sent})
		if got := e.StationName(); got != s.want {
			t.Errorf("%s: StationName = %q, want %q", s.say, got, s.want)
		}
	}
}

// The first ack after more than four minutes without one reconciles orders;
// acks a minute apart do not, and the first ack since boot does not.
func TestOnCoreAck_GapReconciles(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	runs := 0
	e.feeds.reconcile = func() error { runs++; return nil }
	ack := &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}}

	e.OnCoreAck(ack)
	if runs != 0 {
		t.Fatalf("first ack since boot reconciled (%d)", runs)
	}
	e.OnCoreAck(ack)
	if runs != 0 {
		t.Fatalf("ack right after the last reconciled (%d)", runs)
	}
	e.feeds.mu.Lock()
	e.feeds.lastAck = time.Now().Add(-feedAckGap - time.Second)
	e.feeds.mu.Unlock()
	e.OnCoreAck(ack)
	if runs != 1 {
		t.Errorf("ack after a gap: reconciles = %d, want 1", runs)
	}
	e.OnCoreAck(ack)
	if runs != 1 {
		t.Errorf("ack after the gap ack: reconciles = %d, want still 1", runs)
	}
}

// feedCarriers is the Edge-inbound subject Core sends each feed on (Core's
// coreFeeds rows; the Edge's handlers in cmd/shingoedge/main.go). The scene has no
// subject of its own: its revision and geometry ride the node list.
var feedCarriers = map[string]string{
	protocol.FeedContainment: protocol.SubjectContainmentSnapshot,
	protocol.FeedRefusals:    protocol.SubjectSupplyRefusalSnapshot,
	protocol.FeedNodes:       protocol.SubjectNodeListResponse,
	protocol.FeedScene:       protocol.SubjectNodeListResponse,
	protocol.FeedCatalog:     protocol.SubjectCatalogPayloadsResponse,
	protocol.FeedBins:        protocol.SubjectBinsSnapshot,
	protocol.FeedSourcing:    protocol.SubjectSourcingState,
}

// TestFeedRows_CoverAllFeeds: every feed key has exactly one descriptor row and
// no row names a key protocol does not list, so a new feed cannot take the zero
// row's silent answers; and the subject that carries each feed to this Edge is
// one the boot coverage assertion registers (protocol.EdgeInboundSubjects).
func TestFeedRows_CoverAllFeeds(t *testing.T) {
	t.Parallel()
	rows := map[string]int{}
	for _, f := range feedRows {
		rows[f.key]++
		if f.quote == nil {
			t.Errorf("%s: row has no quote", f.key)
		}
	}
	inbound := map[string]bool{}
	for _, s := range protocol.EdgeInboundSubjects() {
		inbound[s] = true
	}
	for _, key := range protocol.AllFeeds() {
		if rows[key] != 1 {
			t.Errorf("%s: %d descriptor rows, want 1", key, rows[key])
		}
		delete(rows, key)
		carrier, ok := feedCarriers[key]
		if !ok || !inbound[carrier] {
			t.Errorf("%s: carrier %q is not in EdgeInboundSubjects", key, carrier)
		}
	}
	for key := range rows {
		t.Errorf("descriptor row %s is not in protocol.AllFeeds", key)
	}
}

// TestActable_TheDecisionRule: every way a held copy fails the decision rule,
// one at a time, against the one way it passes.
func TestActable_TheDecisionRule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ack := now.Add(-30 * time.Second)
	cases := []struct {
		name                    string
		held                    bool
		copyList                string
		received, confirmed, at time.Time
		ok                      bool
		why                     string
	}{
		{"confirmed by the last ack", true, "L", ack.Add(-time.Minute), ack, ack, true, ""},
		{"arrived after the last ack", true, "L", ack.Add(time.Second), time.Time{}, ack, true, ""},
		{"nothing held", false, "", time.Time{}, time.Time{}, ack, false, NotActableNothingHeld},
		{"built for another list", true, "OLD", ack.Add(-time.Minute), ack, ack, false, NotActableOtherList},
		{"no ack yet", true, "L", now.Add(-time.Second), time.Time{}, time.Time{}, false, NotActableNoAck},
		{"the ack is old", true, "L", ack.Add(-time.Minute), now.Add(-ActableAckWithin - time.Second), now.Add(-ActableAckWithin - time.Second), false, NotActableAckOld},
		{"at the ack limit", true, "L", ack.Add(-time.Minute), now.Add(-ActableAckWithin), now.Add(-ActableAckWithin), true, ""},
		{"the last ack quoted another digest", true, "L", ack.Add(-time.Minute), ack.Add(-time.Minute), ack, false, NotActableNotConfirmed},
	}
	for _, c := range cases {
		ok, why := Actable(c.held, c.copyList, "L", c.received, c.confirmed, c.at, now)
		if ok != c.ok || why != c.why {
			t.Errorf("%s: Actable = %v %q, want %v %q", c.name, ok, why, c.ok, c.why)
		}
	}
}

// Core quotes what it held when it answered, and more may have been sent
// since: a quote of a send made since the last one Core proved ends the streak,
// up to the newest 256 of them. The same rule as Core's (protocol/resend).
func TestSendDue_AQuoteOfAnOlderSendConverges(t *testing.T) {
	t.Parallel()
	const key = "claims:PR1"
	cases := []struct {
		sends, quoted int
		kept          bool
	}{
		{3, 1, true}, {3, 2, true}, {3, 3, true}, {22, 1, true},
		{256, 1, true}, {257, 1, false}, {257, 2, true}, {300, 44, false}, {300, 45, true},
	}
	db := testEngineDB(t)
	for _, c := range cases {
		e := testEngine(t, db)
		for i := 1; i <= c.sends; i++ {
			e.sendDue(key, fmt.Sprintf("d%d", i), "")
		}
		if len(e.FeedFlags()) != 1 {
			t.Fatalf("fixture: %d sends never quoted back, flags %v", c.sends, e.FeedFlags())
		}
		if !e.sendDue(key, "next", fmt.Sprintf("d%d", c.quoted)) {
			t.Fatalf("%d sends, quote %d: the newer value was held back", c.sends, c.quoted)
		}
		if got := len(e.FeedFlags()) == 0; got != c.kept {
			t.Errorf("%d sends, then Core quotes send %d: converging %v, want %v", c.sends, c.quoted, got, c.kept)
		}
	}
}

// Core taking each copy one ack behind is converging.
func TestSendDue_CoreOneAckBehindIsNotFlagged(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	const key = "claims:PR1"
	for i, quoted := range []string{"d0", "d0", "d1", "d2", "d3", "d4"} {
		if !e.sendDue(key, fmt.Sprintf("d%d", i+1), quoted) {
			t.Fatalf("send %d held back", i+1)
		}
		if got := e.FeedFlags(); len(got) != 0 {
			t.Errorf("ack %d, Core one copy behind: flags = %v, want none", i+1, got)
		}
	}
}

// A Core frozen at one digest is believed once at most wherever that digest
// stands among the sends, never sent or empty: the third send flags.
func TestSendDue_AFrozenQuoteStillFlags(t *testing.T) {
	t.Parallel()
	const key = "claims:PR1"
	db := testEngineDB(t)
	for _, frozen := range []string{"", "never-sent", "d1", "d2"} {
		e := testEngine(t, db)
		e.sendDue(key, "d1", "")
		e.converged(key)
		e.sendDue(key, "d2", "d1")
		for i := 3; i <= 5; i++ {
			e.sendDue(key, fmt.Sprintf("d%d", i), frozen)
		}
		if got := e.FeedFlags(); len(got) != 1 {
			t.Errorf("Core frozen at %q: flags = %v, want the flag", frozen, got)
		}
	}
}

// The watch list a bins snapshot names ends the streak when it is any list
// sent since the last one proved, not only the newest.
func TestHeldByCore_AnOlderSendEndsTheStreak(t *testing.T) {
	t.Parallel()
	const key = "bins.watch"
	e := testEngine(t, testEngineDB(t))
	for _, d := range []string{"w1", "w2", "w3"} {
		e.sendDue(key, d, "")
	}
	e.heldByCore(key, "w2")
	if got := e.FeedFlags(); len(got) != 0 {
		t.Errorf("a snapshot answered w2 with w3 on its way: flags = %v, want none", got)
	}
	if e.sendDue(key, "w3", "w2") {
		t.Error("w3 is in flight: want it held back")
	}
}

// A Core that held what was sent and restarted is sent it at once, inside the
// guard, and that send is then in flight.
func TestSendDue_ARestartedCoreIsHealedAtOnce(t *testing.T) {
	t.Parallel()
	const key = "bins.watch"
	e := testEngine(t, testEngineDB(t))
	e.sendDue(key, "w1", "")
	e.heldByCore(key, "w1")
	if !e.sendDue(key, "w1", "") {
		t.Error("held, then quoted as nothing: want the same digest at once")
	}
	if e.sendDue(key, "w1", "") {
		t.Error("that copy in flight: want it held back")
	}
	if got := e.FeedFlags(); len(got) != 0 {
		t.Errorf("flags = %v, want none", got)
	}
}
