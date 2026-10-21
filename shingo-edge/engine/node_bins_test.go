package engine

import (
	"encoding/json"
	"errors"
	"fmt"
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
	if count() != 0 || speaksBins(eng) {
		t.Fatalf("a Core without BinWatch: %d published, speaks %v", count(), speaksBins(eng))
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
	if count() != 2 || speaksBins(eng) {
		t.Errorf("after Core rolled back: %d published, speaks %v", count(), speaksBins(eng))
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

// TestNodeBins_ApplyRowsPatchesTheCopyKeepsItsDigest: the rows a refused write
// carries replace those rows in the held copy and fire the event for the nodes
// that changed; the held digest still names the snapshot Core sent, so the
// next heartbeat compare fetches Core's current one. A row for a name the copy
// does not hold is not added, and with no copy held the rows are dropped.
func TestNodeBins_ApplyRowsPatchesTheCopyKeepsItsDigest(t *testing.T) {
	t.Parallel()
	eng, _ := binsEngine(t, testEngineDB(t))
	var events []NodeBinsUpdatedEvent
	eng.Events.SubscribeTypes(func(ev Event) {
		events = append(events, ev.Payload.(NodeBinsUpdatedEvent))
	}, EventNodeBinsUpdated)

	eng.ApplyNodeBinsRows([]protocol.NodeBinsRow{{Node: "SMN_F1", State: protocol.BinsStateEmpty}})
	if eng.LocalNodeBins().Held || len(events) != 0 {
		t.Fatalf("rows with no copy held: held %v, events %+v; want dropped", eng.LocalNodeBins().Held, events)
	}

	snap := binsSnapshot(t, []protocol.NodeBinsRow{
		{Node: "SMN_F1", State: protocol.BinsStateOccupied, Occupied: true, BinID: 7, PayloadCode: "PART-A", Stocked: true},
		{Node: "SMN_H1", State: protocol.BinsStateEmpty},
	}, nil)
	if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	events = nil
	eng.ApplyNodeBinsRows([]protocol.NodeBinsRow{
		{Node: "SMN_F1", State: protocol.BinsStateOccupied, Occupied: true, BinID: 8, PayloadCode: "PART-B", Stocked: true},
		{Node: "SMN_H1", State: protocol.BinsStateEmpty},
		{Node: "NOT-WATCHED", State: protocol.BinsStateEmpty},
	})
	st := eng.LocalNodeBins()
	if r := st.Rows["SMN_F1"]; r.BinID != 8 || r.PayloadCode != "PART-B" {
		t.Errorf("SMN_F1 = %+v, want Core's row (bin 8, PART-B)", r)
	}
	if _, added := st.Rows["NOT-WATCHED"]; added {
		t.Error("a row for a name the copy does not hold was added")
	}
	if got := eng.FeedDigests()[protocol.FeedBins]; got != snap.Digest {
		t.Errorf("heartbeat bins = %q, want the held snapshot's %q", got, snap.Digest)
	}
	if len(events) != 1 || !slices.Equal(events[0].Nodes, []string{"SMN_F1"}) {
		t.Errorf("events = %+v, want one naming SMN_F1", events)
	}
}

// holdActableBins gives eng a bins copy it may decide from: rows as Core
// answered this Edge's current watch list (its list digest), confirmed by a
// heartbeat ack taken now. A decision site's test holds what Core says here
// instead of answering an HTTP read.
func holdActableBins(t *testing.T, eng *Engine, rows ...protocol.NodeBinsRow) {
	t.Helper()
	if err := pushBins(eng, rows...); err != nil {
		t.Fatalf("holdActableBins: %v", err)
	}
}

// pushBins is holdActableBins for a fake Core that pushes its table from its
// own goroutine (an HTTP handler that changed a bin): an error instead of a
// test failure.
func pushBins(eng *Engine, rows ...protocol.NodeBinsRow) error {
	_, own, ok := eng.binWatchList()
	if !ok {
		return errors.New("the watch list has no digest")
	}
	for i := range rows {
		if rows[i].State == "" {
			rows[i].State = protocol.BinsStateEmpty
			if rows[i].Occupied {
				rows[i].State = protocol.BinsStateOccupied
			}
		}
	}
	snap := protocol.NodeBinsSnapshot{ListDigest: own, Rows: rows, Groups: []protocol.NodeBinsGroup{}}
	var err error
	if snap.Digest, err = protocol.NodeBinsDigest(snap); err != nil {
		return fmt.Errorf("digest: %w", err)
	}
	if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	ackHeld(eng)
	if ok, why := eng.BinsActable(); !ok {
		return fmt.Errorf("the copy is not actable (%s)", why)
	}
	return nil
}

// TestBinsForDecision_FailsClosed: a decision reads rows only from a copy it
// may decide from; otherwise every name is refused with Actable's reason and
// the operator's sentence. A name with no row is absent (binsForDecision) or
// refused as unwatched (binsRowAt); an unknown name is a row, never empty.
func TestBinsForDecision_FailsClosed(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	_, err := eng.binsForDecision("SMN_A1")
	var nc *BinsNotCurrent
	if !errors.As(err, &nc) || nc.Node != "SMN_A1" || nc.Why != NotActableNothingHeld {
		t.Fatalf("nothing held: err = %v, want BinsNotCurrent{SMN_A1, %q}", err, NotActableNothingHeld)
	}
	if got, want := err.Error(), "Core's picture of SMN_A1 is not current — try again"; got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	holdActableBins(t, eng,
		protocol.NodeBinsRow{Node: "SMN_A1", Occupied: true, BinID: 7},
		protocol.NodeBinsRow{Node: "SMN_GONE", State: protocol.BinsStateUnknown})
	rows, err := eng.binsForDecision("SMN_A1", "SMN_GONE", "SMN_NOT_WATCHED")
	if err != nil {
		t.Fatalf("actable: %v", err)
	}
	if rows["SMN_A1"].BinID != 7 || rows["SMN_GONE"].State != protocol.BinsStateUnknown {
		t.Errorf("rows = %+v", rows)
	}
	if _, ok := rows["SMN_NOT_WATCHED"]; ok {
		t.Error("a name the copy holds no row for was answered")
	}
	if _, err := eng.binsRowAt("SMN_NOT_WATCHED"); !errors.As(err, &nc) || nc.Why != NotActableUnwatched {
		t.Errorf("binsRowAt unwatched: %v, want BinsNotCurrent{%q}", err, NotActableUnwatched)
	}
	eng.feeds.now = func() time.Time { return time.Now().Add(ActableAckWithin + time.Second) }
	if _, err := eng.binsRowAt("SMN_A1"); !errors.As(err, &nc) || nc.Why != NotActableAckOld {
		t.Errorf("ack aged out: %v, want BinsNotCurrent{%q}", err, NotActableAckOld)
	}
}

// holdActableContainment gives eng a containment copy it may decide from: the
// destinations as Core sent them, confirmed by a heartbeat ack taken now.
func holdActableContainment(t *testing.T, eng *Engine, dests ...protocol.ContainmentDestination) {
	t.Helper()
	snap := protocol.ContainmentSnapshot{Flags: []protocol.PayloadContainmentRow{}, HeldBins: []protocol.HeldBinRow{},
		Destinations: dests}
	var err error
	if snap.Digest, err = protocol.ContainmentDigest(snap); err != nil {
		t.Fatalf("holdActableContainment: digest: %v", err)
	}
	if err := eng.ApplyContainmentSnapshot(snap); err != nil {
		t.Fatalf("holdActableContainment: apply: %v", err)
	}
	ackHeld(eng)
	if _, err := eng.containmentForDecision("any"); err != nil {
		t.Fatalf("holdActableContainment: not actable: %v", err)
	}
}

// ackHeld is a heartbeat ack, taken now, from a Core that holds every feed
// digest eng holds and eng's watch list: each held copy is confirmed by it.
func ackHeld(eng *Engine) {
	feeds := map[string]string{}
	eng.feeds.mu.Lock()
	for key, h := range eng.feeds.held {
		feeds[key] = h.digest
	}
	eng.feeds.mu.Unlock()
	// The same Core: the kinds it answered, it still answers.
	asks := []string{}
	eng.asks.mu.Lock()
	for k := range eng.asks.kinds {
		asks = append(asks, k)
	}
	eng.asks.mu.Unlock()
	_, own, _ := eng.binWatchList()
	eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: feeds, BinWatch: quote(own), Asks: asks})
}

// binsRowsOf is what Core's bins table says for rows written as node-bins
// answers (the HTTP stubs' tables), plus an empty row for each name in also
// that rows leaves out (the stubs answer any name as an empty node Core has).
// Stocked is the count above zero, Partial above zero and below the row's
// capacity; Epoch the delta epoch. Sorted by name.
func binsRowsOf(rows map[string]NodeBinInfo, also ...string) []protocol.NodeBinsRow {
	names := map[string]bool{}
	for n := range rows {
		names[n] = true
	}
	for _, n := range also {
		if n != "" {
			names[n] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	slices.Sort(sorted)
	out := make([]protocol.NodeBinsRow, 0, len(sorted))
	for _, n := range sorted {
		b := rows[n]
		r := protocol.NodeBinsRow{Node: n, State: protocol.BinsStateEmpty, Occupied: b.Occupied}
		if b.Occupied {
			r.State = protocol.BinsStateOccupied
			r.BinID, r.BinLabel, r.BinTypeCode, r.Bare, r.PayloadCode = b.BinID, b.BinLabel, b.BinTypeCode, b.Bare, b.PayloadCode
			r.Stocked = b.UOPRemaining > 0
			r.Partial = b.UOPRemaining > 0 && b.UOPRemaining < b.UOPCapacity
			r.UOPCapacity, r.Epoch = b.UOPCapacity, b.DeltaEpoch
			r.Manifest, r.ManifestConfirmed = b.Manifest, b.ManifestConfirmed
			r.TotesOnCart, r.TotesPerLayer = b.TotesOnCart, b.TotesPerLayer
			r.HeldSince, r.HeldFor = b.HeldSince, b.HeldFor
			if b.HeldSince != "" {
				r.HeldUOP = b.UOPRemaining
			}
		}
		out = append(out, r)
	}
	return out
}

// holdCoreRows holds, as a table eng may decide from, what an HTTP stub's
// node-bins table says (binsRowsOf): the fixtures that answered node-bins
// over HTTP hold the same answer for the sites that read the table.
func holdCoreRows(t *testing.T, eng *Engine, rows map[string]NodeBinInfo, also ...string) {
	t.Helper()
	holdActableBins(t, eng, binsRowsOf(rows, also...)...)
}

// holdProcessNodes holds, as a table eng may decide from, the answer
// fakeCoreBinServer gives for any node: every process node in eng's store
// occupied (or not) by one bin carrying payload, bin 77.
func holdProcessNodes(t *testing.T, eng *Engine, occupied bool, payload string) {
	t.Helper()
	nodes, err := eng.db.ListProcessNodes()
	if err != nil {
		t.Fatalf("holdProcessNodes: %v", err)
	}
	rows := map[string]NodeBinInfo{}
	for _, n := range nodes {
		b := NodeBinInfo{NodeName: n.CoreNodeName, Occupied: occupied}
		if occupied {
			b.PayloadCode, b.BinID = payload, 77
		}
		rows[n.CoreNodeName] = b
	}
	holdCoreRows(t, eng, rows)
}

// placesOf is every place a REQUEST or a keeper may read in eng's store: each
// process node, and each place a claim names (its line, paired positions,
// staging and keep-staged spot). Sorted, each once.
func placesOf(t *testing.T, eng *Engine) []string {
	t.Helper()
	rows, err := eng.db.DB.Query(`SELECT core_node_name FROM process_nodes
		UNION SELECT core_node_name FROM style_node_claims
		UNION SELECT paired_core_node FROM style_node_claims
		UNION SELECT second_paired_core_node FROM style_node_claims
		UNION SELECT inbound_staging FROM style_node_claims
		UNION SELECT outbound_staging FROM style_node_claims
		UNION SELECT keep_staged_node FROM style_node_claims`)
	if err != nil {
		t.Fatalf("placesOf: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("placesOf: %v", err)
		}
		if n != "" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// holdPlaces holds, as a table eng may decide from, what a node-bins stub
// answering by name says (answer) for every place in eng's store (placesOf):
// a name answer leaves out (ok false) gets no row.
func holdPlaces(t *testing.T, eng *Engine, answer func(name string) (NodeBinInfo, bool)) {
	t.Helper()
	rows := map[string]NodeBinInfo{}
	for _, n := range placesOf(t, eng) {
		if b, ok := answer(n); ok {
			rows[n] = b
		}
	}
	holdCoreRows(t, eng, rows)
}

// occupiedAt is a node-bins answer occupied exactly at the names given, as
// the census and press stubs answer.
func occupiedAt(names ...string) func(string) (NodeBinInfo, bool) {
	return func(n string) (NodeBinInfo, bool) {
		return NodeBinInfo{NodeName: n, Occupied: slices.Contains(names, n)}, true
	}
}

// everyAs is a node-bins answer of one verdict for every name, as
// headOccupancyStub answers.
func everyAs(occupied bool) func(string) (NodeBinInfo, bool) {
	return func(n string) (NodeBinInfo, bool) { return NodeBinInfo{NodeName: n, Occupied: occupied}, true }
}

// speaksBins is whether the last heartbeat ack carried BinWatch (the feed
// book's binWatchCarried): false before the first ack and after an ack from a
// Core without the feed.
func speaksBins(e *Engine) bool {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	return e.feeds.binWatchCarried
}
