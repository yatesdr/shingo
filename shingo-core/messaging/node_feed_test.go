//go:build docker

package messaging

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
)

// nodeFeedSetup is one enrolled station with a node of its own and a scene.
func nodeFeedSetup(t *testing.T) (*CoreDataService, *feedsPinResponder) {
	t.Helper()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "", "edge.test")
	testutil.MustNoErr(t, err, "enroll")
	sceneRequest(t, db)
	seedFeedsPinNodes(t, db)
	resp := &feedsPinResponder{}
	return NewCoreDataService(db, resp, service.EpochAnnounce{}), resp
}

func heartbeatWith(svc *CoreDataService, resp *feedsPinResponder, feeds map[string]string) (*protocol.EdgeHeartbeatAck, []feedsPinEvent) {
	before := len(resp.events)
	svc.HandleEdgeHeartbeat(feedsPinEnv("edge.test"), &protocol.EdgeHeartbeat{StationID: "edge.test", Feeds: feeds})
	var ack *protocol.EdgeHeartbeatAck
	var sends []feedsPinEvent
	for _, e := range resp.events[before:] {
		if a, ok := e.payload.(*protocol.EdgeHeartbeatAck); ok {
			ack = a
			continue
		}
		sends = append(sends, e)
	}
	return ack, sends
}

// An Edge holding nothing gets one node list (with geometry) and one catalog,
// each carrying the digest the ack answers; the next heartbeat quoting those
// digests back gets the ack and nothing else.
func TestNodeFeed_SendsWhatDiffersThenNothing(t *testing.T) {
	t.Parallel()
	svc, resp := nodeFeedSetup(t)
	empty := map[string]string{protocol.FeedNodes: "", protocol.FeedScene: "", protocol.FeedCatalog: ""}

	ack, sends := heartbeatWith(svc, resp, empty)
	if ack == nil {
		t.Fatal("no ack")
	}
	var subjects []string
	var nl *protocol.NodeListResponse
	var cat *protocol.CatalogPayloadsResponse
	for _, s := range sends {
		subjects = append(subjects, s.subject+" -> "+s.station)
		switch p := s.payload.(type) {
		case *protocol.NodeListResponse:
			nl = p
		case *protocol.CatalogPayloadsResponse:
			cat = p
		}
	}
	want := []string{
		protocol.SubjectCatalogPayloadsResponse + " -> edge.test",
		protocol.SubjectNodeListResponse + " -> edge.test",
	}
	if !reflect.DeepEqual(subjects, want) {
		t.Fatalf("sends = %v, want %v (one node list carries nodes and scene)", subjects, want)
	}
	if nl.Digest == "" || nl.Digest != ack.Feeds[protocol.FeedNodes] {
		t.Errorf("node list digest %q, ack nodes %q", nl.Digest, ack.Feeds[protocol.FeedNodes])
	}
	if nl.SceneRevision == "" || nl.SceneRevision != ack.Feeds[protocol.FeedScene] || !hasGeometry(nl) {
		t.Errorf("node list scene revision %q (geometry %v), ack scene %q", nl.SceneRevision, hasGeometry(nl), ack.Feeds[protocol.FeedScene])
	}
	if cat.Digest == "" || cat.Digest != ack.Feeds[protocol.FeedCatalog] {
		t.Errorf("catalog digest %q, ack catalog %q", cat.Digest, ack.Feeds[protocol.FeedCatalog])
	}

	held := map[string]string{
		protocol.FeedNodes: nl.Digest, protocol.FeedScene: nl.SceneRevision, protocol.FeedCatalog: cat.Digest,
	}
	ack2, sends2 := heartbeatWith(svc, resp, held)
	if len(sends2) != 0 {
		t.Errorf("converged heartbeat sent %d messages, want none", len(sends2))
	}
	if !reflect.DeepEqual(ack2.Feeds, held) {
		t.Errorf("converged ack feeds = %v, want %v", ack2.Feeds, held)
	}
}

