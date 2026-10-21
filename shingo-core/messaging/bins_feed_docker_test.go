//go:build docker

package messaging

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// binsFixture is three watched places and one unknown name: SMN_A1 holds a
// stocked tote cart, SMN_A2 a bare cart with nothing in it, LANE-1.SLOT-1 (a
// child named by its dotted name) an empty node, and GHOST no node at all.
type binsFixture struct {
	db           *store.DB
	full, base   int64 // bin ids
	a1, a2, slot int64 // node ids
	manifest     string
}

// The manifest is written as Postgres hands jsonb back, so the rows compare
// byte for byte.
func newBinsFixture(t *testing.T, db *store.DB) binsFixture {
	t.Helper()
	var tote, bare int64
	testutil.MustNoErr(t, db.DB.QueryRow(
		`INSERT INTO bin_types (code, description) VALUES ('TOTE', 'synthetic') RETURNING id`).Scan(&tote), "tote type")
	testutil.MustNoErr(t, db.DB.QueryRow(
		`INSERT INTO bin_types (code, description, bare_of) VALUES ('TOTE-BARE', 'synthetic', $1) RETURNING id`,
		tote).Scan(&bare), "bare type")
	node := func(name string, parent *int64) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent}
		testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
		return n
	}
	a1, a2 := node("SMN_A1", nil), node("SMN_A2", nil)
	lane := node("LANE-1", nil)
	slot := node("SLOT-1", &lane.ID)
	bin := func(label string, typ, nodeID int64) int64 {
		b := &bins.Bin{BinTypeID: typ, Label: label, NodeID: &nodeID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(b), "create bin "+label)
		return b.ID
	}
	f := binsFixture{db: db, a1: a1.ID, a2: a2.ID, slot: slot.ID, manifest: `{"items": [{"part_number": "PART-A"}]}`}
	f.full = bin("TOTE-1", tote, a1.ID)
	testutil.MustNoErr(t, db.SetBinManifest(f.full, f.manifest, "PART-A", 40), "manifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(f.full, ""), "confirm")
	f.base = bin("TOTE-2", bare, a2.ID)
	return f
}

var binsFixtureNames = []string{"GHOST", "LANE-1.SLOT-1", "SMN_A1", "SMN_A2"}

func binsWatchList(t *testing.T, names ...string) *protocol.BinWatchList {
	t.Helper()
	d, err := protocol.BinWatchDigest(names)
	testutil.MustNoErr(t, err, "watch digest")
	return &protocol.BinWatchList{Digest: d, Nodes: names}
}

// wantRows is the fixture's snapshot rows, as the HTTP node-bins answer reads
// the same places: one row per name, sorted, nothing but the name for a node
// with no bin and for a name Core does not know.
func (f binsFixture) wantRows() []protocol.NodeBinsRow {
	return []protocol.NodeBinsRow{
		{Node: "GHOST"},
		{Node: "LANE-1.SLOT-1"},
		{Node: "SMN_A1", Occupied: true, BinID: f.full, BinLabel: "TOTE-1", BinTypeCode: "TOTE",
			PayloadCode: "PART-A", Manifest: &f.manifest, ManifestConfirmed: true, Stocked: true},
		{Node: "SMN_A2", Occupied: true, BinID: f.base, BinLabel: "TOTE-2", BinTypeCode: "TOTE-BARE", Bare: true},
	}
}

