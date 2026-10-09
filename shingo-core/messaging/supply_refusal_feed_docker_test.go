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
)

// supply_refusal_feed_docker_test.go — the refusals feed on Core: the digest of
// the open set on the ack, the snapshot to an Edge whose digest differs, and
// the memo dropped by a stored refusal message.

func refusalFeedHeartbeat(svc *CoreDataService, station string, feeds map[string]string) {
	svc.HandleEdgeHeartbeat(feedsPinEnv(station), &protocol.EdgeHeartbeat{StationID: station, Feeds: feeds})
}

// lastAck returns the ack in a recorded trace.
func lastAck(t *testing.T, resp *feedsPinResponder) *protocol.EdgeHeartbeatAck {
	t.Helper()
	for i := len(resp.events) - 1; i >= 0; i-- {
		if ack, ok := resp.events[i].payload.(*protocol.EdgeHeartbeatAck); ok {
			return ack
		}
	}
	t.Fatal("no heartbeat ack recorded")
	return nil
}

func TestRefusalsFeed_BuildAndSend(t *testing.T) {
	t.Parallel()
	const st = "edge.test"
	snapSend := "send " + protocol.SubjectSupplyRefusalSnapshot + " -> " + st
	ackReply := "reply " + protocol.SubjectEdgeHeartbeatAck
	refusedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	opened := &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "PC-A",
		RefusedAt: refusedAt, RefusedBy: "edge.test"}
	emptyDigest, err := protocol.RefusalsDigest(nil)
	testutil.MustNoErr(t, err, "empty digest")

	cases := []struct {
		name       string
		seed       bool   // store one open refusal first
		held       string // the Edge's refusals digest; "match" quotes Core's
		hide       bool   // Core cannot read supply_refusals
		wantTrace  []string
		wantInAck  bool
		wantOpenNo int
	}{
		{name: "empty Core, Edge holds nothing yet: snapshot of nothing", held: "",
			wantTrace: []string{snapSend, ackReply}, wantInAck: true, wantOpenNo: 0},
		{name: "one open refusal, Edge quotes the empty digest: snapshot", seed: true, held: emptyDigest,
			wantTrace: []string{snapSend, ackReply}, wantInAck: true, wantOpenNo: 1},
		{name: "Edge quotes Core's digest: nothing sent", seed: true, held: "match",
			wantTrace: []string{ackReply}, wantInAck: true},
		{name: "Core cannot read: key absent, nothing sent", held: "x", hide: true,
			wantTrace: []string{ackReply}, wantInAck: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			_, err := db.EnrollEdge(st, "", st)
			testutil.MustNoErr(t, err, "enroll")
			if tc.seed {
				testutil.MustNoErr(t, db.ApplySupplyRefusal(*opened, st), "seed")
			}
			if tc.hide {
				hideTable(t, db, "supply_refusals")
			}
			held := tc.held
			if held == "match" {
				open, err := db.ListOpenSupplyRefusals()
				testutil.MustNoErr(t, err, "list")
				held, err = protocol.RefusalsDigest(open)
				testutil.MustNoErr(t, err, "digest")
			}
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			refusalFeedHeartbeat(svc, st, map[string]string{protocol.FeedRefusals: held})

			if got := resp.trace(); !reflect.DeepEqual(got, tc.wantTrace) {
				t.Fatalf("trace = %v, want %v", got, tc.wantTrace)
			}
			ack := lastAck(t, resp)
			digest, inAck := ack.Feeds[protocol.FeedRefusals]
			if inAck != tc.wantInAck {
				t.Fatalf("refusals in ack = %v (%q), want %v", inAck, digest, tc.wantInAck)
			}
			if tc.wantTrace[0] != snapSend {
				return
			}
			snap, ok := resp.events[0].payload.(protocol.SupplyRefusalSnapshot)
			if !ok {
				t.Fatalf("snapshot payload is %T", resp.events[0].payload)
			}
			// The value sent is the value digested, and the ack quotes the same digest.
			recomputed, err := protocol.RefusalsDigest(snap.Open)
			testutil.MustNoErr(t, err, "recompute")
			if snap.Digest != digest || recomputed != digest {
				t.Errorf("snapshot digest %q, recomputed %q, ack %q: want all equal", snap.Digest, recomputed, digest)
			}
			if len(snap.Open) != tc.wantOpenNo {
				t.Errorf("snapshot open = %d rows, want %d", len(snap.Open), tc.wantOpenNo)
			}
		})
	}
}

// A stored refusal message drops the memo, so the very next heartbeat answers
// with the new set rather than one memoised up to ten seconds earlier.
func TestRefusalsFeed_StoredMessageDropsTheMemo(t *testing.T) {
	t.Parallel()
	const st = "edge.test"
	db := testdb.Open(t)
	_, err := db.EnrollEdge(st, "", st)
	testutil.MustNoErr(t, err, "enroll")
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	refusalFeedHeartbeat(svc, st, map[string]string{protocol.FeedRefusals: ""})
	before := lastAck(t, resp).Feeds[protocol.FeedRefusals]

	svc.HandleSupplyRefusal(feedsPinEnv(st), &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened,
		LoaderNode: "LN-1", PayloadCode: "PC-A", RefusedAt: time.Now().UTC(), RefusedBy: st})
	resp.events = nil
	refusalFeedHeartbeat(svc, st, map[string]string{protocol.FeedRefusals: before})

	after := lastAck(t, resp).Feeds[protocol.FeedRefusals]
	if after == before {
		t.Fatalf("refusals digest unchanged (%q) after a stored refusal: the memo was not dropped", after)
	}
	if got := resp.trace(); len(got) != 2 || got[0] != "send "+protocol.SubjectSupplyRefusalSnapshot+" -> "+st {
		t.Errorf("trace = %v, want the snapshot then the ack", got)
	}
}

// An older Edge names no feeds: Core never reads its refusals for it and never
// sends it a snapshot, whatever Core holds.
func TestRefusalsFeed_OlderEdgeGetsNoSnapshot(t *testing.T) {
	t.Parallel()
	const st = "edge.test"
	db := testdb.Open(t)
	_, err := db.EnrollEdge(st, "", st)
	testutil.MustNoErr(t, err, "enroll")
	testutil.MustNoErr(t, db.ApplySupplyRefusal(protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened,
		LoaderNode: "LN-1", PayloadCode: "PC-A", RefusedAt: time.Now().UTC(), RefusedBy: st}, st), "seed")
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	refusalFeedHeartbeat(svc, st, nil)
	if got := resp.trace(); !reflect.DeepEqual(got, []string{"reply " + protocol.SubjectEdgeHeartbeatAck}) {
		t.Errorf("trace = %v, want the ack alone", got)
	}
}
