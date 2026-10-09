package engine

import (
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
