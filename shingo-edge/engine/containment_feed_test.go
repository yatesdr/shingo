package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store"
)

// containmentTestSnapshot is a snapshot as Core builds it, digest included.
func containmentTestSnapshot(t *testing.T, reason string) protocol.ContainmentSnapshot {
	t.Helper()
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	snap := protocol.ContainmentSnapshot{
		Flags: []protocol.PayloadContainmentRow{{PayloadCode: "PART-A", Active: true, Reason: reason,
			ActivatedBy: "qa", ActivatedAt: &at}},
		HeldBins: []protocol.HeldBinRow{{BinID: 9, Label: "BIN-9", PayloadCode: "PART-A", HoldBy: "edge.test", HoldAt: &at}},
		Destinations: []protocol.ContainmentDestination{{Node: "QH", Children: []string{"QH.A"},
			Bins: []protocol.ContainmentBin{{Node: "QH.A", BinID: 11, Label: "BIN-11", PayloadCode: "PART-A", UOP: 7}}}},
	}
	d, err := protocol.ContainmentDigest(snap)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	snap.Digest = d
	return snap
}

func countContainmentEvents(e *Engine) *int {
	n := new(int)
	e.Events.SubscribeTypes(func(Event) { *n++ }, EventContainmentUpdated)
	return n
}

// An applied snapshot is what readers see, its digest is what the next
// heartbeat quotes, and its row is written once. An identical re-send writes
// nothing and fires no event; a new digest writes one row and fires one.
func TestApplyContainmentSnapshot_HoldsAndCountsWrites(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	e := testEngine(t, db)
	events := countContainmentEvents(e)

	if _, held := e.LocalContainment(); held {
		t.Fatal("LocalContainment held before any snapshot")
	}
	if got := e.FeedDigests()[protocol.FeedContainment]; got != "" {
		t.Fatalf("heartbeat quotes %q before any snapshot, want \"\"", got)
	}

	first := containmentTestSnapshot(t, "burr")
	second := containmentTestSnapshot(t, "crack")
	steps := []struct {
		name       string
		snap       protocol.ContainmentSnapshot
		wantWrites int64
		wantEvents int
	}{
		{"first snapshot", first, 1, 1},
		{"identical re-send", first, 0, 1},
		{"a changed snapshot", second, 1, 2},
	}
	for _, s := range steps {
		counter.Reset()
		if err := e.ApplyContainmentSnapshot(s.snap); err != nil {
			t.Fatalf("%s: apply: %v", s.name, err)
		}
		if got := counter.Writes(); got != s.wantWrites {
			t.Errorf("%s: %d writes, want %d", s.name, got, s.wantWrites)
		}
		if *events != s.wantEvents {
			t.Errorf("%s: %d containment events so far, want %d", s.name, *events, s.wantEvents)
		}
		if got := e.FeedDigests()[protocol.FeedContainment]; got != s.snap.Digest {
			t.Errorf("%s: heartbeat quotes %q, want %q", s.name, got, s.snap.Digest)
		}
		st, held := e.LocalContainment()
		if !held || len(st.Containment) != 1 || st.Containment[0].Reason != s.snap.Flags[0].Reason {
			t.Errorf("%s: LocalContainment = %+v, %v", s.name, st, held)
		}
		if held && (len(st.Destinations) != 1 || st.ReceivedAt.IsZero()) {
			t.Errorf("%s: destinations %+v received %v", s.name, st.Destinations, st.ReceivedAt)
		}
	}
}

// A snapshot with no digest is refused: nothing is held, nothing written, and
// the heartbeat keeps quoting what it had, so Core sends again.
func TestApplyContainmentSnapshot_NoDigestRefused(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	snap := containmentTestSnapshot(t, "burr")
	snap.Digest = ""
	if err := e.ApplyContainmentSnapshot(snap); err == nil {
		t.Fatal("a snapshot with no digest applied")
	}
	if _, held := e.LocalContainment(); held {
		t.Error("a refused snapshot is held")
	}
	if got := e.FeedDigests()[protocol.FeedContainment]; got != "" {
		t.Errorf("heartbeat quotes %q after a refused snapshot", got)
	}
}

// A restart reads the held copy back: the page renders it, the heartbeat
// quotes its digest, and its times survive for the "as of". A body that no
// longer decodes is dropped with its digest, so Core resends instead of
// trusting a copy this Edge cannot show.
func TestContainmentCopy_SurvivesARestart(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	first := testEngine(t, db)
	snap := containmentTestSnapshot(t, "burr")
	if err := first.ApplyContainmentSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}

	second := testEngine(t, db)
	second.loadFeedCopies()
	second.loadContainmentCopy()
	st, held := second.LocalContainment()
	if !held || len(st.Containment) != 1 || st.Containment[0].Reason != "burr" || len(st.Destinations) != 1 {
		t.Fatalf("after restart: LocalContainment = %+v, %v", st, held)
	}
	if st.ReceivedAt.IsZero() {
		t.Error("after restart: the received time was lost")
	}
	if got := second.FeedDigests()[protocol.FeedContainment]; got != snap.Digest {
		t.Errorf("after restart: heartbeat quotes %q, want %q", got, snap.Digest)
	}

	if err := db.SaveFeedCopy(store.FeedCopy{Feed: protocol.FeedContainment, Digest: snap.Digest,
		Body: "{not json", ReceivedAt: time.Now()}); err != nil {
		t.Fatalf("corrupt the body: %v", err)
	}
	third := testEngine(t, db)
	third.loadFeedCopies()
	third.loadContainmentCopy()
	if _, held := third.LocalContainment(); held {
		t.Error("an undecodable body is held")
	}
	if got := third.FeedDigests()[protocol.FeedContainment]; got != "" {
		t.Errorf("an undecodable body's digest is quoted: %q", got)
	}
}

// A new Edge on an old Core: the ack carries no Feeds map and no snapshot ever
// arrives. The Edge reads as holding nothing, quotes "" every heartbeat (an
// old Core ignores the map), and knows the Core is old, which is what the
// containment page tells the floor.
func TestContainment_NewEdgeOnOldCore(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{StationID: "edge.test", ServerTS: time.Now().UTC()})
	if e.CoreSpeaksFeeds() {
		t.Error("an ack with no Feeds map reads as a Core that speaks feeds")
	}
	if local, _ := e.LastCoreAck(); local.IsZero() {
		t.Error("the ack from the old Core was not recorded")
	}
	if _, held := e.LocalContainment(); held {
		t.Error("LocalContainment held with no snapshot from an old Core")
	}
	if got, ok := e.FeedDigests()[protocol.FeedContainment]; !ok || got != "" {
		t.Errorf("heartbeat containment digest = %q (present %v), want \"\" present", got, ok)
	}
}

// Core's ack quoting the held digest confirms the copy; the times reach
// LocalContainment for the page's "as of".
func TestContainment_AckConfirmsTheCopy(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	snap := containmentTestSnapshot(t, "burr")
	if err := e.ApplyContainmentSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if st, _ := e.LocalContainment(); !st.ConfirmedAt.IsZero() {
		t.Fatalf("confirmed before any ack: %v", st.ConfirmedAt)
	}
	e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedContainment: snap.Digest}})
	if st, _ := e.LocalContainment(); st.ConfirmedAt.IsZero() {
		t.Error("an ack quoting the held digest did not confirm the copy")
	}
}
