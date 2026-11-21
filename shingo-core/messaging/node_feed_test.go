//go:build docker

package messaging

import (
	"math"
	"reflect"
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/payloads"
)

// nodeFeedSetup is one enrolled station with a node of its own and a scene.
func nodeFeedSetup(t *testing.T) (*CoreDataService, *feedsPinResponder) {
	t.Helper()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "")
	testutil.MustNoErr(t, err, "enroll")
	sceneRequest(t, db)
	seedFeedsPinNodes(t, db)
	resp := &feedsPinResponder{}
	return NewCoreDataService(db, resp, service.EpochAnnounce{}), resp
}

func heartbeatWith(svc *CoreDataService, resp *feedsPinResponder, feeds map[string]string) (*protocol.EdgeHeartbeatAck, []feedsPinEvent) {
	return heartbeatFrom(svc, resp, "edge.test", feeds)
}

// heartbeatFrom is one station's heartbeat naming feeds: its ack, and what
// else was sent while it was handled.
func heartbeatFrom(svc *CoreDataService, resp *feedsPinResponder, station string, feeds map[string]string) (*protocol.EdgeHeartbeatAck, []feedsPinEvent) {
	before := len(resp.events)
	svc.HandleEdgeHeartbeat(feedsPinEnv(station), &protocol.EdgeHeartbeat{StationID: station, Feeds: feeds})
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

// TestNodeFeed_AReAskedListIsNotSentBack: a write that changes a station's
// node list and drops no memo (a group deleted or reparented, which Core
// announces with NodeStructureChanged) makes the Edge re-ask the list and
// hold the fresh answer's digest. The heartbeats that followed inside the
// memo's life compared that digest with the build from before the write and
// sent the old list back, which the Edge applied, undoing the change until
// the memo aged out. A re-ask forgets the station's memo, so the next
// heartbeat compares against a build as new as the answer and sends nothing.
func TestNodeFeed_AReAskedListIsNotSentBack(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "")
	testutil.MustNoErr(t, err, "enroll")
	seedFeedsPinNodes(t, db)
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	ack, _ := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: ""})
	before := ack.Feeds[protocol.FeedNodes]
	if before == "" {
		t.Fatal("no node-list digest in the first ack")
	}
	// The Edge applied that list and quotes it back: converged.
	heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: before})

	// The write, with no memo dropped: LN-9 joins the station's list.
	ln9, err := db.GetNodeByName("LN-9")
	testutil.MustNoErr(t, err, "LN-9")
	testutil.MustNoErr(t, db.AssignNodeToStation(ln9.ID, "edge.test"), "assign LN-9")

	// The Edge re-asks, as a NodeStructureChanged makes it, and holds the answer.
	svc.HandleNodeListRequest(feedsPinEnv("edge.test"), &protocol.NodeListRequest{})
	reply := resp.events[len(resp.events)-1].payload.(*protocol.NodeListResponse)
	if reply.Digest == before {
		t.Fatalf("the re-asked list's digest %s is the old one: the write changed nothing", reply.Digest)
	}

	// Three heartbeats inside the memo's life, the Edge still holding the answer.
	for i := 1; i <= 3; i++ {
		ack, sends := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: reply.Digest})
		for _, s := range sends {
			if nl, ok := s.payload.(*protocol.NodeListResponse); ok {
				t.Errorf("heartbeat %d after the re-ask sent a node list of digest %s (the re-asked one is %s): "+
					"the build from before the write, sent back over the answer", i, nl.Digest, reply.Digest)
			}
		}
		if ack.Feeds[protocol.FeedNodes] != reply.Digest {
			t.Errorf("heartbeat %d: the ack quotes %s, want the re-asked list's %s", i, ack.Feeds[protocol.FeedNodes], reply.Digest)
		}
	}
}

