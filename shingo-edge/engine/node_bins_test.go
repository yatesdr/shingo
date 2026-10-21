package engine

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// node_bins_test.go pins the Edge's half of the node-bins feed: the copy
// (applied, held, never written to SQLite), the event it fires, the heartbeat
// key, and when the watch list is published to Core.

// binsEngine is a test engine wired as engine.New wires the feed: the watch
// list is the station service's, and a list that moves publishes at once.
// Every envelope the engine sends is captured. One station holds a dedicated
// loader's home SMN_H1; its buffer spot SMN_F1 is on no station.
func binsEngine(t *testing.T, db *store.DB) (*Engine, *[]*protocol.Envelope) {
	t.Helper()
	eng := testEngine(t, db)
	eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
	eng.stationService.SetLoaderGenerationResolver(eng.loaderGeneration.Load)
	eng.binWatchSource = eng.stationService.WatchedBinNodes
	eng.stationService.SetBinNodesChangedFunc(eng.binWatchChanged)
	var sent []*protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		sent = append(sent, env)
		return nil
	})
	loaderStation(t, db, "LDR-BINS", "SMN_H1")
	setCoreLoaders(t, eng, protocol.LoaderInfo{
		Name: "Homes", LoaderKey: "loader:31", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: []protocol.LoaderPosition{
			{CoreNodeName: "SMN_H1", PayloadCode: "PART-A", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 1},
			{CoreNodeName: "SMN_F1", HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 2},
		},
	})
	return eng, &sent
}

// watchLists decodes the bins.watch lists among the captured envelopes.
func watchLists(t *testing.T, sent []*protocol.Envelope) []protocol.BinWatchList {
	t.Helper()
	var out []protocol.BinWatchList
	for _, env := range sent {
		var d protocol.Data
		if err := env.DecodePayload(&d); err != nil || d.Subject != protocol.SubjectBinsWatch {
			continue
		}
		var l protocol.BinWatchList
		if err := json.Unmarshal(d.Body, &l); err != nil {
			t.Fatalf("decode watch list: %v", err)
		}
		out = append(out, l)
	}
	return out
}

func quote(s string) *string { return &s }

func binsSnapshot(t *testing.T, rows []protocol.NodeBinsRow, groups []protocol.NodeBinsGroup) protocol.NodeBinsSnapshot {
	t.Helper()
	snap := protocol.NodeBinsSnapshot{Rows: rows, Groups: groups}
	if snap.Groups == nil {
		snap.Groups = []protocol.NodeBinsGroup{}
	}
	d, err := protocol.NodeBinsDigest(snap)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	snap.Digest = d
	return snap
}

