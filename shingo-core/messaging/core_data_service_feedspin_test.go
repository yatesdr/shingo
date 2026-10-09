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
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// core_data_service_feedspin_test.go — behaviour pins for the Core request
// handlers the versioned-feeds change touches: the heartbeat, the node-list
// sync and the catalog sync. Each case asserts what the code does at the base
// and carries the predicted post-change value beside it with the brief label
// that moves it. A label commit flips want to after for its own cases only.

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
// write, the ack and its body, the register request for an unenrolled station,
// and the error path (UpdateHeartbeat fails: today nothing at all is sent).
//
// Every heartbeat here has no Feeds field, which is what an old Edge sends
// after F1. The brief: nil Feeds gets today's behaviour only, and a new Core
// always sends feeds as at least {}; Claims is filled only on the feeds path
// and is nil when Core could not read it, so an old Edge's ack carries
// "claims": null.
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

		wantTrace   []string // base
		wantAckKeys []string // base; nil when no ack is sent

		afterTrace   []string // predicted
		afterAckKeys []string // predicted
		label        string
	}{
		{
			name: "enrolled station, plant zone configured", enroll: true, plantTZ: "America/Chicago",
			wantTrace:    []string{ack},
			wantAckKeys:  []string{"claims", "feeds", "server_ts", "station_id", "timezone"}, // F1: feeds={}, claims=null
			afterTrace:   []string{ack},
			afterAckKeys: []string{"claims", "feeds", "server_ts", "station_id", "timezone"}, // feeds={}, claims=null
			label:        "F1, F5",
		},
		{
			name: "enrolled station, plant zone unset", enroll: true, plantTZ: "",
			wantTrace:    []string{ack},
			wantAckKeys:  []string{"claims", "feeds", "server_ts", "station_id"}, // F1
			afterTrace:   []string{ack},
			afterAckKeys: []string{"claims", "feeds", "server_ts", "station_id"}, // feeds={}, claims=null
			label:        "F1, F5",
		},
		{
			name: "unenrolled station gets the ack and a register request", enroll: false, plantTZ: "America/Chicago",
			wantTrace:    []string{ack, regReq},
			wantAckKeys:  []string{"claims", "feeds", "server_ts", "station_id", "timezone"}, // F1
			afterTrace:   []string{ack, regReq},
			afterAckKeys: []string{"claims", "feeds", "server_ts", "station_id", "timezone"},
			label:        "F1, F5",
		},
		{
			// X8: was no ack at all (trace empty).
			name: "registry write fails: ack, no register request", enroll: true, breakRegistry: true, plantTZ: "America/Chicago",
			wantTrace:   []string{ack},
			wantAckKeys: []string{"claims", "feeds", "server_ts", "station_id", "timezone"},
			// X8: Core acks anyway, with only the feed keys it could read (none
			// for an old Edge) and Claims nil; still no register request.
			afterTrace:   []string{ack},
			afterAckKeys: []string{"claims", "feeds", "server_ts", "station_id", "timezone"},
			label:        "X8, F1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			if tc.enroll {
				_, err := db.EnrollEdge(st, "", st)
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
				t.Fatalf("trace = %v, want %v (after %s: %v)", got, tc.wantTrace, tc.label, tc.afterTrace)
			}

			var gotAck *protocol.EdgeHeartbeatAck
			for _, e := range resp.events {
				switch p := e.payload.(type) {
				case *protocol.EdgeHeartbeatAck:
					gotAck = p
				case *protocol.EdgeRegisterRequest:
					if p.StationID != st || p.Reason != "station not enrolled" {
						t.Errorf("register request = %+v, want station %s, reason \"station not enrolled\" (after: same)", p, st)
					}
				}
			}
			if tc.wantAckKeys == nil {
				if gotAck != nil {
					t.Errorf("an ack was sent: %+v (after %s: keys %v)", gotAck, tc.label, tc.afterAckKeys)
				}
				return
			}
			if gotAck == nil {
				t.Fatal("no ack payload recorded")
			}
			if keys := jsonKeys(t, gotAck); !reflect.DeepEqual(keys, tc.wantAckKeys) {
				t.Errorf("ack keys = %v, want %v (after %s: %v)", keys, tc.wantAckKeys, tc.label, tc.afterAckKeys)
			}
			if gotAck.StationID != st {
				t.Errorf("ack station_id = %q, want %q (after: same)", gotAck.StationID, st)
			}
			if gotAck.Timezone != tc.plantTZ {
				t.Errorf("ack timezone = %q, want the configured plant zone %q (after: same)", gotAck.Timezone, tc.plantTZ)
			}
			if gotAck.ServerTS.Before(before) || gotAck.ServerTS.After(afterCall) || gotAck.ServerTS.Location() != time.UTC {
				t.Errorf("ack server_ts = %v, want UTC now (after: same)", gotAck.ServerTS)
			}

			if !tc.enroll || tc.breakRegistry {
				return
			}
			// The registry write: status promoted, the Edge's own zone and tick
			// lag stored, last_heartbeat stamped. After: same.
			var status, tz string
			var tick sql.NullInt64
			var beat bool
			testutil.MustNoErr(t, db.QueryRow(`SELECT status, COALESCE(timezone, ''), tick_pending, last_heartbeat IS NOT NULL
				FROM edge_registry WHERE station_uid = $1`, st).Scan(&status, &tz, &tick, &beat), "read registry row")
			if status != "active" || tz != "America/New_York" || !tick.Valid || tick.Int64 != 4 || !beat {
				t.Errorf("registry row = status %q tz %q tick_pending %v heartbeat %v; want active, America/New_York, 4, stamped (after: same)",
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
// its own when it has one, the plant-wide list when its own is empty, and —
// today — the plant-wide list too when its own could not be READ. X6 makes
// that last one a failed read: nothing sent.
func TestFeedsPin_HandleNodeListRequest_Scope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		station          string
		breakStationRead bool

		wantReplies int
		wantExact   []string // the exact node names, when set
		wantHas     []string // names that must be present

		afterReplies int
		label        string
	}{
		{
			name: "station-scoped list", station: "edge.test",
			wantReplies: 1, wantExact: []string{"LN-1"},
			afterReplies: 1, label: "same",
		},
		{
			name: "empty station list falls back to the plant-wide list", station: "edge.other",
			wantReplies: 1, wantHas: []string{"LN-1", "LN-9"},
			afterReplies: 1, label: "same",
		},
		{
			// X6: was one reply with the plant-wide list.
			name: "station-list read error is a failed read: nothing sent", station: "edge.test", breakStationRead: true,
			wantReplies:  0,
			afterReplies: 0, label: "X6",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			seedFeedsPinNodes(t, db)
			if tc.breakStationRead {
				// node_stations is read by the station-scoped list only; the
				// plant-wide list, loaders, maintained groups, bin types and
				// scene do not touch it.
				hideTable(t, db, "node_stations")
			}
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.HandleNodeListRequest(feedsPinEnv(tc.station), &protocol.NodeListRequest{})

			if len(resp.events) != tc.wantReplies {
				t.Fatalf("replies = %d, want %d (after %s: %d)", len(resp.events), tc.wantReplies, tc.label, tc.afterReplies)
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
				t.Errorf("nodes = %v, want exactly %v (after: same)", names, tc.wantExact)
			}
			have := map[string]bool{}
			for _, n := range names {
				have[n] = true
			}
			for _, w := range tc.wantHas {
				if !have[w] {
					t.Errorf("nodes = %v, want %s present (after %s: %d replies)", names, w, tc.label, tc.afterReplies)
				}
			}
		})
	}
}