// TestNodeFeed_ACatalogReplyAfterAnEdit: a part is edited, one Edge's catalog
// sync is answered with the edited catalog (a reconnect or a register asks
// it), and that Edge's next heartbeat quotes the answer's digest inside the
// plant-wide memo's life, a memo the heartbeats built before the edit. Nothing
// is sent and the ack quotes the answer, whether the edit dropped the memo or
// not:
//
//   - through the payload service, the door every runtime catalog writer goes
//     through, its hook wired as the composition root wires it, the hook drops
//     the memo (PayloadsChanged);
//   - a write that calls no hook (the store's own writer, which cmd/seeddev
//     uses) leaves the memo's build from before the edit, and the reply drops
//     it (HandleCatalogPayloadsRequest). Before it did, the heartbeat sent the
//     memo's catalog from before the edit back over the answer.
//
// Neither case touches the memo; the test reads it, to show it was warm.
func TestNodeFeed_ACatalogReplyAfterAnEdit(t *testing.T) {
	t.Parallel()
	for _, throughTheService := range []bool{true, false} {
		name := "a write that calls no hook"
		if throughTheService {
			name = "through the payload service"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			for _, st := range []string{"edge.test", "edge.other"} {
				_, err := db.EnrollEdge(st, "")
				testutil.MustNoErr(t, err, "enroll "+st)
			}
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			ps := service.NewPayloadService(db)
			ps.SetPayloadsChangedFunc(svc.PayloadsChanged)
			p := &payloads.Payload{Code: "SYN-CE-1", Description: "before", UOPCapacity: 10}
			testutil.MustNoErr(t, ps.CreateWithSets(p, service.PayloadSets{}), "create the part")

			reply := func() *protocol.CatalogPayloadsResponse {
				t.Helper()
				before := len(resp.events)
				svc.HandleCatalogPayloadsRequest(feedsPinEnv("edge.test"))
				if len(resp.events) != before+1 {
					t.Fatalf("catalog sync: %d messages, want the one reply", len(resp.events)-before)
				}
				return resp.events[before].payload.(*protocol.CatalogPayloadsResponse)
			}
			// edge.test learns the catalog from a reply, as at boot; both
			// stations' heartbeats quote it back: converged, memo built.
			old := reply()
			for _, st := range []string{"edge.other", "edge.test"} {
				if _, sends := heartbeatFrom(svc, resp, st, map[string]string{protocol.FeedCatalog: old.Digest}); len(sends) != 0 {
					t.Fatalf("%s quoting the catalog it holds was sent %d messages, want none", st, len(sends))
				}
			}
			built := clock.Now()
			if e, ok := svc.feeds.lookup("", protocol.FeedCatalog, built); !ok || e.digest != old.Digest {
				t.Fatalf("fixture: the catalog memo is %q (warm %v), want the converged %s", e.digest, ok, old.Digest)
			}

			p.Description = "after"
			if throughTheService {
				testutil.MustNoErr(t, ps.Update(p), "edit through the payload service")
			} else {
				testutil.MustNoErr(t, db.UpdatePayload(p), "edit through the store")
			}
			answer := reply()
			if answer.Digest == old.Digest {
				t.Fatalf("the edit left the catalog digest at %s", old.Digest)
			}

			ack, sends := heartbeatFrom(svc, resp, "edge.test", map[string]string{protocol.FeedCatalog: answer.Digest})
			if age := clock.Now().Sub(built); age >= feedMemoPlant {
				t.Fatalf("the heartbeat came %v after the memo was built, past its %v: the case was not exercised", age, feedMemoPlant)
			}
			for _, s := range sends {
				if cat, ok := s.payload.(*protocol.CatalogPayloadsResponse); ok && cat.Digest == old.Digest {
					t.Errorf("the heartbeat quoting the answer's catalog %s was sent the catalog from before the edit, %s",
						answer.Digest, old.Digest)
				}
			}
			if len(sends) != 0 {
				t.Errorf("the heartbeat quoting the answer's catalog %s was sent %d messages, want none", answer.Digest, len(sends))
			}
			if ack.Feeds[protocol.FeedCatalog] != answer.Digest {
				t.Errorf("the ack quotes catalog %s, want the answer's %s", ack.Feeds[protocol.FeedCatalog], answer.Digest)
			}
		})
	}
}