// The map is deleted on Core while the Edge holds it: one node list carries
// SceneRevisionNone and no geometry, the Edge holds that, and from then on the
// scene converges — no further send, no flag. Before, Core's revision of an
// empty scene was one no Edge could hold, so it resent every guard window and
// flagged the station.
func TestNodeFeed_MapDeletedOneSendThenConverged(t *testing.T) {
	t.Parallel()
	svc, resp := nodeFeedSetup(t)
	ack, _ := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: "", protocol.FeedScene: ""})
	held := map[string]string{protocol.FeedNodes: ack.Feeds[protocol.FeedNodes], protocol.FeedScene: ack.Feeds[protocol.FeedScene]}
	if held[protocol.FeedScene] == "" || held[protocol.FeedScene] == protocol.SceneRevisionNone {
		t.Fatalf("fixture scene revision %q, want a real one", held[protocol.FeedScene])
	}

	for _, stmt := range []string{`DELETE FROM scene_edges`, `DELETE FROM scene_points`} {
		_, err := svc.db.Exec(stmt)
		testutil.MustNoErr(t, err, stmt)
	}
	// The station memo and the guard would hold this for two minutes; start
	// from what a later heartbeat sees.
	svc.feeds = newFeedState()

	ack, sends := heartbeatWith(svc, resp, held)
	if ack.Feeds[protocol.FeedScene] != protocol.SceneRevisionNone {
		t.Fatalf("ack scene = %q, want %q", ack.Feeds[protocol.FeedScene], protocol.SceneRevisionNone)
	}
	if len(sends) != 1 {
		t.Fatalf("sends after the delete = %d, want the one node list", len(sends))
	}
	nl := sends[0].payload.(*protocol.NodeListResponse)
	if nl.SceneRevision != protocol.SceneRevisionNone || len(nl.ScenePoints) != 0 || len(nl.SceneEdges) != 0 {
		t.Errorf("node list scene = %q with %d points, %d edges; want %q and none",
			nl.SceneRevision, len(nl.ScenePoints), len(nl.SceneEdges), protocol.SceneRevisionNone)
	}

	held[protocol.FeedScene] = protocol.SceneRevisionNone
	for i := 0; i < 3; i++ {
		if _, sends := heartbeatWith(svc, resp, held); len(sends) != 0 {
			t.Errorf("heartbeat %d holding no scene sent %d messages, want none", i+1, len(sends))
		}
	}
	if flags := svc.EdgeFeedFlags("edge.test"); len(flags) != 0 {
		t.Errorf("flags = %v, want none", flags)
	}
}

// Nodes differ, scene held: the node list goes with the scene's names and no
// geometry, the request path's rule with the revision taken from the heartbeat.
func TestNodeFeed_NodesDifferSceneHeld(t *testing.T) {
	t.Parallel()
	svc, resp := nodeFeedSetup(t)
	ack, _ := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: "", protocol.FeedScene: ""})
	rev := ack.Feeds[protocol.FeedScene]

	// Forget the first send, which the resend guard would otherwise hold back
	// for two minutes.
	svc.feeds = newFeedState()
	_, sends := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: "stale", protocol.FeedScene: rev})
	if len(sends) != 1 {
		t.Fatalf("sends = %d, want the node list alone", len(sends))
	}
	nl := sends[0].payload.(*protocol.NodeListResponse)
	if hasGeometry(nl) || len(nl.ScenePoints) == 0 || nl.SceneRevision != rev {
		t.Errorf("node list: geometry %v, %d scene names, revision %q; want names only at %q",
			hasGeometry(nl), len(nl.ScenePoints), nl.SceneRevision, rev)
	}
}

// The reply path and the feed path digest the same value: what a node-list
// request answers is what the heartbeat's ack quotes.
func TestNodeFeed_ReplyDigestIsTheFeedDigest(t *testing.T) {
	t.Parallel()
	svc, resp := nodeFeedSetup(t)
	svc.HandleNodeListRequest(feedsPinEnv("edge.test"), &protocol.NodeListRequest{})
	reply := resp.events[len(resp.events)-1].payload.(*protocol.NodeListResponse)
	svc.HandleCatalogPayloadsRequest(feedsPinEnv("edge.test"))
	catReply := resp.events[len(resp.events)-1].payload.(*protocol.CatalogPayloadsResponse)

	ack, sends := heartbeatWith(svc, resp, map[string]string{
		protocol.FeedNodes: reply.Digest, protocol.FeedCatalog: catReply.Digest,
	})
	if ack.Feeds[protocol.FeedNodes] != reply.Digest || ack.Feeds[protocol.FeedCatalog] != catReply.Digest {
		t.Errorf("ack %v, replies nodes %q catalog %q", ack.Feeds, reply.Digest, catReply.Digest)
	}
	if len(sends) != 0 {
		t.Errorf("sends = %d, want none", len(sends))
	}
}

// The feed path is strict: a failed read leaves the key out of the ack and
// sends nothing, where the reply path still answers with what it could read.
func TestNodeFeed_FailedReadLeavesTheKeyOut(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "", "edge.test")
	testutil.MustNoErr(t, err, "enroll")
	seedFeedsPinNodes(t, db)
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	hideTable(t, db, "payload_bin_types")

	ack, sends := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: ""})
	if _, ok := ack.Feeds[protocol.FeedNodes]; ok || len(sends) != 0 {
		t.Errorf("ack feeds %v with %d sends; want nodes left out and nothing sent", ack.Feeds, len(sends))
	}
	before := len(resp.events)
	svc.HandleNodeListRequest(feedsPinEnv("edge.test"), &protocol.NodeListRequest{})
	if len(resp.events) != before+1 {
		t.Error("reply path: no node list sent on a bin-type read failure")
	}
}
