//go:build docker

package messaging

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// core_data_service_feedspin_test.go — behaviour pins for three Core request
// handlers: the heartbeat (HandleEdgeHeartbeat), the node-list sync
// (HandleNodeListRequest) and the catalog sync (HandleCatalogPayloadsRequest).
//
// They were written before the versioned-feeds change, each case carrying the
// value it predicted beside the value it asserted. Those changes have landed —
// the feeds and claims keys on the ack in "feat(core,edge): one feed digest
// rule for Core and Edge, the wire and the plumbing", the reply digests in
// "perf(core,edge): node list, scene and catalog sent when the digest differs,
// not every two minutes", and the ack on a registry failure and the node list
// with no plant-wide fallback in "fix(core,edge): ack on a registry failure,
// no plant-wide list on a read error, reconcile after an ack gap, uptime and
// order count off the heartbeat" — so every case asserts that behaviour alone.
//
// The recorder and helpers below (feedsPinResponder, jsonKeys, feedsPinEnv,
// hideTable, seedFeedsPinNodes) are shared with the package's feeds tests.
// The last section pins every feed's golden digest as the heartbeat answers
// and sends it (TestFeeds_GoldenDigestPerFeed).

// feedsPinResponder records replyData AND sendData in one ordered trace. The
// shared captureResponder drops sendData, and the heartbeat's register request
// goes out on that seam, so the pins need a recorder that sees both.
type feedsPinResponder struct {
	events []feedsPinEvent
}

type feedsPinEvent struct {
	kind    string // "reply" or "send"
	subject string
	station string // sendData's addressee; empty on a reply
	payload any
}

func (r *feedsPinResponder) dbg(format string, args ...any) {}
func (r *feedsPinResponder) replyData(env *protocol.Envelope, subject string, payload any) {
	r.events = append(r.events, feedsPinEvent{kind: "reply", subject: subject, payload: payload})
}
func (r *feedsPinResponder) sendData(subject, stationID string, payload any) {
	r.events = append(r.events, feedsPinEvent{kind: "send", subject: subject, station: stationID, payload: payload})
}

// trace renders the recorded calls as "reply <subject>" / "send <subject> -> <station>".
func (r *feedsPinResponder) trace() []string {
	out := []string{}
	for _, e := range r.events {
		if e.kind == "send" {
			out = append(out, "send "+e.subject+" -> "+e.station)
			continue
		}
		out = append(out, "reply "+e.subject)
	}
	return out
}

// jsonKeys is the sorted top-level key set of v as it goes on the wire.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "marshal")
	var m map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal(b, &m), "unmarshal to key map")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func feedsPinEnv(station string) *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: station},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
}