// TestNodeFeed_ASceneChangeThenAReAskIsNotSentTheOldScene: the scene changes
// (a sync moves a point, writing nothing that drops a scene memo), the Edge
// re-asks its node list (boot, a reconnect, NodeStructureChanged) quoting the
// scene it holds, and the reply carries the new scene with its geometry. The
// next heartbeat, quoting the reply's digests inside the station memo's life,
// is sent nothing and the ack quotes the reply's scene: the reply forgets the
// station's scene memo as well as its node-list memo. Before it did, Core sent
// the node list with the memo's old revision and old geometry, and the Edge
// applied it until the memo aged out. The node half is
// TestNodeFeed_AReAskedListIsNotSentBack. Nothing here touches the memo.
func TestNodeFeed_ASceneChangeThenAReAskIsNotSentTheOldScene(t *testing.T) {
	t.Parallel()
	svc, resp := nodeFeedSetup(t)
	ack, _ := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: "", protocol.FeedScene: ""})
	built := clock.Now()
	nodesDigest, oldScene := ack.Feeds[protocol.FeedNodes], ack.Feeds[protocol.FeedScene]
	if nodesDigest == "" || oldScene == "" || oldScene == protocol.SceneRevisionNone {
		t.Fatalf("fixture: first ack quotes nodes %q, scene %q; want both, a real scene", nodesDigest, oldScene)
	}
	held := map[string]string{protocol.FeedNodes: nodesDigest, protocol.FeedScene: oldScene}
	if _, sends := heartbeatWith(svc, resp, held); len(sends) != 0 {
		t.Fatalf("the converged heartbeat was sent %d messages, want none", len(sends))
	}

	// The sync moves LM9 a metre along x and writes the area again.
	const oldX, newX = -0.604, 0.396
	points, err := svc.db.ListScenePoints()
	testutil.MustNoErr(t, err, "read the scene points")
	edges, err := svc.db.ListSceneEdges()
	testutil.MustNoErr(t, err, "read the scene edges")
	moved := false
	for _, p := range points {
		if p.InstanceName == "LM9" && p.PosX == oldX {
			p.PosX, moved = newX, true
		}
	}
	if !moved {
		t.Fatalf("fixture: no LM9 at x=%v", oldX)
	}
	testutil.MustNoErr(t, svc.db.ReplaceAreaScene("AREA-G", points, edges), "sync the moved point")

	before := len(resp.events)
	svc.HandleNodeListRequest(feedsPinEnv("edge.test"), &protocol.NodeListRequest{SceneRevision: oldScene})
	reply := resp.events[before].payload.(*protocol.NodeListResponse)
	if reply.SceneRevision == oldScene || !hasGeometry(reply) || lm9X(reply) != newX {
		t.Fatalf("fixture: the re-ask's reply carries scene %s, geometry %v, LM9 at x=%v; want a new scene with LM9 at %v",
			reply.SceneRevision, hasGeometry(reply), lm9X(reply), newX)
	}

	ack, sends := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: reply.Digest, protocol.FeedScene: reply.SceneRevision})
	if age := clock.Now().Sub(built); age >= feedMemoStation {
		t.Fatalf("the heartbeat came %v after the station's memo was built, past its %v: the case was not exercised", age, feedMemoStation)
	}
	for _, s := range sends {
		nl, ok := s.payload.(*protocol.NodeListResponse)
		if !ok {
			t.Errorf("sent %s, want nothing", s.subject)
			continue
		}
		t.Errorf("the heartbeat quoting the reply's scene %s was sent a node list at scene %s (geometry %v, LM9 at x=%v), "+
			"want nothing", reply.SceneRevision, nl.SceneRevision, hasGeometry(nl), lm9X(nl))
	}
	if got := ack.Feeds[protocol.FeedScene]; got != reply.SceneRevision {
		t.Errorf("the ack quotes scene %s, want the reply's %s (the memo's from before the change was %s)",
			got, reply.SceneRevision, oldScene)
	}
	if got := ack.Feeds[protocol.FeedNodes]; got != reply.Digest {
		t.Errorf("the ack quotes nodes %s, want the reply's %s (the nodes did not change)", got, reply.Digest)
	}
}

// lm9X is LM9's x in a node list's geometry; NaN when it carries none.
func lm9X(nl *protocol.NodeListResponse) float64 {
	for _, p := range nl.ScenePoints {
		if p.InstanceName == "LM9" && p.PosX != nil {
			return *p.PosX
		}
	}
	return math.NaN()
}

