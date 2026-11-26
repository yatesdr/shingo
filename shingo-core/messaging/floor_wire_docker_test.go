//go:build docker

package messaging

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
)

// TestFloorWire_AnEdgesBodiesNeedNoStationCopy: a register, a heartbeat and a
// lineside report with the station only in the envelope do all they do with a
// copy in the body; the two replies still carry the station_id a v0.19.0 Edge
// logs. Raw JSON, as TestFloorWire_* on the Edge: green at v0.19.0 itself, so
// a v0.19.0 Core takes the bodies of an Edge that dropped the copies.
func TestFloorWire_AnEdgesBodiesNeedNoStationCopy(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "stn-f1f1f1f1f1f1f1f1"
	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	svc.SetAskKinds(func() []string { return []string{"bin.count"} })
	replyKeys := func(want func(payload any) bool) map[string]json.RawMessage {
		t.Helper()
		for _, e := range resp.events {
			if e.kind != "reply" || !want(e.payload) {
				continue
			}
			raw, err := json.Marshal(e.payload)
			testutil.MustNoErr(t, err, "encode the reply")
			var keys map[string]json.RawMessage
			testutil.MustNoErr(t, json.Unmarshal(raw, &keys), "decode the reply")
			return keys
		}
		t.Fatalf("no such reply in %v", resp.trace())
		return nil
	}
	quoted := `"` + st + `"`

	var reg protocol.EdgeRegister
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"hostname":"syn-edge","version":"v0.19.0","schema_version":59}`), &reg), "decode the register")
	svc.HandleEdgeRegister(registerEnv(st), &reg)
	var schema int
	testutil.MustNoErr(t, db.QueryRow(`SELECT schema_version FROM edge_registry WHERE station_uid = $1`, st).Scan(&schema), "read the registry row")
	if schema != 59 {
		t.Errorf("schema_version = %d, want the register's 59", schema)
	}
	registered := replyKeys(func(p any) bool { _, ok := p.(*protocol.EdgeRegistered); return ok })
	if got := string(registered["station_id"]); got != quoted {
		t.Errorf("edge.registered station_id = %s, want %s", got, quoted)
	}

	resp.events = nil
	var hb protocol.EdgeHeartbeat
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"feeds":{"bins":""},"owned":{"claims":"stale"}}`), &hb), "decode the heartbeat")
	svc.HandleEdgeHeartbeat(feedsPinEnv(st), &hb)
	ack := replyKeys(func(p any) bool { _, ok := p.(*protocol.EdgeHeartbeatAck); return ok })
	if got := string(ack["station_id"]); got != quoted {
		t.Errorf("ack station_id = %s, want %s", got, quoted)
	}
	var owned map[string]protocol.OwnedAnswer
	testutil.MustNoErr(t, json.Unmarshal(ack["owned"], &owned), "decode the ack owned map")
	if _, ok := owned[protocol.OwnedClaims]; !ok || string(ack["asks"]) != `["bin.count"]` {
		t.Errorf("ack owned %v asks %s, want the claims answered and the ask named", owned, ack["asks"])
	}

	var rep protocol.LinesideLevelReport
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"reported_at":"2026-01-02T03:04:05Z","entries":[{"core_node_name":"SYN-LINE",`+
		`"payload_code":"SYN-PART-A","bin_count":0,"bin_uop":0,"bucket_qty":0}]}`), &rep), "decode the report")
	svc.HandleLinesideLevelReport(linesideEnvelope(st), &rep)
	if _, ok := storedReportedAt(t, db, st, "SYN-LINE", "SYN-PART-A"); !ok {
		t.Errorf("no lineside row under %s", st)
	}
}