// hideTable renames a table for the rest of the test so the reads against it
// fail, and puts it back on cleanup. The database is the test's own.
func hideTable(t *testing.T, db *store.DB, table string) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_hidden`); err != nil {
		t.Fatalf("hide %s: %v", table, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`ALTER TABLE ` + table + `_hidden RENAME TO ` + table); err != nil {
			t.Errorf("restore %s: %v", table, err)
		}
	})
}

// ── HandleEdgeHeartbeat ────────────────────────────────────────────────────

// TestFeedsPin_HandleEdgeHeartbeat pins the heartbeat handler: the registry
// write, the ack and its key set, the register request for an unenrolled
// station, and the registry-write failure, which still acks and sends no
// register request (no request goes out on a row Core could not check).
//
// Every heartbeat here has no Feeds field, which is what an older Edge sends.
// Its ack carries feeds as {} and claims as null (claimsFor is nil for an Edge
// that does not speak feeds); bin_watch and asks are left off; timezone is
// there only when a plant zone is configured; display_name only when the
// registry write found the station's row (EdgeHeartbeatAck.DisplayName); owned
// is null, as to every Edge that sends no owned map.
func TestFeedsPin_HandleEdgeHeartbeat(t *testing.T) {
	t.Parallel()

	const st = "edge.test"
	ack := "reply " + protocol.SubjectEdgeHeartbeatAck
	regReq := "send " + protocol.SubjectEdgeRegisterRequest + " -> " + st

	cases := []struct {
		name          string
		enroll        bool
		breakRegistry bool
		plantTZ       string

		wantTrace   []string
		wantAckKeys []string // nil when no ack is sent
	}{
		{
			name: "enrolled station, plant zone configured", enroll: true, plantTZ: "America/Chicago",
			wantTrace:   []string{ack},
			wantAckKeys: []string{"claims", "display_name", "feeds", "owned", "server_ts", "station_id", "timezone"}, // feeds={}, claims=null
		},
		{
			name: "enrolled station, plant zone unset", enroll: true, plantTZ: "",
			wantTrace:   []string{ack},
			wantAckKeys: []string{"claims", "display_name", "feeds", "owned", "server_ts", "station_id"},
		},
		{
			name: "unenrolled station gets the ack and a register request", enroll: false, plantTZ: "America/Chicago",
			wantTrace:   []string{ack, regReq},
			wantAckKeys: []string{"claims", "feeds", "owned", "server_ts", "station_id", "timezone"},
		},
		{
			// Core acks anyway, with only the feed keys it could read (none
			// for an older Edge) and claims null; still no register request.
			// Before the registry-failure ack (see the file header) nothing
			// was sent at all.
			name: "registry write fails: ack, no register request", enroll: true, breakRegistry: true, plantTZ: "America/Chicago",
			wantTrace:   []string{ack},
			wantAckKeys: []string{"claims", "feeds", "owned", "server_ts", "station_id", "timezone"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			if tc.enroll {
				_, err := db.EnrollEdge(st, "")
				testutil.MustNoErr(t, err, "enroll")
			}
			if tc.breakRegistry {
				hideTable(t, db, "edge_registry")
			}
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.SetPlantTimezone(tc.plantTZ)

			pending := int64(4)
			before := time.Now().UTC().Add(-time.Second)
			svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{
				StationID: st, Timezone: "America/New_York", TickPending: &pending,
			})
			afterCall := time.Now().UTC().Add(time.Second)

			if got := resp.trace(); !reflect.DeepEqual(got, tc.wantTrace) {
				t.Fatalf("trace = %v, want %v", got, tc.wantTrace)
			}

			var gotAck *protocol.EdgeHeartbeatAck
			for _, e := range resp.events {
				switch p := e.payload.(type) {
				case *protocol.EdgeHeartbeatAck:
					gotAck = p
				case *protocol.EdgeRegisterRequest:
					if p.StationID != st || p.Reason != "station not enrolled" {
						t.Errorf("register request = %+v, want station %s, reason \"station not enrolled\"", p, st)
					}
				}
			}
			if tc.wantAckKeys == nil {
				if gotAck != nil {
					t.Errorf("an ack was sent: %+v, want none", gotAck)
				}
				return
			}
			if gotAck == nil {
				t.Fatal("no ack payload recorded")
			}
			if keys := jsonKeys(t, gotAck); !reflect.DeepEqual(keys, tc.wantAckKeys) {
				t.Errorf("ack keys = %v, want %v", keys, tc.wantAckKeys)
			}
			if gotAck.StationID != st {
				t.Errorf("ack station_id = %q, want %q", gotAck.StationID, st)
			}
			if gotAck.Timezone != tc.plantTZ {
				t.Errorf("ack timezone = %q, want the configured plant zone %q", gotAck.Timezone, tc.plantTZ)
			}
			if gotAck.ServerTS.Before(before) || gotAck.ServerTS.After(afterCall) || gotAck.ServerTS.Location() != time.UTC {
				t.Errorf("ack server_ts = %v, want UTC now", gotAck.ServerTS)
			}

			if !tc.enroll || tc.breakRegistry {
				return
			}
			// The registry write: status promoted, the Edge's own zone and tick
			// lag stored, last_heartbeat stamped.
			var status, tz string
			var tick sql.NullInt64
			var beat bool
			testutil.MustNoErr(t, db.QueryRow(`SELECT status, COALESCE(timezone, ''), tick_pending, last_heartbeat IS NOT NULL
				FROM edge_registry WHERE station_uid = $1`, st).Scan(&status, &tz, &tick, &beat), "read registry row")
			if status != "active" || tz != "America/New_York" || !tick.Valid || tick.Int64 != 4 || !beat {
				t.Errorf("registry row = status %q tz %q tick_pending %v heartbeat %v; want active, America/New_York, 4, stamped",
					status, tz, tick, beat)
			}
		})
	}
}

// ── HandleNodeListRequest ──────────────────────────────────────────────────

// seedFeedsPinNodes creates LN-1 assigned to edge.test and LN-9 assigned to
// nobody, so the station-scoped list and the plant-wide list differ.
func seedFeedsPinNodes(t *testing.T, db *store.DB) {
	t.Helper()
	ln1 := &nodes.Node{Name: "LN-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(ln1), "create LN-1")
	ln9 := &nodes.Node{Name: "LN-9", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(ln9), "create LN-9")
	testutil.MustNoErr(t, db.AssignNodeToStation(ln1.ID, "edge.test"), "assign LN-1")
}

// TestFeedsPin_HandleNodeListRequest_Scope pins which list a station gets:
// its own when it has one, the plant-wide list when its own is empty, and
// nothing at all when its own could not be READ (buildNodeList returns the
// error, and falling back would hand the station every node in the plant as
// though they were its own).
func TestFeedsPin_HandleNodeListRequest_Scope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		station          string
		breakStationRead bool

		wantReplies int
		wantExact   []string // the exact node names, when set
		wantHas     []string // names that must be present
	}{
		{
			name: "station-scoped list", station: "edge.test",
			wantReplies: 1, wantExact: []string{"LN-1"},
		},
		{
			name: "empty station list falls back to the plant-wide list", station: "edge.other",
			wantReplies: 1, wantHas: []string{"LN-1", "LN-9"},
		},
		{
			// Before the no-fallback node list (see the file header) this was
			// one reply with the plant-wide list.
			name: "station-list read error is a failed read: nothing sent", station: "edge.test", breakStationRead: true,
			wantReplies: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			seedFeedsPinNodes(t, db)
			if tc.breakStationRead {
				// node_stations is read by the station-scoped list only; the
				// plant-wide list, loaders, kanbans, bin types and
				// scene do not touch it.
				hideTable(t, db, "node_stations")
			}
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.HandleNodeListRequest(feedsPinEnv(tc.station), &protocol.NodeListRequest{})

			if len(resp.events) != tc.wantReplies {
				t.Fatalf("replies = %d, want %d", len(resp.events), tc.wantReplies)
			}
			if tc.wantReplies == 0 {
				return
			}
			if resp.events[0].kind != "reply" || resp.events[0].subject != protocol.SubjectNodeListResponse {
				t.Fatalf("event = %s %s, want reply %s", resp.events[0].kind, resp.events[0].subject, protocol.SubjectNodeListResponse)
			}
			r := resp.events[0].payload.(*protocol.NodeListResponse)
			var names []string
			for _, n := range r.Nodes {
				names = append(names, n.Name)
			}
			if tc.wantExact != nil && !reflect.DeepEqual(names, tc.wantExact) {
				t.Errorf("nodes = %v, want exactly %v", names, tc.wantExact)
			}
			have := map[string]bool{}
			for _, n := range names {
				have[n] = true
			}
			for _, w := range tc.wantHas {
				if !have[w] {
					t.Errorf("nodes = %v, want %s present", names, w)
				}
			}
		})
	}
}

// TestFeedsPin_HandleNodeListRequest_GeometryAndDigest pins geometry only on a
// scene-revision mismatch, the current scene revision on every reply, and the
// reply's digest: protocol.NodesDigest of the Nodes, Loaders, PayloadBinTypes
// and Family the reply carries, the scene excluded (the scene has its own
// feed key), so it is the same on all three replies.
func TestFeedsPin_HandleNodeListRequest_GeometryAndDigest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sceneRequest(t, db)
	seedFeedsPinNodes(t, db)
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})

	ask := func(rev string) *protocol.NodeListResponse {
		t.Helper()
		before := len(resp.events)
		svc.HandleNodeListRequest(feedsPinEnv("edge.test"), &protocol.NodeListRequest{SceneRevision: rev})
		if len(resp.events) != before+1 {
			t.Fatalf("replies = %d new, want 1", len(resp.events)-before)
		}
		return resp.events[before].payload.(*protocol.NodeListResponse)
	}
	current := ask("").SceneRevision
	if current == "" {
		t.Fatal("no scene revision from a readable scene")
	}

	cases := []struct {
		name         string
		heldRevision string
		wantGeometry bool
	}{
		{name: "no revision held", heldRevision: "", wantGeometry: true},
		{name: "current revision held", heldRevision: current, wantGeometry: false},
		{name: "stale revision held", heldRevision: "not-the-revision", wantGeometry: true},
	}
	firstDigest := ""
	for _, tc := range cases {
		r := ask(tc.heldRevision)
		if got := hasGeometry(r); got != tc.wantGeometry {
			t.Errorf("%s: geometry = %v, want %v", tc.name, got, tc.wantGeometry)
		}
		if r.SceneRevision != current {
			t.Errorf("%s: scene_revision = %q, want the current %q on every reply", tc.name, r.SceneRevision, current)
		}
		// "digest" present and equal to the FeedNodes digest of
		// {Nodes, Loaders, PayloadBinTypes, Family, PlantWide} (protocol.NodesDigest, the
		// function the reply path and the feed path both use), identical
		// across these three replies because only
		// the held scene revision differs.
		wantDigest, err := protocol.NodesDigest(r.Nodes, r.Loaders, r.PayloadBinTypes, r.Family, r.PlantWide)
		testutil.MustNoErr(t, err, "digest")
		if r.Digest == "" || r.Digest != wantDigest {
			t.Errorf("%s: digest = %q, want %q", tc.name, r.Digest, wantDigest)
		}
		if firstDigest == "" {
			firstDigest = r.Digest
		} else if r.Digest != firstDigest {
			t.Errorf("%s: digest %q differs from the first reply's %q", tc.name, r.Digest, firstDigest)
		}
	}
}

// ── HandleCatalogPayloadsRequest ───────────────────────────────────────────

// TestFeedsPin_HandleCatalogPayloadsRequest pins the catalog reply: one reply,
// payloads in the order the store returns them (by code), each row's fields,
// and the body's key set — digest and payloads. The digest's value is not
// pinned here; TestNodeFeed_ReplyDigestIsTheFeedDigest pins it equal to the
// FeedCatalog digest the heartbeat ack quotes. A row carries its payload's manifest lines (PC-A's one line); a payload with
// none carries no key (PC-B).
func TestFeedsPin_HandleCatalogPayloadsRequest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Created out of code order, so the reply's order is the store's, not ours.
	pb := &payloads.Payload{Code: "PC-B", Description: "synthetic B", UOPCapacity: 24}
	testutil.MustNoErr(t, db.CreatePayload(pb), "create PC-B")
	pa := &payloads.Payload{Code: "PC-A", Description: "synthetic A", UOPCapacity: 12}
	testutil.MustNoErr(t, db.CreatePayload(pa), "create PC-A")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: pa.ID, PartNumber: "PN-A", PartsPerCycle: 1,
	}, "CAT-A"), "manifest PC-A")

	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	svc.HandleCatalogPayloadsRequest(feedsPinEnv("edge.test"))

	if got, want := resp.trace(), []string{"reply " + protocol.SubjectCatalogPayloadsResponse}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace = %v, want %v", got, want)
	}
	r := resp.events[0].payload.(*protocol.CatalogPayloadsResponse)

	keys := jsonKeys(t, r)
	wantKeys := []string{"digest", "payloads"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("catalog reply keys = %v, want %v", keys, wantKeys)
	}

	var mine []protocol.CatalogPayloadInfo
	for _, p := range r.Payloads {
		if p.Code == "PC-A" || p.Code == "PC-B" {
			mine = append(mine, p)
		}
	}
	want := []protocol.CatalogPayloadInfo{
		{ID: pa.ID, Name: "PC-A", Code: "PC-A", Description: "synthetic A", UOPCapacity: 12, CATID: "CAT-A",
			Manifest: []protocol.ManifestItem{{PartNumber: "PN-A", PartsPerCycle: 1}}},
		{ID: pb.ID, Name: "PC-B", Code: "PC-B", Description: "synthetic B", UOPCapacity: 24},
	}
	if !reflect.DeepEqual(mine, want) {
		t.Errorf("catalog rows = %+v, want %+v in code order", mine, want)
	}
	// Order across the whole reply is the store's (ORDER BY code).
	for i := 1; i < len(r.Payloads); i++ {
		if r.Payloads[i-1].Code > r.Payloads[i].Code {
			t.Errorf("catalog not in code order at %d: %q before %q", i, r.Payloads[i-1].Code, r.Payloads[i].Code)
		}
	}
}

// ── Every feed's golden digest ─────────────────────────────────────────────

// goldenAt is the instant the golden fixtures stamp: the refusals' and
// verdicts' times, which their digests leave out.
var goldenAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// feedGolden is one feed's golden: a fixed input written to a fresh database
// (an enrolled edge.test, nothing else), the subject the heartbeat compare
// sends the feed on, the digest Core answers for it, and fromWire, the digest
// recomputed by the feed's own protocol function from the value sent as it
// decodes off the wire, so the answer is shown to be the digest of what an
// Edge receives. The text above each row is what its golden hashes.
type feedGolden struct {
	carrier  string
	golden   string
	seed     func(t *testing.T, svc *CoreDataService)
	fromWire func(t *testing.T, sent any) string
}

// feedGoldens has one row per feed Core serves; TestFeeds_GoldenDigestPerFeed
// fails on a feed in protocol.AllFeeds without one.
var feedGoldens = map[string]feedGolden{
	// {"flags":[{"payload_code":"P1","active":true,"reason":"r","activated_by":"qa",
	//   "activated_at":"2026-10-01T12:00:00Z","deactivated_by":"","deactivated_at":null}],
	//  "held_bins":[],"destinations":[]}
	// The flag TestBuildContainmentSnapshot_GoldenDigest builds and the value
	// protocol's TestContainmentDigest_Golden pins, here through the heartbeat.
	protocol.FeedContainment: {
		carrier: protocol.SubjectContainmentSnapshot,
		golden:  "495ee5733d2adacc",
		seed: func(t *testing.T, svc *CoreDataService) {
			testutil.MustNoErr(t, svc.db.SetPayloadContainment("P1", "r", "qa", true), "flag P1")
			_, err := svc.db.Exec(`UPDATE payload_containment SET activated_at = $1, updated_at = $1`,
				time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
			testutil.MustNoErr(t, err, "stamp the flag")
		},
		fromWire: func(t *testing.T, sent any) string {
			return goldenDigest(t)(protocol.ContainmentDigest(decodeWire[protocol.ContainmentSnapshot](t, sent)))
		},
	},
	// [{"loader_node":"SYN-GD-LDR","payload_code":"SYN-GD-P1","refused_by":"stn-gd-a","ack_choice":"",
	//   "ack_process_id":""},
	//  {"loader_node":"SYN-GD-LDR","payload_code":"SYN-GD-P2","refused_by":"stn-gd-a","ack_choice":"wait",
	//   "ack_process_id":"SYN-GD-PR1"}]
	protocol.FeedRefusals: {
		carrier: protocol.SubjectSupplyRefusalSnapshot,
		golden:  "29fde962eaf138a8",
		seed: func(t *testing.T, svc *CoreDataService) {
			for i, code := range []string{"SYN-GD-P1", "SYN-GD-P2"} {
				testutil.MustNoErr(t, svc.db.ApplySupplyRefusal(protocol.SupplyRefusalState{
					Action: protocol.SupplyRefusalOpened, LoaderNode: "SYN-GD-LDR", PayloadCode: code,
					RefusedAt: goldenAt.Add(time.Duration(i) * time.Minute), RefusedBy: "stn-gd-a",
				}), "open the refusal of "+code)
			}
			ackAt := goldenAt.Add(time.Hour)
			testutil.MustNoErr(t, svc.db.ApplySupplyRefusal(protocol.SupplyRefusalState{
				Action: protocol.SupplyRefusalAcked, LoaderNode: "SYN-GD-LDR", PayloadCode: "SYN-GD-P2",
				AckAt: &ackAt, AckChoice: protocol.SupplyRefusalChoiceWait, AckProcessID: "SYN-GD-PR1",
			}), "answer the refusal of SYN-GD-P2")
		},
		fromWire: func(t *testing.T, sent any) string {
			snap := decodeWire[protocol.SupplyRefusalSnapshot](t, sent)
			d := goldenDigest(t)(protocol.RefusalsDigest(snap.Open))
			if snap.Digest != d {
				t.Errorf("refusals: the snapshot names digest %s, its rows digest to %s", snap.Digest, d)
			}
			return d
		},
	},
	// {"nodes":[{"name":"LN-1","node_type":""}],"loaders":[],
	//  "payload_bin_types":[{"payload_code":"SYN-GD-P1","bin_type_code":"SYN-GD-BT"}],
	//  "floor":{"SYN-GD-BT":"SYN-GD-BT"}}
	// (NodeListResponse.Family under NodesDigest's key "floor"; a type in no
	// family is its own.)
	protocol.FeedNodes: {
		carrier: protocol.SubjectNodeListResponse,
		golden:  "c4cb681fa5ca164d",
		seed: func(t *testing.T, svc *CoreDataService) {
			seedFeedsPinNodes(t, svc.db)
			bt := &bins.BinType{Code: "SYN-GD-BT"}
			testutil.MustNoErr(t, svc.db.CreateBinType(bt), "create SYN-GD-BT")
			testutil.MustNoErr(t, service.NewPayloadService(svc.db).CreateWithSets(
				&payloads.Payload{Code: "SYN-GD-P1", UOPCapacity: 10},
				service.PayloadSets{BinTypeIDs: &[]int64{bt.ID}}), "create SYN-GD-P1 on SYN-GD-BT")
		},
		fromWire: func(t *testing.T, sent any) string {
			nl := decodeWire[protocol.NodeListResponse](t, sent)
			return goldenDigest(t)(protocol.NodesDigest(nl.Nodes, nl.Loaders, nl.PayloadBinTypes, nl.Family, nl.PlantWide))
		},
	},
	// Not JSON: scene.Revision is the SHA-256 of one "\n"-terminated line per
	// row, sorted, each a row's columns with its strings quoted (%q) and its
	// numbers exact, a handle that is not there written "-":
	//   e "AREA-G" "CP51-LM54" "StraightPath" "CP51" "LM54" -9.338 6.23 -8.855 6.221 - - - -
	//   e "AREA-G" "LM9-PP224" "BezierPath" "LM9" "PP224" -0.604 22.449 0.986 22.169 -0.287 22.094 0.303 22.142
	//   p "AREA-G" "LM9" "LocationMark" "" "" "" -0.604 22.449 0 1.5708 "{}"
	//   p "AREA-G" "PLN_ORIGIN" "GeneralLocation" "AP1" "" "" 0 0 0 0 "{}"
	// Row ids and synced_at are not in it. It covers columns the wire does not
	// carry (a point's properties), so fromWire is the revision the node list
	// carries. The golden moved once, from the hash of each row's (id,
	// synced_at), when the revision became the content's: every Edge is sent
	// its geometry once on that upgrade.
	protocol.FeedScene: {
		carrier: protocol.SubjectNodeListResponse,
		golden:  "edcc4e48f82820b49fed786cf76a449d4d3f9cf97aa27c578ff8ac8e5cc1b330",
		seed: func(t *testing.T, svc *CoreDataService) {
			sceneRequest(t, svc.db)
		},
		fromWire: func(t *testing.T, sent any) string {
			nl := decodeWire[protocol.NodeListResponse](t, sent)
			if !hasGeometry(&nl) {
				t.Error("scene: the node list sent to an Edge holding no scene carries no geometry")
			}
			return nl.SceneRevision
		},
	},
	// [{"id":1,"name":"SYN-GD-P1","code":"SYN-GD-P1","description":"golden part","uop_capacity":10,
	//   "catid":"SYN-GD-C1","manifest":[{"part_number":"SYN-GD-PN1","parts_per_cycle":2,"description":""}]}]
	protocol.FeedCatalog: {
		carrier: protocol.SubjectCatalogPayloadsResponse,
		golden:  "2e7fbf41f821d2a1",
		seed: func(t *testing.T, svc *CoreDataService) {
			p := &payloads.Payload{Code: "SYN-GD-P1", Description: "golden part", UOPCapacity: 10}
			testutil.MustNoErr(t, service.NewPayloadService(svc.db).CreateWithSets(p, service.PayloadSets{
				Manifest: &[]service.ManifestLine{{PartNumber: "SYN-GD-PN1", CATID: "SYN-GD-C1", PartsPerCycle: 2}},
			}), "create SYN-GD-P1")
			if p.ID != 1 {
				t.Fatalf("fixture: SYN-GD-P1 has id %d, want 1: the golden digest covers the id, and a new "+
					"database's sequence starts at 1", p.ID)
			}
		},
		fromWire: func(t *testing.T, sent any) string {
			cat := decodeWire[protocol.CatalogPayloadsResponse](t, sent)
			d := goldenDigest(t)(protocol.CatalogDigest(cat.Payloads))
			if cat.Digest != d {
				t.Errorf("catalog: the response names digest %s, its rows digest to %s", cat.Digest, d)
			}
			return d
		},
	},
	// newBinsFixture watched under binsFixtureNames: the list's digest, the
	// rows of binsFixture.wantRows (bin ids 1 and 2; the build's rows are
	// those, TestBuildNodeBins_FiveStatementsWhateverTheListLength) less their
	// counts, no groups.
	protocol.FeedBins: {
		carrier: protocol.SubjectBinsSnapshot,
		golden:  "f55f440e930daf52",
		seed: func(t *testing.T, svc *CoreDataService) {
			f := newBinsFixture(t, svc.db)
			if f.full != 1 || f.base != 2 {
				t.Fatalf("fixture: bin ids %d and %d, want 1 and 2: the golden digest covers them, and a new "+
					"database's sequence starts at 1", f.full, f.base)
			}
			svc.HandleBinWatch(feedsPinEnv("edge.test"), binsWatchList(t, binsFixtureNames...))
		},
		fromWire: func(t *testing.T, sent any) string {
			snap := decodeWire[protocol.NodeBinsSnapshot](t, sent)
			d := goldenDigest(t)(protocol.NodeBinsDigest(snap))
			if snap.Digest != d {
				t.Errorf("bins: the snapshot names digest %s, its rows digest to %s", snap.Digest, d)
			}
			return d
		},
	},
	// [{"process_id":"SYN-GD-PR1","style_id":"SYN-GD-S1","status":"red","missing":["SYN-GD-P1"],
	//   "absent":["SYN-GD-P1"],"at_risk":null,"reason":"Cannot change over: missing SYN-GD-P1."},
	//  {"process_id":"SYN-GD-PR1","style_id":"SYN-GD-S2","status":"yellow","missing":null,"absent":null,
	//   "at_risk":[{"payload_code":"SYN-GD-P2","node":"LN-1"}],"reason":"Can change over; SYN-GD-P2 running low."}]
	protocol.FeedSourcing: {
		carrier: protocol.SubjectSourcingState,
		golden:  "6bea1c8227734e57",
		seed: func(t *testing.T, svc *CoreDataService) {
			report := protocol.SourcingStateReport{Snapshot: true, States: []protocol.SourcingState{
				{ProcessID: "SYN-GD-PR1", StyleID: "SYN-GD-S1", Status: "red", Missing: []string{"SYN-GD-P1"},
					Absent: []string{"SYN-GD-P1"}, Reason: "Cannot change over: missing SYN-GD-P1.", ComputedAt: goldenAt},
				{ProcessID: "SYN-GD-PR1", StyleID: "SYN-GD-S2", Status: "yellow", Reason: "Can change over; SYN-GD-P2 running low.",
					AtRisk:     []protocol.SourcingAtRisk{{PayloadCode: "SYN-GD-P2", Node: "LN-1", TimeToEmptySeconds: 600}},
					ComputedAt: goldenAt},
			}}
			svc.SetSourcingReport(func(station string) (protocol.SourcingStateReport, bool) {
				return report, station == "edge.test"
			})
		},
		fromWire: func(t *testing.T, sent any) string {
			r := decodeWire[protocol.SourcingStateReport](t, sent)
			if !r.Snapshot {
				t.Error("sourcing: the compare's send is not a snapshot")
			}
			return goldenDigest(t)(protocol.SourcingDigest(r.States))
		},
	},
}

// TestFeeds_GoldenDigestPerFeed: for each feed Core serves, a fixed input
// gives a fixed digest on the ack, the heartbeat compare sends the feed on its
// carrier to an Edge holding nothing, and the value sent, decoded as an Edge
// decodes it, digests to the same string. A moved golden is a resend of that
// feed to every Edge at every plant on upgrade, so only a change that means it
// (the scene's revision from content) moves one, and says so in the
// changelog. The heartbeat is the station's first: no memo stands before it.
func TestFeeds_GoldenDigestPerFeed(t *testing.T) {
	t.Parallel()
	for _, key := range protocol.AllFeeds() {
		if _, ok := feedGoldens[key]; !ok {
			t.Errorf("feed %s has no row in feedGoldens: add its fixed input and golden digest", key)
		}
	}
	for key, g := range feedGoldens {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			_, err := db.EnrollEdge("edge.test", "")
			testutil.MustNoErr(t, err, "enroll")
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			g.seed(t, svc)

			ack, sends := heartbeatWith(svc, resp, map[string]string{key: ""})
			if got := ack.Feeds[key]; got != g.golden {
				t.Errorf("%s: Core answers digest %s for the fixed input, golden %s. If that is meant, every Edge "+
					"is sent this feed once on upgrade: update the golden and say so in the changelog", key, got, g.golden)
			}
			if len(sends) != 1 || sends[0].subject != g.carrier || sends[0].station != "edge.test" {
				var trace []string
				for _, s := range sends {
					trace = append(trace, s.subject+" -> "+s.station)
				}
				t.Fatalf("%s: sent %v, want one %s to edge.test", key, trace, g.carrier)
			}
			if got := g.fromWire(t, sends[0].payload); got != g.golden {
				t.Errorf("%s: the value sent digests to %s off the wire, golden %s", key, got, g.golden)
			}
		})
	}
}

// goldenDigest unwraps a digest function's two results.
func goldenDigest(t *testing.T) func(string, error) string {
	return func(d string, err error) string {
		t.Helper()
		testutil.MustNoErr(t, err, "digest")
		return d
	}
}

// decodeWire is v as an Edge receives it: encoded to JSON, decoded into T.
func decodeWire[T any](t *testing.T, v any) T {
	t.Helper()
	b, err := json.Marshal(v)
	testutil.MustNoErr(t, err, "encode the value sent")
	var out T
	testutil.MustNoErr(t, json.Unmarshal(b, &out), "decode the value sent")
	return out
}