// TestNodeFeed_TheSameSceneReSyncedIsNotSentAgain: a sync that writes the
// scene again unchanged (scenesync replaces each area: ReplaceAreaScene deletes
// its rows and inserts them afresh, with new ids and synced_at = now) keeps
// the scene's revision, which hashes what the rows say (scene.Revision), and
// the next heartbeat that builds the scene feed for an Edge holding that scene
// sends nothing. Before, the revision hashed each row's (id, synced_at), so
// every sync was a new scene and every Edge was sent its whole geometry again.
// The pure rule is pinned beside the function
// (TestRevision_IsAFunctionOfTheContentOnly).
func TestNodeFeed_TheSameSceneReSyncedIsNotSentAgain(t *testing.T) {
	t.Parallel()
	svc, resp := nodeFeedSetup(t)
	svc.HandleNodeListRequest(feedsPinEnv("edge.test"), &protocol.NodeListRequest{})
	held := resp.events[len(resp.events)-1].payload.(*protocol.NodeListResponse)
	if !hasGeometry(held) || held.SceneRevision == "" || held.SceneRevision == protocol.SceneRevisionNone {
		t.Fatalf("fixture: the boot reply carries scene %q, geometry %v; want a real scene with geometry",
			held.SceneRevision, hasGeometry(held))
	}

	points, err := svc.db.ListScenePoints()
	testutil.MustNoErr(t, err, "read the scene points")
	edges, err := svc.db.ListSceneEdges()
	testutil.MustNoErr(t, err, "read the scene edges")
	testutil.MustNoErr(t, svc.db.ReplaceAreaScene("AREA-G", points, edges), "sync the same scene again")

	ack, sends := heartbeatWith(svc, resp, map[string]string{protocol.FeedNodes: held.Digest, protocol.FeedScene: held.SceneRevision})
	if got := ack.Feeds[protocol.FeedScene]; got != held.SceneRevision {
		t.Errorf("the same scene re-synced has a new revision, %s (was %s)", got, held.SceneRevision)
	}
	for _, s := range sends {
		if nl, ok := s.payload.(*protocol.NodeListResponse); ok {
			t.Errorf("the scene's geometry (%d points, %d edges) was sent again to an Edge holding it",
				len(nl.ScenePoints), len(nl.SceneEdges))
			continue
		}
		t.Errorf("sent %s, want nothing", s.subject)
	}
	if got := ack.Feeds[protocol.FeedNodes]; got != held.Digest {
		t.Errorf("the ack quotes nodes %s, want the reply's %s (the nodes did not change)", got, held.Digest)
	}
}

// The feed path is strict: a failed read leaves the key out of the ack and
// sends nothing, where the reply path still answers with what it could read.
func TestNodeFeed_FailedReadLeavesTheKeyOut(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "")
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

// A loader that names an Edge goes down with that Edge's display name, filled
// before the digest: picking another Edge moves the digest, and so does
// renaming the one picked. An Edge with no display name sends none (the Edge
// falls back to the uid). The feed path fails on a failed registry read; a
// reply still answers, with the uid and no names. A build whose loaders name
// no Edge does not read the registry at all.
func TestNodeFeed_LoaderNamesItsEdge(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "")
	testutil.MustNoErr(t, err, "enroll")
	seedFeedsPinNodes(t, db)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	_, err = db.EnrollEdge("stn-nf-press", "PRESS EDGE")
	testutil.MustNoErr(t, err, "enroll the press Edge")
	_, err = db.IntroduceEdge("stn-nf-cell", "cell-host", "test")
	testutil.MustNoErr(t, err, "introduce an unnamed Edge")
	id, err := db.CreateLoader(loaders.Loader{Name: "NF-LDR", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator})
	testutil.MustNoErr(t, err, "a loader")

	build := func(strict bool) (protocol.LoaderInfo, string) {
		t.Helper()
		nl, err := svc.buildNodeList("edge.test", strict)
		testutil.MustNoErr(t, err, "build the node list")
		for _, li := range nl.Loaders {
			if li.LoaderKey == loaders.Key(id) {
				return li, nl.Digest
			}
		}
		t.Fatal("NF-LDR missing from the node list")
		return protocol.LoaderInfo{}, ""
	}
	pick := func(uid string) {
		t.Helper()
		l, err := db.GetLoader(id)
		testutil.MustNoErr(t, err, "read the loader")
		l.StationUID = uid
		testutil.MustNoErr(t, db.UpdateLoader(*l), "pick "+uid)
	}
	_, none := build(true)

	pick("stn-nf-press")
	li, picked := build(true)
	if li.EdgeStationUID != "stn-nf-press" || li.EdgeDisplayName != "PRESS EDGE" {
		t.Errorf("picked: uid %q name %q; want stn-nf-press, PRESS EDGE", li.EdgeStationUID, li.EdgeDisplayName)
	}
	if picked == none {
		t.Error("picking an Edge left the digest where it was")
	}

	_, err = db.RenameEdge("stn-nf-press", "PRESS EDGE 2")
	testutil.MustNoErr(t, err, "rename the press Edge")
	li, renamed := build(true)
	if li.EdgeDisplayName != "PRESS EDGE 2" || renamed == picked {
		t.Errorf("renamed: name %q, digest moved %v; want PRESS EDGE 2 and a new digest", li.EdgeDisplayName, renamed != picked)
	}

	pick("stn-nf-cell")
	li, other := build(true)
	if li.EdgeStationUID != "stn-nf-cell" || li.EdgeDisplayName != "" || other == renamed {
		t.Errorf("an unnamed Edge: uid %q name %q, digest moved %v; want stn-nf-cell, no name, a new digest",
			li.EdgeStationUID, li.EdgeDisplayName, other != renamed)
	}

	pick("stn-nf-press")
	hideTable(t, db, "edge_registry")
	if _, err := svc.buildNodeList("edge.test", true); err == nil {
		t.Error("feed path: built a node list with the registry unread; want the build to fail")
	}
	if li, _ := build(false); li.EdgeStationUID != "stn-nf-press" || li.EdgeDisplayName != "" {
		t.Errorf("reply path, registry unread: uid %q name %q; want the uid and no name", li.EdgeStationUID, li.EdgeDisplayName)
	}
	pick("")
	if _, err := svc.buildNodeList("edge.test", true); err != nil {
		t.Errorf("no loader names an Edge, registry unread: the feed build failed (%v); want no registry read", err)
	}
}