// TestNodeBins_ApplyHoldsTheDigestAndNamesWhatChanged: the heartbeat always
// names the key ("" until something is held); an applied snapshot is held
// and quoted; the event names the nodes whose row or assignment changed and
// the groups whose wants changed, and an identical re-send fires nothing. A
// snapshot that cannot be applied leaves the held digest quoted, so a lost or
// refused snapshot is sent again on Core's next compare.
func TestNodeBins_ApplyHoldsTheDigestAndNamesWhatChanged(t *testing.T) {
	t.Parallel()
	eng, _ := binsEngine(t, testEngineDB(t))
	var events []NodeBinsUpdatedEvent
	eng.Events.SubscribeTypes(func(ev Event) {
		events = append(events, ev.Payload.(NodeBinsUpdatedEvent))
	}, EventNodeBinsUpdated)

	if d, ok := eng.FeedDigests()[protocol.FeedBins]; !ok || d != "" {
		t.Fatalf("heartbeat bins = %q (named %v), want named and \"\"", d, ok)
	}
	one := 1
	first := binsSnapshot(t, []protocol.NodeBinsRow{
		{Node: "SMN_F1", Occupied: true, PayloadCode: "PART-A", Stocked: true},
		{Node: "SMN_H1"},
	}, []protocol.NodeBinsGroup{{Group: "GRP", Wants: []protocol.NodeBinsWant{{BinType: "TOTE-1", Want: 2}},
		Assignments: []protocol.NodeBinsAssignment{{Node: "ULN_1", BinType: "TOTE-1", Layers: &one}}}})
	if err := eng.ApplyNodeBinsSnapshot(first); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := eng.FeedDigests()[protocol.FeedBins]; got != first.Digest {
		t.Errorf("heartbeat bins = %q, want the applied digest %q", got, first.Digest)
	}
	if err := eng.ApplyNodeBinsSnapshot(first); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	second := binsSnapshot(t, []protocol.NodeBinsRow{
		{Node: "SMN_F1", Occupied: true, PayloadCode: "PART-A", Stocked: false},
		{Node: "SMN_H1"},
	}, []protocol.NodeBinsGroup{{Group: "GRP", Wants: []protocol.NodeBinsWant{{BinType: "TOTE-1", Want: 3}},
		Assignments: []protocol.NodeBinsAssignment{{Node: "ULN_1", BinType: "TOTE-1", Layers: &one}}}})
	if err := eng.ApplyNodeBinsSnapshot(second); err != nil {
		t.Fatalf("apply second: %v", err)
	}
	want := []NodeBinsUpdatedEvent{
		{Nodes: []string{"SMN_F1", "SMN_H1", "ULN_1"}, Groups: []string{"GRP"}},
		{Nodes: []string{"SMN_F1"}, Groups: []string{"GRP"}},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
	for i := range want {
		if !slices.Equal(events[i].Nodes, want[i].Nodes) || !slices.Equal(events[i].Groups, want[i].Groups) {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}

	if err := eng.ApplyNodeBinsSnapshot(protocol.NodeBinsSnapshot{Rows: []protocol.NodeBinsRow{{Node: "SMN_F1"}}}); err == nil {
		t.Error("a snapshot with no digest applied")
	}
	if got := eng.FeedDigests()[protocol.FeedBins]; got != second.Digest {
		t.Errorf("after a refused snapshot the heartbeat quotes %q, want the held %q", got, second.Digest)
	}
	st := eng.LocalNodeBins()
	if !st.Held || st.Rows["SMN_F1"].Stocked || st.ReceivedAt.IsZero() {
		t.Errorf("held copy = %+v", st)
	}
}

// TestNodeBins_NoSQLiteWrite: a bins delivery, Core's confirmation of it (with
// the confirmation persist interval long past) and a watch-list publish write
// nothing to SQLite.
func TestNodeBins_NoSQLiteWrite(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng, sent := binsEngine(t, db)
	eng.stationService.WatchedBinNodes() // the list read, before the count
	snap := binsSnapshot(t, []protocol.NodeBinsRow{{Node: "SMN_F1"}, {Node: "SMN_H1"}}, nil)

	counter.Reset()
	if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	eng.feeds.mu.Lock()
	eng.feeds.held[protocol.FeedBins].persistedAt = time.Now().Add(-time.Hour)
	eng.feeds.mu.Unlock()
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: snap.Digest}, BinWatch: quote("")})
	// Writes, not every statement: the process-node generation is shared by
	// every test in the package, so a parallel test's node write can make the
	// watch list rebuild (one read) inside this window.
	if counter.Writes() != 0 {
		t.Errorf("a delivery, a confirmation and a publish wrote %d statements (%v), want none",
			counter.Writes(), counter.Statements())
	}
	if len(watchLists(t, *sent)) != 1 {
		t.Errorf("published %d watch lists, want 1", len(watchLists(t, *sent)))
	}
	if _, confirmed := eng.FeedTimes(protocol.FeedBins); confirmed.IsZero() {
		t.Error("the ack quoting the held digest did not confirm it")
	}
}

