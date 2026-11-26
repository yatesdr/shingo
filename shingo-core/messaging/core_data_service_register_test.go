//go:build docker

package messaging

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

func registerEnv(stationID string) *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
}

func stationThresholds(t *testing.T, db *store.DB, stationID string) map[string]int {
	t.Helper()
	rows, err := db.DB.Query(`SELECT core_node_name || '/' || payload_code, replenish_uop_threshold
		FROM demand_registry WHERE station_uid = $1`, stationID)
	testutil.MustNoErr(t, err, "read demand_registry")
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var thr int
		testutil.MustNoErr(t, rows.Scan(&k, &thr), "scan")
		out[k] = thr
	}
	return out
}

// seedRegisterLoader is one dedicated loader with one payload-bearing home.
func seedRegisterLoader(t *testing.T, db *store.DB, nodeName, payload string, threshold int) (loaderID, nodeID int64) {
	t.Helper()
	id, err := db.CreateLoader(loaders.Loader{Name: "L-" + nodeName, Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	testutil.MustNoErr(t, err, "create loader")
	n := &nodes.Node{Name: nodeName, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create node")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n.ID,
		PayloadCode: payload, UOPThreshold: threshold}), "upsert home")
	return id, n.ID
}

// TestHandleEdgeRegisterPin_DerivesRegistry pins the register writer: a
// (re)connect derives the station's registry from the aggregate, and a second
// register after the loader is archived commits the now-empty derivation.
func TestHandleEdgeRegisterPin_DerivesRegistry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "ST-REG-PIN"
	_, err := db.EnrollEdge(st, "")
	testutil.MustNoErr(t, err, "enroll edge")
	id, _ := seedRegisterLoader(t, db, "REG-PIN-SLOT", "P-REG", 12)

	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{Hostname: "h", Version: "v"})

	got := stationThresholds(t, db, st)
	if len(got) != 1 || got["REG-PIN-SLOT/P-REG"] != 12 {
		t.Fatalf("registry after register = %v, want REG-PIN-SLOT/P-REG=12", got)
	}

	testutil.MustNoErr(t, db.DeleteLoader(id), "archive loader")
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{Hostname: "h", Version: "v"})
	if got := stationThresholds(t, db, st); len(got) != 0 {
		t.Fatalf("registry after archive + register = %v, want empty — every loader archived is config with no demand", got)
	}
}

// TestHandleEdgeRegister_UnresolvableNodeKeepsRows is the refusal on the
// register writer: a (re)connect whose derivation could not resolve a home's
// node must leave the station's rows where they are, and say so.
//
// Not parallel: it reads the process-global logger.
func TestHandleEdgeRegister_UnresolvableNodeKeepsRows(t *testing.T) {
	db := testdb.Open(t)
	const st = "ST-REG-RED"
	_, err := db.EnrollEdge(st, "")
	testutil.MustNoErr(t, err, "enroll edge")
	_, nodeID := seedRegisterLoader(t, db, "REG-RED-SLOT", "P-RED", 7)

	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{Hostname: "h", Version: "v"})
	if got := stationThresholds(t, db, st); got["REG-RED-SLOT/P-RED"] != 7 {
		t.Fatalf("seeded registry = %v, want REG-RED-SLOT/P-RED=7", got)
	}

	_, err = db.DB.Exec(`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_position_node_id_fkey`)
	testutil.MustNoErr(t, err, "drop fk")
	_, err = db.DB.Exec(`DELETE FROM nodes WHERE id=$1`, nodeID)
	testutil.MustNoErr(t, err, "delete node")

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, &buf))
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{Hostname: "h", Version: "v"})
	log.SetOutput(prev)

	if got := stationThresholds(t, db, st); got["REG-RED-SLOT/P-RED"] != 7 {
		t.Errorf("registry after an unresolvable register = %v, want REG-RED-SLOT/P-RED=7 kept", got)
	}
	if out := buf.String(); !strings.Contains(out, "station="+st) || !strings.Contains(out, "outcome=refused") || !strings.Contains(out, "node_gone=1") {
		t.Errorf("log = %q, want one derive line for %s with outcome=refused and node_gone=1", out, st)
	}
}