// TestPayloadsChanged_DropsTheMemos: a payload write through the payload
// service, wired as the composition root wires it, drops the catalog memo and
// every station's node-list memo, and a heartbeat right after it compares
// against a build made after the write. An Edge whose catalog digest differs
// from the build before the write (one that restarted, holding nothing) is
// sent the catalog with the new row, not that older build, and its node list
// carries the part's carrier row; an Edge holding the old digest is sent the
// new catalog at once rather than when the memo ages out.
func TestPayloadsChanged_DropsTheMemos(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	stations := []string{"edge.test", "edge.other"}
	for _, st := range stations {
		_, err := db.EnrollEdge(st, "")
		testutil.MustNoErr(t, err, "enroll "+st)
	}
	seedFeedsPinNodes(t, db)
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	ps := service.NewPayloadService(db)
	ps.SetPayloadsChangedFunc(svc.PayloadsChanged)
	bt := &bins.BinType{Code: "SYN-PC-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	heartbeat := func(station string, feeds map[string]string) (*protocol.EdgeHeartbeatAck, *protocol.CatalogPayloadsResponse, *protocol.NodeListResponse) {
		t.Helper()
		before := len(resp.events)
		svc.HandleEdgeHeartbeat(feedsPinEnv(station), &protocol.EdgeHeartbeat{StationID: station, Feeds: feeds})
		var ack *protocol.EdgeHeartbeatAck
		var cat *protocol.CatalogPayloadsResponse
		var nl *protocol.NodeListResponse
		for _, e := range resp.events[before:] {
			switch p := e.payload.(type) {
			case *protocol.EdgeHeartbeatAck:
				ack = p
			case *protocol.CatalogPayloadsResponse:
				cat = p
			case *protocol.NodeListResponse:
				nl = p
			}
		}
		if ack == nil {
			t.Fatalf("%s: no ack", station)
		}
		return ack, cat, nl
	}
	held := map[string]map[string]string{}
	for _, st := range stations {
		ack, _, _ := heartbeat(st, map[string]string{protocol.FeedNodes: "", protocol.FeedCatalog: ""})
		held[st] = ack.Feeds
	}
	now := clock.Now()
	if _, ok := svc.feeds.lookup("", protocol.FeedCatalog, now); !ok {
		t.Fatal("no catalog memo after the heartbeats")
	}
	for _, st := range stations {
		if _, ok := svc.feeds.lookup(st, protocol.FeedNodes, now); !ok {
			t.Fatalf("no node-list memo for %s after its heartbeat", st)
		}
	}

	p := &payloads.Payload{Code: "SYN-PC-NEW", UOPCapacity: 10}
	testutil.MustNoErr(t, ps.CreateWithSets(p, service.PayloadSets{BinTypeIDs: &[]int64{bt.ID}}), "create the part")

	now = clock.Now()
	if _, ok := svc.feeds.lookup("", protocol.FeedCatalog, now); ok {
		t.Error("the catalog memo survived a payload create")
	}
	for _, st := range stations {
		if _, ok := svc.feeds.lookup(st, protocol.FeedNodes, now); ok {
			t.Errorf("%s's node-list memo survived a payload create", st)
		}
	}

	hasRow := func(cat *protocol.CatalogPayloadsResponse) bool {
		for _, r := range cat.Payloads {
			if r.Code == "SYN-PC-NEW" {
				return true
			}
		}
		return false
	}
	ack, cat, nl := heartbeat("edge.other", map[string]string{protocol.FeedCatalog: "", protocol.FeedNodes: ""})
	if cat == nil || !hasRow(cat) {
		t.Errorf("an Edge holding nothing was sent a catalog without the new part: %+v", cat)
	}
	if ack.Feeds[protocol.FeedCatalog] == held["edge.other"][protocol.FeedCatalog] {
		t.Errorf("the ack quotes the catalog digest from before the create (%s)", ack.Feeds[protocol.FeedCatalog])
	}
	want := protocol.PayloadBinTypeInfo{PayloadCode: "SYN-PC-NEW", BinTypeCode: "SYN-PC-BT"}
	if nl == nil || !slices.Contains(nl.PayloadBinTypes, want) {
		t.Errorf("the node list after the create does not carry %+v", want)
	}

	_, cat, _ = heartbeat("edge.test", held["edge.test"])
	if cat == nil || !hasRow(cat) {
		t.Errorf("an Edge holding the old digest was not sent the catalog with the new part: %+v", cat)
	}
}