// One build is the two reads B8 gave the HTTP answer — every name at once,
// then the bins at every resolved node — and its value is the golden: the rows,
// no groups, and the digest of exactly what is sent.
func TestBuildNodeBins_TwoStatementsWhateverTheListLength(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	f := newBinsFixture(t, db)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	svc := NewCoreDataService(cdb, &feedsPinResponder{}, service.EpochAnnounce{})
	svc.bins.replace("edge.a", binsWatchList(t, binsFixtureNames...))

	counter.Reset()
	snap, known, err := svc.buildNodeBins("edge.a")
	testutil.MustNoErr(t, err, "build")
	if !known {
		t.Fatal("known = false for an Edge whose list Core holds")
	}
	if got := counter.Count(); got != 2 {
		t.Errorf("one build = %d statements, want 2", got)
	}
	if !reflect.DeepEqual(snap.Rows, f.wantRows()) {
		t.Errorf("rows =\n %+v\nwant\n %+v", snap.Rows, f.wantRows())
	}
	d, err := protocol.NodeBinsDigest(*snap)
	testutil.MustNoErr(t, err, "digest")
	if snap.Digest != d {
		t.Errorf("snapshot digest %q, NodeBinsDigest of the value sent %q", snap.Digest, d)
	}
	want := fmt.Sprintf(`{"digest":%q,"rows":[`+
		`{"node":"GHOST","occupied":false,"manifest_confirmed":false,"stocked":false},`+
		`{"node":"LANE-1.SLOT-1","occupied":false,"manifest_confirmed":false,"stocked":false},`+
		`{"node":"SMN_A1","occupied":true,"bin_id":%d,"bin_label":"TOTE-1","bin_type_code":"TOTE",`+
		`"payload_code":"PART-A","manifest":"{\"items\": [{\"part_number\": \"PART-A\"}]}",`+
		`"manifest_confirmed":true,"stocked":true},`+
		`{"node":"SMN_A2","occupied":true,"bin_id":%d,"bin_label":"TOTE-2","bin_type_code":"TOTE-BARE",`+
		`"bare":true,"manifest_confirmed":false,"stocked":false}],"groups":[]}`, d, f.full, f.base)
	if got := renderTwiceCore(t, snap); got != want {
		t.Errorf("snapshot =\n  %s\nwant\n  %s", got, want)
	}

	// The same two statements for a long list, and the bytes it costs.
	var tote int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT id FROM bin_types WHERE code = 'TOTE'`).Scan(&tote), "tote type")
	for _, n := range []int{22, 40} {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("SMN_%03d", i)
			if i%2 == 0 {
				x := &nodes.Node{Name: names[i], Enabled: true}
				testutil.MustNoErr(t, db.CreateNode(x), "node")
				testutil.MustNoErr(t, db.CreateBin(&bins.Bin{BinTypeID: tote, Label: fmt.Sprintf("B-%d-%03d", n, i),
					NodeID: &x.ID, Status: "available"}), "bin")
			}
		}
		station := fmt.Sprintf("edge.%d", n)
		svc.bins.replace(station, binsWatchList(t, names...))
		counter.Reset()
		big, _, err := svc.buildNodeBins(station)
		testutil.MustNoErr(t, err, "build")
		if got := counter.Count(); got != 2 {
			t.Errorf("%d names: one build = %d statements, want 2", n, got)
		}
		raw, zipped := wireBytes(t, big)
		t.Logf("%d watched nodes, half occupied: %d bytes raw, %d gzipped", n, raw, zipped)
		// The next size seeds afresh.
		_, err = db.DB.Exec(`DELETE FROM bins WHERE label LIKE 'B-%'`)
		testutil.MustNoErr(t, err, "clear bins")
		_, err = db.DB.Exec(`DELETE FROM nodes WHERE name LIKE 'SMN_0%'`)
		testutil.MustNoErr(t, err, "clear nodes")
	}
}

func renderTwiceCore(t *testing.T, v any) string {
	t.Helper()
	a, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "marshal")
	b, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "marshal again")
	if !bytes.Equal(a, b) {
		t.Fatalf("two renderings differ:\n  %s\n  %s", a, b)
	}
	return string(a)
}

func wireBytes(t *testing.T, v any) (raw, zipped int) {
	t.Helper()
	b, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "marshal")
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, err = w.Write(b)
	testutil.MustNoErr(t, err, "gzip write")
	testutil.MustNoErr(t, w.Close(), "gzip")
	return len(b), z.Len()
}

// What moves the digest is what a station draws. A count that ticks, an
// adjustment that stays above zero and an epoch reset do not; a count reaching
// zero or going below it reads not stocked.
func TestBuildNodeBins_CountsDoNotMoveTheDigest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	f := newBinsFixture(t, db)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	svc.bins.replace("edge.a", binsWatchList(t, binsFixtureNames...))
	digest := func() (string, bool) {
		snap, _, err := svc.buildNodeBins("edge.a")
		testutil.MustNoErr(t, err, "build")
		for _, r := range snap.Rows {
			if r.Node == "SMN_A1" {
				return snap.Digest, r.Stocked
			}
		}
		t.Fatal("no SMN_A1 row")
		return "", false
	}
	set := func(sql string, args ...any) {
		t.Helper()
		_, err := db.DB.Exec(sql, args...)
		testutil.MustNoErr(t, err, sql)
	}
	base, _ := digest()
	for _, step := range []struct {
		name        string
		sql         string
		arg         any
		wantSame    bool
		wantStocked bool
	}{
		{"a count tick", `UPDATE bins SET uop_remaining = uop_remaining - 1 WHERE id = $1`, f.full, true, true},
		{"an adjustment staying above zero", `UPDATE bins SET uop_remaining = 7 WHERE id = $1`, f.full, true, true},
		{"an epoch reset", `UPDATE bins SET delta_epoch = delta_epoch + 1 WHERE id = $1`, f.full, true, true},
		{"the count reaches zero", `UPDATE bins SET uop_remaining = 0 WHERE id = $1`, f.full, false, false},
		{"the count goes below zero", `UPDATE bins SET uop_remaining = -2 WHERE id = $1`, f.full, false, false},
	} {
		set(step.sql, step.arg)
		got, stocked := digest()
		if (got == base) != step.wantSame {
			t.Errorf("%s: digest %s, before %s, want same = %v", step.name, got, base, step.wantSame)
		}
		if stocked != step.wantStocked {
			t.Errorf("%s: stocked = %v, want %v", step.name, stocked, step.wantStocked)
		}
	}
}

// A watch list is answered with its snapshot at once, whatever was last sent,
// and the ack quotes its digest from then on. A changed list is answered the
// same way, with no heartbeat in between.
func TestHandleBinWatch_AnswersEveryListWithItsSnapshot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	f := newBinsFixture(t, db)
	_, err := db.EnrollEdge("edge.a", "", "edge.a")
	testutil.MustNoErr(t, err, "enroll")
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	send := "send " + protocol.SubjectBinsSnapshot + " -> edge.a"

	list := binsWatchList(t, binsFixtureNames...)
	svc.HandleBinWatch(feedsPinEnv("edge.a"), list)
	if got := resp.trace(); !reflect.DeepEqual(got, []string{send}) {
		t.Fatalf("on a list: trace = %v, want one snapshot", got)
	}
	first := resp.events[0].payload.(*protocol.NodeBinsSnapshot)
	if !reflect.DeepEqual(first.Rows, f.wantRows()) {
		t.Errorf("rows = %+v", first.Rows)
	}

	// The same list again is sent again: a restarted Edge that holds nothing
	// asks with the list Core already has.
	svc.HandleBinWatch(feedsPinEnv("edge.a"), list)
	if got := len(resp.trace()); got != 2 {
		t.Errorf("the same list again: %d sends in all, want 2", got)
	}

	ack := heartbeatAck(t, svc, resp, map[string]string{protocol.FeedBins: first.Digest})
	if ack.BinWatch == nil || *ack.BinWatch != list.Digest {
		t.Errorf("ack bin_watch = %v, want %s", ack.BinWatch, list.Digest)
	}
	if ack.Feeds[protocol.FeedBins] != first.Digest {
		t.Errorf("ack feeds[bins] = %q, want %s", ack.Feeds[protocol.FeedBins], first.Digest)
	}

	resp.events = nil
	shorter := binsWatchList(t, "SMN_A1")
	svc.HandleBinWatch(feedsPinEnv("edge.a"), shorter)
	if got := resp.trace(); !reflect.DeepEqual(got, []string{send}) {
		t.Fatalf("a changed list: trace = %v, want its snapshot with no heartbeat", got)
	}
	if rows := resp.events[0].payload.(*protocol.NodeBinsSnapshot).Rows; len(rows) != 1 || rows[0].Node != "SMN_A1" {
		t.Errorf("rows for the changed list = %+v", rows)
	}
}

// heartbeatAck runs one heartbeat with this Feeds map and returns its ack.
func heartbeatAck(t *testing.T, svc *CoreDataService, resp *feedsPinResponder, feeds map[string]string) *protocol.EdgeHeartbeatAck {
	t.Helper()
	svc.HandleEdgeHeartbeat(feedsPinEnv("edge.a"), &protocol.EdgeHeartbeat{StationID: "edge.a", Feeds: feeds})
	for i := len(resp.events) - 1; i >= 0; i-- {
		if a, ok := resp.events[i].payload.(*protocol.EdgeHeartbeatAck); ok {
			return a
		}
	}
	t.Fatal("no ack")
	return nil
}

// The mixed versions Core can see. An old Edge (no Feeds) is answered as it
// always was: no bin_watch, nothing built, nothing sent. An Edge that speaks
// feeds and has published no list is told "" and nothing more: the bins key
// is unknown to the compare, so it is not answered and not sent. An Edge that
// stops quoting the bins key loses its list.
func TestBinsFeed_EdgesWithoutAListGetNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	newBinsFixture(t, db)
	_, err := db.EnrollEdge("edge.a", "", "edge.a")
	testutil.MustNoErr(t, err, "enroll")
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	ackOnly := []string{"reply " + protocol.SubjectEdgeHeartbeatAck}

	old := heartbeatAck(t, svc, resp, nil)
	if old.BinWatch != nil {
		t.Errorf("old Edge: bin_watch = %q, want absent", *old.BinWatch)
	}
	if got := resp.trace(); !reflect.DeepEqual(got, ackOnly) {
		t.Errorf("old Edge: trace = %v, want the ack alone", got)
	}

	resp.events = nil
	fresh := heartbeatAck(t, svc, resp, map[string]string{protocol.FeedBins: ""})
	if fresh.BinWatch == nil || *fresh.BinWatch != "" {
		t.Errorf("no list held: bin_watch = %v, want \"\"", fresh.BinWatch)
	}
	if _, answered := fresh.Feeds[protocol.FeedBins]; answered {
		t.Errorf("no list held: feeds = %v, want no bins key", fresh.Feeds)
	}
	if got := resp.trace(); !reflect.DeepEqual(got, ackOnly) {
		t.Errorf("no list held: trace = %v, want the ack alone", got)
	}

	svc.HandleBinWatch(feedsPinEnv("edge.a"), binsWatchList(t, binsFixtureNames...))
	resp.events = nil
	rolledBack := heartbeatAck(t, svc, resp, map[string]string{protocol.FeedCatalog: ""})
	if rolledBack.BinWatch == nil || *rolledBack.BinWatch != "" {
		t.Errorf("an Edge that stopped quoting bins: bin_watch = %v, want \"\" (its list dropped)", rolledBack.BinWatch)
	}
	if _, held := svc.bins.lists["edge.a"]; held {
		t.Error("an Edge that stopped quoting bins still has its list")
	}
}

// A lost snapshot heals on the heartbeat: the Edge quotes what it holds, Core's
// digest differs, and the snapshot goes. A push does not arm the resend guard,
// so the heartbeat after a lost push sends at once.
func TestBinsFeed_ALostSnapshotIsResentByTheHeartbeat(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	newBinsFixture(t, db)
	_, err := db.EnrollEdge("edge.a", "", "edge.a")
	testutil.MustNoErr(t, err, "enroll")
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	svc.HandleBinWatch(feedsPinEnv("edge.a"), binsWatchList(t, binsFixtureNames...)) // lost on the way

	resp.events = nil
	heartbeatAck(t, svc, resp, map[string]string{protocol.FeedBins: ""})
	want := []string{"send " + protocol.SubjectBinsSnapshot + " -> edge.a", "reply " + protocol.SubjectEdgeHeartbeatAck}
	if got := resp.trace(); !reflect.DeepEqual(got, want) {
		t.Errorf("heartbeat after a lost snapshot: trace = %v, want %v", got, want)
	}
}

// syncSends counts bins snapshots sent per station, safe to read while push
// timers run.
type syncSends struct {
	mu    sync.Mutex
	sends map[string]int
}

func (r *syncSends) dbg(string, ...any)                        {}
func (r *syncSends) replyData(*protocol.Envelope, string, any) {}
func (r *syncSends) sendData(subject, station string, _ any) {
	if subject != protocol.SubjectBinsSnapshot {
		return
	}
	r.mu.Lock()
	r.sends[station]++
	r.mu.Unlock()
}

func (r *syncSends) count(station string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends[station]
}

// settle waits out every armed push: well past the delay, and until no Edge is
// pending.
func settle(t *testing.T, svc *CoreDataService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		time.Sleep(4 * svc.bins.delay)
		svc.bins.mu.Lock()
		n := len(svc.bins.pending)
		svc.bins.mu.Unlock()
		if n == 0 {
			time.Sleep(4 * svc.bins.delay) // a push that started is now done
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("pushes still pending after 5 s")
		}
	}
}

// pushFixture serves edge.a the fixture's four names and edge.b SMN_A2 alone,
// on a database that counts its statements.
func pushFixture(t *testing.T) (*CoreDataService, *syncSends, binsFixture, *store.QueryCounter) {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	f := newBinsFixture(t, db)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	resp := &syncSends{sends: map[string]int{}}
	svc := NewCoreDataService(cdb, resp, service.EpochAnnounce{})
	svc.bins.delay = 20 * time.Millisecond
	svc.HandleBinWatch(feedsPinEnv("edge.a"), binsWatchList(t, binsFixtureNames...))
	svc.HandleBinWatch(feedsPinEnv("edge.b"), binsWatchList(t, "SMN_A2"))
	if resp.count("edge.a") != 1 || resp.count("edge.b") != 1 {
		t.Fatalf("on the lists: sends %v, want one each", resp.sends)
	}
	return svc, resp, f, counter
}

// A change at a watched bin pushes the Edges watching it, once per burst, and
// only when what they draw changed: a count that stays above zero sends
// nothing, and an Edge not watching the bin is not touched.
func TestBinsPush_AfterAChangeAtAWatchedBin(t *testing.T) {
	t.Parallel()
	svc, resp, f, counter := pushFixture(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := svc.db.DB.Exec(sql, args...)
		testutil.MustNoErr(t, err, sql)
	}

	exec(`UPDATE bins SET uop_remaining = 39 WHERE id = $1`, f.full) // a count tick
	svc.BinsChanged(f.full)
	settle(t, svc)
	if a, b := resp.count("edge.a"), resp.count("edge.b"); a != 1 || b != 1 {
		t.Errorf("a count tick: sends a=%d b=%d, want no more than the lists' one each", a, b)
	}

	exec(`UPDATE bins SET uop_remaining = 0 WHERE id = $1`, f.full) // emptied
	counter.Reset()
	for i := 0; i < 5; i++ { // one delivery emits several events
		svc.BinsChanged(f.full, f.a1)
	}
	settle(t, svc)
	if got := counter.Count(); got != 2 {
		t.Errorf("five changes for one Edge: %d statements, want one build's 2", got)
	}
	if a, b := resp.count("edge.a"), resp.count("edge.b"); a != 2 || b != 1 {
		t.Errorf("a burst at a watched bin: sends a=%d b=%d, want one more to a only", a, b)
	}

	// A bin event by node alone (an arrival names where it went).
	exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, f.slot, f.full)
	svc.BinsChanged(0, f.a1, f.slot)
	settle(t, svc)
	if a := resp.count("edge.a"); a != 3 {
		t.Errorf("a bin moved between watched nodes: a=%d, want 3", a)
	}
}

// An order to or from a watched place pushes its Edges when it starts and when
// it ends; an order elsewhere arms nothing and is not remembered.
func TestBinsPush_OrdersToAWatchedPlace(t *testing.T) {
	t.Parallel()
	svc, resp, f, _ := pushFixture(t)
	id, err := svc.db.CreateLoader(loaders.Loader{Name: "UL-1", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: "KD-1"})
	testutil.MustNoErr(t, err, "loader")
	testutil.MustNoErr(t, svc.db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: f.a2}), "window")
	svc.HandleBinWatch(feedsPinEnv("edge.b"), binsWatchList(t, "SMN_A2")) // re-read with the loader there

	svc.BinsOrderStarted(51, "ELSEWHERE")
	if _, kept := svc.bins.orders[51]; kept {
		t.Error("an order elsewhere was remembered")
	}

	// The change lands before the order that pushes it: the push is armed by
	// BinsOrderStarted and builds binsPushDelay later, so a write after the
	// start races the build (seen under a loaded gate: the build read the old
	// count and sent nothing).
	_, err = svc.db.DB.Exec(`UPDATE bins SET uop_remaining = 5 WHERE id = $1`, f.base)
	testutil.MustNoErr(t, err, "stock the base")
	before := resp.count("edge.b")
	svc.BinsOrderStarted(52, "KD-1")
	if got := svc.bins.orders[52]; !reflect.DeepEqual(got, []string{"edge.b"}) {
		t.Errorf("order to the outbound group: remembered for %v, want [edge.b]", got)
	}
	settle(t, svc)
	if got := resp.count("edge.b") - before; got != 1 {
		t.Errorf("order to the group started: %d sends to edge.b, want 1", got)
	}

	_, err = svc.db.DB.Exec(`UPDATE bins SET uop_remaining = 0 WHERE id = $1`, f.base)
	testutil.MustNoErr(t, err, "drain the base")
	svc.BinsOrderEnded(52)
	settle(t, svc)
	if got := resp.count("edge.b") - before; got != 2 {
		t.Errorf("order to the group ended: %d sends to edge.b in all, want 2", got)
	}
	if _, kept := svc.bins.orders[52]; kept {
		t.Error("an ended order is still remembered")
	}
	if a := resp.count("edge.a"); a != 1 {
		t.Errorf("edge.a, not watching the group's window: %d sends, want its list's one", a)
	}
}

// A cart's tote numbers ride its row as the HTTP answer carries them: unknown
// is absent (the golden above, where no number is set), and a set number is
// sent as it stands, 0 included.
func TestBuildNodeBins_CarriesTheToteNumbers(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	f := newBinsFixture(t, db)
	_, err := db.DB.Exec(`UPDATE bin_types SET totes_per_layer = 8 WHERE code = 'TOTE'`)
	testutil.MustNoErr(t, err, "totes per layer")
	_, err = db.DB.Exec(`UPDATE bins SET totes_on_cart = 16 WHERE id = $1`, f.full)
	testutil.MustNoErr(t, err, "totes on the full")
	_, err = db.DB.Exec(`UPDATE bins SET totes_on_cart = 0 WHERE id = $1`, f.base)
	testutil.MustNoErr(t, err, "a base")
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	svc.bins.replace("edge.a", binsWatchList(t, binsFixtureNames...))
	snap, _, err := svc.buildNodeBins("edge.a")
	testutil.MustNoErr(t, err, "build")
	got := map[string][2]string{}
	show := func(p *int) string {
		if p == nil {
			return "unknown"
		}
		return fmt.Sprint(*p)
	}
	for _, r := range snap.Rows {
		got[r.Node] = [2]string{show(r.TotesOnCart), show(r.TotesPerLayer)}
	}
	want := map[string][2]string{
		"GHOST": {"unknown", "unknown"}, "LANE-1.SLOT-1": {"unknown", "unknown"},
		"SMN_A1": {"16", "8"}, "SMN_A2": {"0", "unknown"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tote numbers by node = %v, want %v", got, want)
	}
}