// TestFeedsPin_HandleNodeListRequest_GeometryAndDigest pins geometry only on a
// scene-revision mismatch (after: same — F4 keeps the short-circuit) and the
// reply's key set: no digest today. After F4 every reply carries
// digest = protocol.Digest(struct{Nodes, Loaders, PayloadBinTypes}) as the
// reply carries them, the scene excluded (scene has its own key).
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
		after        string
		label        string
	}{
		{name: "no revision held", heldRevision: "", wantGeometry: true, after: "same", label: "-"},
		{name: "current revision held", heldRevision: current, wantGeometry: false, after: "same", label: "-"},
		{name: "stale revision held", heldRevision: "not-the-revision", wantGeometry: true, after: "same", label: "-"},
	}
	firstDigest := ""
	for _, tc := range cases {
		r := ask(tc.heldRevision)
		if got := hasGeometry(r); got != tc.wantGeometry {
			t.Errorf("%s: geometry = %v, want %v (after: %s)", tc.name, got, tc.wantGeometry, tc.after)
		}
		if r.SceneRevision != current {
			t.Errorf("%s: scene_revision = %q, want the current %q on every reply (after: same)", tc.name, r.SceneRevision, current)
		}
		// F4: "digest" present and equal to the FeedNodes digest of
		// {Nodes, Loaders, PayloadBinTypes} (protocol.NodesDigest — the pin
		// predicted protocol.Digest; the canonical-copy function is the one
		// both paths use), identical across these three replies because only
		// the held scene revision differs.
		wantDigest, err := protocol.NodesDigest(r.Nodes, r.Loaders, r.PayloadBinTypes)
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
// and the body's key set — "payloads" only. After F4: keys digest, payloads,
// with digest = protocol.Digest of the payload list; rows and order unchanged.
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
		t.Fatalf("trace = %v, want %v (after: same)", got, want)
	}
	r := resp.events[0].payload.(*protocol.CatalogPayloadsResponse)

	keys := jsonKeys(t, r)
	wantKeys := []string{"digest", "payloads"}  // F4: was payloads only
	afterKeys := []string{"digest", "payloads"} // F4
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("catalog reply keys = %v, want %v (after F4: %v)", keys, wantKeys, afterKeys)
	}

	var mine []protocol.CatalogPayloadInfo
	for _, p := range r.Payloads {
		if p.Code == "PC-A" || p.Code == "PC-B" {
			mine = append(mine, p)
		}
	}
	want := []protocol.CatalogPayloadInfo{
		{ID: pa.ID, Name: "PC-A", Code: "PC-A", Description: "synthetic A", UOPCapacity: 12, CATID: "CAT-A"},
		{ID: pb.ID, Name: "PC-B", Code: "PC-B", Description: "synthetic B", UOPCapacity: 24},
	}
	if !reflect.DeepEqual(mine, want) {
		t.Errorf("catalog rows = %+v, want %+v in code order (after: same)", mine, want)
	}
	// Order across the whole reply is the store's (ORDER BY code).
	for i := 1; i < len(r.Payloads); i++ {
		if r.Payloads[i-1].Code > r.Payloads[i].Code {
			t.Errorf("catalog not in code order at %d: %q before %q (after: same)", i, r.Payloads[i-1].Code, r.Payloads[i].Code)
		}
	}
}