// TestCatalogRow_IsTheFeedsRow: one payload's catalog row is the row the
// catalog feed builds for it, CATID and manifest line included, and its carrier
// rows are the node list's rows for that payload and no other's, so an Edge
// that writes them from the part create's answer holds what the next feeds
// carry, and their conditional upsert writes nothing new.
func TestCatalogRow_IsTheFeedsRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, err := db.EnrollEdge("edge.test", "")
	testutil.MustNoErr(t, err, "enroll")
	seedFeedsPinNodes(t, db)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	ps := service.NewPayloadService(db)
	var ids []int64
	for _, code := range []string{"SYN-CR-BT-A", "SYN-CR-BT-B", "SYN-CR-BT-C"} {
		bt := &bins.BinType{Code: code}
		testutil.MustNoErr(t, db.CreateBinType(bt), "create "+code)
		ids = append(ids, bt.ID)
	}
	testutil.MustNoErr(t, ps.CreateWithSets(&payloads.Payload{Code: "SYN-CR-1", Description: "one", UOPCapacity: 40},
		service.PayloadSets{BinTypeIDs: &[]int64{ids[0], ids[1]},
			Manifest: &[]service.ManifestLine{{PartNumber: "SYN-CR-1", CATID: "SYN-CR-C1", PartsPerCycle: 1}}}), "create SYN-CR-1")
	testutil.MustNoErr(t, ps.CreateWithSets(&payloads.Payload{Code: "SYN-CR-2", UOPCapacity: 10},
		service.PayloadSets{BinTypeIDs: &[]int64{ids[2]}}), "create SYN-CR-2")

	row, binTypes, err := svc.CatalogRow("SYN-CR-1")
	testutil.MustNoErr(t, err, "CatalogRow")
	cat, err := svc.buildCatalog(true)
	testutil.MustNoErr(t, err, "the catalog feed")
	var feedRow *protocol.CatalogPayloadInfo
	for i := range cat.Payloads {
		if cat.Payloads[i].Code == "SYN-CR-1" {
			feedRow = &cat.Payloads[i]
		}
	}
	if feedRow == nil || !reflect.DeepEqual(row, *feedRow) {
		t.Errorf("CatalogRow = %+v, the catalog feed's row %+v", row, feedRow)
	}
	if row.CATID != "SYN-CR-C1" || len(row.Manifest) != 1 || row.Manifest[0].PartsPerCycle != 1 {
		t.Errorf("CatalogRow lost the CATID or the manifest line: %+v", row)
	}
	nl, err := svc.buildNodeList("edge.test", true)
	testutil.MustNoErr(t, err, "the node list")
	var feedBinTypes []protocol.PayloadBinTypeInfo
	for _, r := range nl.PayloadBinTypes {
		if r.PayloadCode == "SYN-CR-1" {
			feedBinTypes = append(feedBinTypes, r)
		}
	}
	if len(binTypes) != 2 || !reflect.DeepEqual(binTypes, feedBinTypes) {
		t.Errorf("carrier rows = %+v, the node list's for SYN-CR-1 %+v", binTypes, feedBinTypes)
	}
}
