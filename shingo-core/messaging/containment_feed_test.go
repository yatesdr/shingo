//go:build docker

package messaging

import (
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// containmentFeedFixture seeds three containment destinations: QH, a group
// with two plain children, a nested group and a synthetic child (both left out
// of its members); PLAIN, a concrete node; and GHOST, a name no node carries.
// QH-A holds an occupied bin, QH-B only a retired one, PLAIN a held bin. Two
// processes name QH, so the destination read must de-duplicate.
func containmentFeedFixture(t *testing.T, db *store.DB) (b11, b12 int64) {
	t.Helper()
	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	var btID int64
	testutil.MustNoErr(t, db.DB.QueryRow(
		`INSERT INTO bin_types (code, description) VALUES ('CF-TYPE', 'synthetic') RETURNING id`,
	).Scan(&btID), "seed bin type")

	node := func(name string, parent *int64, typ *int64, synthetic bool) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: parent, NodeTypeID: typ, IsSynthetic: synthetic}
		testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
		return n
	}
	qh := node("QH", nil, &grpType.ID, true)
	a := node("QH-A", &qh.ID, nil, false)
	b := node("QH-B", &qh.ID, nil, false)
	node("QH-SUB", &qh.ID, &grpType.ID, true)
	node("QH-LANE", &qh.ID, nil, true)
	plain := node("PLAIN", nil, nil, false)

	bin := func(label string, nodeID int64, uop int) int64 {
		x := &bins.Bin{BinTypeID: btID, Label: label, NodeID: &nodeID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(x), "create bin "+label)
		testutil.MustNoErr(t, db.SetBinManifest(x.ID, `{"items":[]}`, "CF-PART", uop), "manifest "+label)
		return x.ID
	}
	b11 = bin("CF-BIN-11", a.ID, 7)
	retired := bin("CF-BIN-13", b.ID, 1)
	_, err = db.DB.Exec(`UPDATE bins SET status = 'retired' WHERE id = $1`, retired)
	testutil.MustNoErr(t, err, "retire bin")
	b12 = bin("CF-BIN-12", plain.ID, 3)

	_, err = db.DB.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code,
		 containment_destination, outbound_destination)
		VALUES ('CF-P1', 'S1', 'CF-PROD-1', 'produce', 'auto', 'CF-PART', 'QH', 'CF-FG-1'),
		       ('CF-P2', 'S1', 'CF-PROD-2', 'produce', 'auto', 'CF-PART', 'QH', 'CF-FG-2'),
		       ('CF-P3', 'S1', 'CF-PROD-3', 'produce', 'auto', 'CF-PART', 'PLAIN', 'CF-FG-3'),
		       ('CF-P4', 'S1', 'CF-PROD-4', 'produce', 'auto', 'CF-PART', 'GHOST', 'CF-FG-4'),
		       ('CF-P5', 'S1', 'CF-PROD-5', 'produce', 'auto', 'CF-PART', '', 'CF-FG-5')`)
	testutil.MustNoErr(t, err, "seed claims")
	testutil.MustNoErr(t, db.SetPayloadContainment("CF-PART", "burr", "qa", true), "flag")
	testutil.MustNoErr(t, db.SetBinQualityHold(b12, true, "edge.test"), "hold")
	return b11, b12
}

// One build is three statements whatever the number of destinations (three
// here), and its content is what the Edge's page used to read over HTTP per
// destination: a group's non-group, non-synthetic children by dotted name and
// the newest non-retired bin at each member, a concrete node's own bin, and an
// unresolved name with nothing.
func TestBuildContainmentSnapshot_ThreeStatements(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	b11, b12 := containmentFeedFixture(t, db)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	svc := NewCoreDataService(cdb, &feedsPinResponder{}, service.EpochAnnounce{})

	counter.Reset()
	snap, err := svc.buildContainmentSnapshot()
	testutil.MustNoErr(t, err, "build")
	if got := counter.Count(); got != 3 {
		t.Errorf("one build = %d statements, want 3", got)
	}

	if len(snap.Flags) != 1 || snap.Flags[0].PayloadCode != "CF-PART" || !snap.Flags[0].Active {
		t.Errorf("flags = %+v", snap.Flags)
	}
	if len(snap.HeldBins) != 1 || snap.HeldBins[0].BinID != b12 || snap.HeldBins[0].NodeName != "PLAIN" {
		t.Errorf("held bins = %+v", snap.HeldBins)
	}
	want := []protocol.ContainmentDestination{
		{Node: "GHOST", Children: []string{}, Bins: []protocol.ContainmentBin{}},
		{Node: "PLAIN", Children: []string{}, Bins: []protocol.ContainmentBin{
			{Node: "PLAIN", BinID: b12, Label: "CF-BIN-12", PayloadCode: "CF-PART", UOP: 3}}},
		{Node: "QH", Children: []string{"QH.QH-A", "QH.QH-B"}, Bins: []protocol.ContainmentBin{
			{Node: "QH.QH-A", BinID: b11, Label: "CF-BIN-11", PayloadCode: "CF-PART", UOP: 7}}},
	}
	if !reflect.DeepEqual(snap.Destinations, want) {
		t.Errorf("destinations =\n %+v\nwant\n %+v", snap.Destinations, want)
	}
	d, err := protocol.ContainmentDigest(*snap)
	testutil.MustNoErr(t, err, "digest")
	if snap.Digest == "" || snap.Digest != d {
		t.Errorf("snapshot digest %q, ContainmentDigest of the value sent %q", snap.Digest, d)
	}
}

// An empty plant builds a snapshot of empty lists, sent as [] rather than null.
func TestBuildContainmentSnapshot_Empty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	snap, err := svc.buildContainmentSnapshot()
	testutil.MustNoErr(t, err, "build")
	if snap.Flags == nil || snap.HeldBins == nil || snap.Destinations == nil {
		t.Errorf("empty snapshot has a nil list: %+v", snap)
	}
}

// A known write broadcasts the snapshot once, to every station, with the
// digest the next heartbeat compares against: an Edge that applied it hears its
// digest back and is sent nothing; an Edge that missed it is sent it alone.
// A second write after a change broadcasts the new digest, not the memo.
func TestContainmentChanged_BroadcastsThenHeartbeatsCompare(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	containmentFeedFixture(t, db)
	for _, st := range []string{"edge.a", "edge.b"} {
		_, err := db.EnrollEdge(st, "", st)
		testutil.MustNoErr(t, err, "enroll "+st)
	}
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	svc.ContainmentChanged()
	if got := resp.trace(); !reflect.DeepEqual(got, []string{"send " + protocol.SubjectContainmentSnapshot + " -> " + protocol.StationBroadcast}) {
		t.Fatalf("trace after ContainmentChanged = %v, want one broadcast", got)
	}
	first := resp.events[0].payload.(*protocol.ContainmentSnapshot)

	resp.events = nil
	svc.HandleEdgeHeartbeat(feedsPinEnv("edge.a"), &protocol.EdgeHeartbeat{StationID: "edge.a",
		Feeds: map[string]string{protocol.FeedContainment: first.Digest}})
	if got := resp.trace(); !reflect.DeepEqual(got, []string{"reply " + protocol.SubjectEdgeHeartbeatAck}) {
		t.Errorf("edge holding the broadcast: trace = %v, want the ack alone", got)
	}
	if ack := resp.events[0].payload.(*protocol.EdgeHeartbeatAck); ack.Feeds[protocol.FeedContainment] != first.Digest {
		t.Errorf("ack containment digest = %q, want %q", ack.Feeds[protocol.FeedContainment], first.Digest)
	}

	resp.events = nil
	svc.HandleEdgeHeartbeat(feedsPinEnv("edge.b"), &protocol.EdgeHeartbeat{StationID: "edge.b",
		Feeds: map[string]string{protocol.FeedContainment: ""}})
	if got := resp.trace(); !reflect.DeepEqual(got, []string{
		"send " + protocol.SubjectContainmentSnapshot + " -> edge.b", "reply " + protocol.SubjectEdgeHeartbeatAck}) &&
		!reflect.DeepEqual(got, []string{
			"reply " + protocol.SubjectEdgeHeartbeatAck, "send " + protocol.SubjectContainmentSnapshot + " -> edge.b"}) {
		t.Errorf("edge that missed it: trace = %v, want the ack and a unicast snapshot", got)
	}

	testutil.MustNoErr(t, db.SetPayloadContainment("CF-PART", "cleared", "qa", false), "clear flag")
	resp.events = nil
	svc.ContainmentChanged()
	second := resp.events[0].payload.(*protocol.ContainmentSnapshot)
	if second.Digest == first.Digest {
		t.Error("a write followed by ContainmentChanged broadcast the old digest")
	}
}

// The payload service calls the hook only after a write succeeded: a refused
// activation (no claim routes the payload) pushes nothing.
func TestPayloadService_ContainmentHook(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, b12 := containmentFeedFixture(t, db)
	ps := service.NewPayloadService(db)
	calls := 0
	ps.SetContainmentChangedFunc(func() { calls++ })

	if err := ps.SetContainment("CF-UNROUTED", "x", "qa", true); err == nil {
		t.Fatal("activation with no routed claim succeeded")
	}
	if calls != 0 {
		t.Errorf("refused write called the hook %d times", calls)
	}
	testutil.MustNoErr(t, ps.SetContainment("CF-PART", "burr", "qa", true), "set flag")
	testutil.MustNoErr(t, ps.SetBinHold(b12, false, "edge.test"), "unhold")
	if calls != 2 {
		t.Errorf("two successful writes called the hook %d times, want 2", calls)
	}
}

// Golden through the builder: one flag row with fixed values reads back from
// Postgres (in the driver's local zone) and digests to the same string the
// protocol golden pins for that value built by hand in UTC. The built value's
// time moved to another zone digests the same, so no Core's or Edge's zone can
// make a copy look changed.
func TestBuildContainmentSnapshot_GoldenDigest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	_, err := db.DB.Exec(`INSERT INTO payload_containment
		(payload_code, active, reason, activated_by, activated_at, deactivated_by, deactivated_at, updated_at)
		VALUES ('P1', TRUE, 'r', 'qa', $1, '', NULL, $1)`, at)
	testutil.MustNoErr(t, err, "seed flag")
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})

	snap, err := svc.buildContainmentSnapshot()
	testutil.MustNoErr(t, err, "build")
	// The value protocol.TestContainmentDigest_Golden pins.
	if snap.Digest != "495ee5733d2adacc" {
		t.Errorf("built digest = %s, want 495ee5733d2adacc", snap.Digest)
	}

	zoned := *snap
	local := snap.Flags[0].ActivatedAt.In(time.FixedZone("plant", -5*3600))
	zoned.Flags = []protocol.PayloadContainmentRow{snap.Flags[0]}
	zoned.Flags[0].ActivatedAt = &local
	d, err := protocol.ContainmentDigest(zoned)
	testutil.MustNoErr(t, err, "digest zoned")
	if d != snap.Digest {
		t.Errorf("same rows in another zone digest %s, want %s", d, snap.Digest)
	}
}