// TestBinWatch_WhenTheListIsPublished: the list goes to a Core whose ack
// carries BinWatch and quotes a different digest, at once when the list
// changes, and on the first such ack while nothing is held whatever was
// quoted (a restarted Edge asks). Never to a Core whose last ack lacked the
// field (an old Core, or one rolled back), and the guard holds a repeat back.
func TestBinWatch_WhenTheListIsPublished(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, sent := binsEngine(t, db)
	own := []string{"SMN_F1", "SMN_H1"}
	ownDigest, err := protocol.BinWatchDigest(own)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	count := func() int { return len(watchLists(t, *sent)) }

	// A Core with feeds and no bins feed: nothing published, bins by HTTP.
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if count() != 0 || eng.CoreSpeaksBins() {
		t.Fatalf("a Core without BinWatch: %d published, speaks %v", count(), eng.CoreSpeaksBins())
	}

	// A restarted Edge on a Core that still holds its list: the first ack
	// quotes this Edge's own digest, and the Edge asks anyway.
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: quote(ownDigest)})
	if lists := watchLists(t, *sent); len(lists) != 1 || !slices.Equal(lists[0].Nodes, own) || lists[0].Digest != ownDigest {
		t.Fatalf("first ack: published %+v, want one list %v", lists, own)
	}
	// The next ack quotes it: nothing.
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: quote(ownDigest)})
	if count() != 1 {
		t.Errorf("an ack quoting the Edge's own list published again (%d)", count())
	}
	// A Core that lost the list quotes "": the same list went under two
	// minutes ago, so the resend guard holds it back (Core's compare heals).
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: quote("")})
	if count() != 1 {
		t.Errorf("the guard did not hold a repeat of the same list inside its window (%d)", count())
	}

	// The list changes (a node joins the station): published at once, on the
	// next read of the list, with no heartbeat in between.
	sid := stationIDByCode(t, db, "LDR-BINS-ST")
	if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processOfStation(t, db, sid), OperatorStationID: &sid,
		CoreNodeName: "SMN_H2", Code: "SMN_H2", Name: "SMN_H2", Sequence: 2, Enabled: true}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	eng.stationService.WatchedBinNodes()
	if lists := watchLists(t, *sent); len(lists) != 2 || !slices.Equal(lists[1].Nodes, []string{"SMN_F1", "SMN_H1", "SMN_H2"}) {
		t.Fatalf("after the list changed: %+v", lists)
	}

	// Core rolled back: the next ack lacks the field. Nothing is published,
	// not even when the list moves again.
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processOfStation(t, db, sid), OperatorStationID: &sid,
		CoreNodeName: "SMN_H3", Code: "SMN_H3", Name: "SMN_H3", Sequence: 3, Enabled: true}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	eng.stationService.WatchedBinNodes()
	if count() != 2 || eng.CoreSpeaksBins() {
		t.Errorf("after Core rolled back: %d published, speaks %v", count(), eng.CoreSpeaksBins())
	}
}

// TestBinWatch_AListThatMovesEveryHeartbeatIsNotFlagged: an Edge whose list
// changes between three heartbeats in a row converges each time Core quotes
// what it sent, so the key is never flagged as not converging.
func TestBinWatch_AListThatMovesEveryHeartbeatIsNotFlagged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng, sent := binsEngine(t, db)
	sid := stationIDByCode(t, db, "LDR-BINS-ST")
	quoted := ""
	for i := 0; i < 4; i++ {
		eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: quote(quoted)})
		lists := watchLists(t, *sent)
		quoted = lists[len(lists)-1].Digest // Core takes what was sent
		name := "SMN_X" + string(rune('A'+i))
		if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processOfStation(t, db, sid), OperatorStationID: &sid,
			CoreNodeName: name, Code: name, Name: name, Sequence: 10 + i, Enabled: true}); err != nil {
			t.Fatalf("create node: %v", err)
		}
	}
	if got := len(watchLists(t, *sent)); got < 4 {
		t.Errorf("published %d lists over four moves, want at least 4", got)
	}
	if flags := eng.FeedFlags(); len(flags) != 0 {
		t.Errorf("flagged: %v", flags)
	}
}

func stationIDByCode(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM operator_stations WHERE code=?`, code).Scan(&id); err != nil {
		t.Fatalf("station %s: %v", code, err)
	}
	return id
}

func processOfStation(t *testing.T, db *store.DB, sid int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT process_id FROM operator_stations WHERE id=?`, sid).Scan(&id); err != nil {
		t.Fatalf("process of station %d: %v", sid, err)
	}
	return id
}

// TestBinWatch_PublishedAtBoot: the boot burst carries the watch list before
// any ack, and the first ack's own publish of the same list (a restarted Edge
// with nothing held asks) is held back while the boot send is in flight. A
// boot send that failed records nothing, so the first ack still asks.
func TestBinWatch_PublishedAtBoot(t *testing.T) {
	t.Parallel()
	own := []string{"SMN_F1", "SMN_H1"}
	ownDigest, err := protocol.BinWatchDigest(own)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	eng, sent := binsEngine(t, testEngineDB(t))
	var boot []*protocol.BinWatchList
	eng.PublishBinWatchAtBoot(func(l *protocol.BinWatchList) error { boot = append(boot, l); return nil })
	if len(boot) != 1 || !slices.Equal(boot[0].Nodes, own) || boot[0].Digest != ownDigest {
		t.Fatalf("boot burst: %+v, want one list %v", boot, own)
	}
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: quote(ownDigest)})
	if n := len(watchLists(t, *sent)); n != 0 {
		t.Errorf("first ack after a boot send: published %d more, want 0 (the boot send is in flight)", n)
	}

	eng, sent = binsEngine(t, testEngineDB(t))
	eng.PublishBinWatchAtBoot(func(*protocol.BinWatchList) error { return errors.New("broker down") })
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: quote("")})
	if n := len(watchLists(t, *sent)); n != 1 {
		t.Errorf("first ack after a failed boot send: published %d, want 1", n)
	}
}