// TestHandleEdgeRegister_ANewEdgeIsIntroducedAndThenKnown pins "a new Edge
// just works" at Core: a heartbeat from a uid Core has no row for is acked and
// draws a register request addressed to that uid and writes no row; the
// register that answers it introduces the station (unclaimed, unnamed, with the
// host and version it sent) and is answered; the next heartbeat is known, so it
// draws no second request and stamps the row.
//
// The envelope's source and the payload carry the same uid, as the Edge sends
// them; Core reads the envelope's (TestHandlers_TheStationIsTheEnvelopes).
func TestHandleEdgeRegister_ANewEdgeIsIntroducedAndThenKnown(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const uid = "stn-a0a0a0a0a0a0a0a0"
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	rowCount := func() int {
		t.Helper()
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM edge_registry WHERE station_uid = $1`, uid).Scan(&n), "count rows")
		return n
	}
	ack := "reply " + protocol.SubjectEdgeHeartbeatAck

	svc.HandleEdgeHeartbeat(feedsPinEnv(uid), &protocol.EdgeHeartbeat{Feeds: map[string]string{}})
	if got, want := resp.trace(), []string{ack, "send " + protocol.SubjectEdgeRegisterRequest + " -> " + uid}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first heartbeat: trace = %v, want %v", got, want)
	}
	if n := rowCount(); n != 0 {
		t.Fatalf("the heartbeat wrote %d registry rows for an unknown uid, want 0: only the register introduces", n)
	}

	resp.events = nil
	svc.HandleEdgeRegister(registerEnv(uid), &protocol.EdgeRegister{Hostname: "pin-host",
		Instance: "pin-run-1", Version: "v-pin", SchemaVersion: 3})
	if got, want := resp.trace(), []string{"reply " + protocol.SubjectEdgeRegistered}; !reflect.DeepEqual(got, want) {
		t.Fatalf("register: trace = %v, want %v", got, want)
	}
	if reg, ok := resp.events[0].payload.(*protocol.EdgeRegistered); !ok || reg.Message != "registered" {
		t.Errorf("register reply = %#v, want an edge.registered saying \"registered\"", resp.events[0].payload)
	}
	var name, host, version, status, boundInstance string
	var unclaimed bool
	var schema int
	testutil.MustNoErr(t, db.QueryRow(`SELECT display_name, hostname, version, status, bound_instance,
		claimed_at IS NULL, COALESCE(schema_version, -1) FROM edge_registry WHERE station_uid = $1`, uid).Scan(
		&name, &host, &version, &status, &boundInstance, &unclaimed, &schema), "read the introduced row")
	if name != "" || !unclaimed || host != "pin-host" || version != "v-pin" || status != "active" ||
		boundInstance != "pin-run-1" || schema != 3 {
		t.Errorf("introduced row = name %q unclaimed %v host %q version %q status %q instance %q schema %d; "+
			"want unnamed, unclaimed, pin-host, v-pin, active, pin-run-1, 3", name, unclaimed, host, version, status, boundInstance, schema)
	}

	resp.events = nil
	svc.HandleEdgeHeartbeat(feedsPinEnv(uid), &protocol.EdgeHeartbeat{Feeds: map[string]string{}})
	if got, want := resp.trace(), []string{ack}; !reflect.DeepEqual(got, want) {
		t.Fatalf("heartbeat after the register: trace = %v, want %v (no second register request)", got, want)
	}
	var beat bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT last_heartbeat IS NOT NULL FROM edge_registry WHERE station_uid = $1`, uid).Scan(&beat), "read heartbeat")
	if !beat || rowCount() != 1 {
		t.Errorf("after the second heartbeat: last_heartbeat stamped %v, rows %d; want stamped, 1", beat, rowCount())
	}
}

// TestHandlers_TheStationIsTheEnvelopes pins B0 for the register and the
// heartbeat: Core takes the station from the envelope's source and never from
// a station_id in the body, which a v0.19.0 Edge sends. Each body here is that
// JSON naming a station that does not exist; were it read, the register would
// introduce that one and the heartbeat would draw a register request to it.
func TestHandlers_TheStationIsTheEnvelopes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const sender, body = "stn-b0b0b0b0b0b0b0b0", "stn-not-the-sender"
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	rows := func(uid string) int {
		t.Helper()
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM edge_registry WHERE station_uid = $1`, uid).Scan(&n), "count rows")
		return n
	}

	var reg protocol.EdgeRegister
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"station_id":"`+body+`","hostname":"h","version":"v"}`), &reg), "decode the register")
	svc.HandleEdgeRegister(registerEnv(sender), &reg)
	if rows(sender) != 1 || rows(body) != 0 {
		t.Fatalf("register from %s naming %s: rows %d for the sender, %d for the body's; want 1 and 0",
			sender, body, rows(sender), rows(body))
	}
	if reg, ok := resp.events[0].payload.(*protocol.EdgeRegistered); !ok || reg.StationID != sender {
		t.Errorf("register reply = %#v, want edge.registered echoing the sender %s", resp.events[0].payload, sender)
	}

	resp.events = nil
	var beatBody protocol.EdgeHeartbeat
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"station_id":"`+body+`","feeds":{}}`), &beatBody), "decode the heartbeat")
	svc.HandleEdgeHeartbeat(feedsPinEnv(sender), &beatBody)
	if got, want := resp.trace(), []string{"reply " + protocol.SubjectEdgeHeartbeatAck}; !reflect.DeepEqual(got, want) {
		t.Fatalf("heartbeat from %s naming %s: trace = %v, want %v (the sender is known)", sender, body, got, want)
	}
	if ack, ok := resp.events[0].payload.(*protocol.EdgeHeartbeatAck); !ok || ack.StationID != sender {
		t.Errorf("ack = %#v, want one echoing the sender %s", resp.events[0].payload, sender)
	}
	var beat bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT last_heartbeat IS NOT NULL FROM edge_registry WHERE station_uid = $1`, sender).Scan(&beat), "read heartbeat")
	if !beat {
		t.Errorf("the sender's last_heartbeat was not stamped")
	}
}