// TestBinsActable_FromTheBook: the engine's own reading of the rule, through
// a snapshot applied and the acks that follow it.
func TestBinsActable_FromTheBook(t *testing.T) {
	t.Parallel()
	eng, _ := binsEngine(t, testEngineDB(t))
	clock := time.Now()
	eng.feeds.now = func() time.Time { return clock }
	own, err := protocol.BinWatchDigest([]string{"SMN_F1", "SMN_H1"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if ok, why := eng.BinsActable(); ok || why != NotActableNothingHeld {
		t.Fatalf("before any snapshot: %v %q", ok, why)
	}
	snap := binsSnapshot(t, []protocol.NodeBinsRow{{Node: "SMN_F1"}, {Node: "SMN_H1"}}, nil)
	snap.ListDigest = own
	if snap.Digest, err = protocol.NodeBinsDigest(snap); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ok, why := eng.BinsActable(); ok || why != NotActableNoAck {
		t.Errorf("held, no ack yet: %v %q", ok, why)
	}
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: snap.Digest}, BinWatch: quote(own)})
	if ok, why := eng.BinsActable(); !ok {
		t.Errorf("confirmed by the last ack: not actable (%q)", why)
	}
	clock = clock.Add(ActableAckWithin + time.Second)
	if ok, why := eng.BinsActable(); ok || why != NotActableAckOld {
		t.Errorf("no ack for %v: %v %q", ActableAckWithin, ok, why)
	}
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: "a-different-digest"}, BinWatch: quote(own)})
	if ok, why := eng.BinsActable(); ok || why != NotActableNotConfirmed {
		t.Errorf("a Core that restarted (quotes another digest): %v %q", ok, why)
	}
}

// holdGroupRow gives eng a bins copy holding one group row: the group's
// members as Core answers a watched group (row v2).
func holdGroupRow(t *testing.T, eng *Engine, group string, members ...string) {
	t.Helper()
	snap := protocol.NodeBinsSnapshot{Rows: []protocol.NodeBinsRow{{Node: group, State: protocol.BinsStateGroup, Members: members}}}
	for _, m := range members {
		snap.Rows = append(snap.Rows, protocol.NodeBinsRow{Node: m, State: protocol.BinsStateEmpty})
	}
	var err error
	if snap.Digest, err = protocol.NodeBinsDigest(snap); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

// TestGroupMembers_FromTheGroupRow: a group's members come from its group row
// on the bins copy; a name with no group row there is not known, and the miss
// rebuilds the station lists so an unwatched group is watched from then on.
func TestGroupMembers_FromTheGroupRow(t *testing.T) {
	t.Parallel()
	eng, _ := binsEngine(t, testEngineDB(t))
	if _, known := eng.groupMembers("MKT"); known {
		t.Error("nothing held: a group read as known")
	}
	holdGroupRow(t, eng, "MKT", "MKT.S1", "MKT.S2")
	if got, known := eng.groupMembers("MKT"); !known || !slices.Equal(got, []string{"MKT.S1", "MKT.S2"}) {
		t.Errorf("members = %v (known %v), want [MKT.S1 MKT.S2]", got, known)
	}
	if _, known := eng.groupMembers("MKT.S1"); known {
		t.Error("a member row (not a group) read as a group")
	}
}

// TestContainmentNodeMatches_FromTheContainmentCopy: a group containment
// destination's members are the containment copy's children; no copy held is
// no match.
func TestContainmentNodeMatches_FromTheContainmentCopy(t *testing.T) {
	t.Parallel()
	eng, _ := binsEngine(t, testEngineDB(t))
	if eng.containmentNodeMatches("QC-GRP", "A") {
		t.Error("no containment copy held: matched a member")
	}
	if !eng.containmentNodeMatches("QC-1", "QC-1") {
		t.Error("a concrete destination did not match itself")
	}
	snap := protocol.ContainmentSnapshot{Destinations: []protocol.ContainmentDestination{
		{Node: "QC-GRP", Children: []string{"QC-GRP.A", "QC-GRP.B"}, Bins: []protocol.ContainmentBin{}}}}
	var err error
	if snap.Digest, err = protocol.ContainmentDigest(snap); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := eng.ApplyContainmentSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, n := range []string{"QC-GRP.A", "B"} {
		if !eng.containmentNodeMatches("QC-GRP", n) {
			t.Errorf("%s did not match the group's member", n)
		}
	}
	if eng.containmentNodeMatches("QC-GRP", "C") {
		t.Error("a non-member matched")
	}
}
