//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
)

// TestSourcingFeed_TheHeartbeatSendsTheStationItsSnapshot is FeedSourcing on
// the heartbeat compare, which replaced the snapshot on every (re)registration
// and the 30-minute snapshot to every station:
//   - a registration sends no sourcing report (the boot burst's heartbeat,
//     one round trip later, does the work);
//   - a heartbeat quoting a digest other than Core's for the station's own
//     report gets that report as a snapshot, built for that station, and the
//     ack quotes Core's digest;
//   - a heartbeat quoting Core's digest gets the confirmation and no send;
//   - before the monitor's first pass the key is left out of the ack and
//     nothing is sent.
func TestSourcingFeed_TheHeartbeatSendsTheStationItsSnapshot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "ST-SRC-SNAP"
	_, err := db.EnrollEdge(st, "", st)
	testutil.MustNoErr(t, err, "enroll edge")

	resp := &feedsPinResponder{}
	svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
	report := protocol.SourcingStateReport{Snapshot: true, States: []protocol.SourcingState{
		{ProcessID: "PRESS-SRC", StyleID: "S1", Status: "green", Reason: "Can change over."}}}
	ready := false
	var builtFor []string
	svc.SetSourcingReport(func(station string) (protocol.SourcingStateReport, bool) {
		builtFor = append(builtFor, station)
		return report, ready
	})
	want, err := protocol.SourcingDigest(report.States)
	testutil.MustNoErr(t, err, "digest")
	sends := func() []string {
		var out []string
		for _, e := range resp.events {
			if e.kind == "send" && e.subject == protocol.SubjectSourcingState {
				out = append(out, e.station)
			}
		}
		return out
	}
	beat := func(quoted string) *protocol.EdgeHeartbeatAck {
		t.Helper()
		resp.events = nil
		svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{StationID: st,
			Feeds: map[string]string{protocol.FeedSourcing: quoted}})
		for _, e := range resp.events {
			if e.kind == "reply" && e.subject == protocol.SubjectEdgeHeartbeatAck {
				return e.payload.(*protocol.EdgeHeartbeatAck)
			}
		}
		t.Fatal("no heartbeat ack")
		return nil
	}

	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})
	if got := sends(); len(got) != 0 || len(builtFor) != 0 {
		t.Fatalf("registration sent sourcing to %v and built %v, want neither", got, builtFor)
	}

	if ack := beat(""); len(sends()) != 0 {
		t.Errorf("before the first pass: sent to %v, want nothing", sends())
	} else if _, named := ack.Feeds[protocol.FeedSourcing]; named {
		t.Errorf("before the first pass the ack names sourcing (%q), want it left out", ack.Feeds[protocol.FeedSourcing])
	}

	ready = true
	ack := beat("")
	if got := sends(); len(got) != 1 || got[0] != st {
		t.Fatalf("a differing digest: sent to %v, want the station's snapshot to %s", got, st)
	}
	if ack.Feeds[protocol.FeedSourcing] != want {
		t.Errorf("ack quotes %q, want Core's digest %q", ack.Feeds[protocol.FeedSourcing], want)
	}
	if builtFor[len(builtFor)-1] != st {
		t.Errorf("built for %v, want the heartbeat's station", builtFor)
	}
	if beat(want); len(sends()) != 0 {
		t.Errorf("a matching digest: sent to %v, want nothing", sends())
	}
}

// TestHandlePlantClaims_RecordsThePublishingStation: the plant.claims ingest
// records the envelope's station against the process (process_styles.station_id,
// owner ruling 2026-09-25), and a second station publishing the same process
// becomes the recorded one — the last writer.
func TestHandlePlantClaims_RecordsThePublishingStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	report := &protocol.PlantClaimsReport{
		ProcessID: "PRESS-ST",
		Styles:    []protocol.PlantClaimsStyle{{StyleID: "S1"}},
	}

	svc.HandlePlantClaims(registerEnv("edge-a"), report)
	stations, err := db.ProcessStations()
	testutil.MustNoErr(t, err, "read stations")
	if stations["PRESS-ST"] != "edge-a" {
		t.Fatalf("PRESS-ST is recorded against %q after edge-a published it, want edge-a", stations["PRESS-ST"])
	}

	svc.HandlePlantClaims(registerEnv("edge-b"), report)
	stations, err = db.ProcessStations()
	testutil.MustNoErr(t, err, "read stations")
	if stations["PRESS-ST"] != "edge-b" {
		t.Fatalf("PRESS-ST is recorded against %q after edge-b also published it, want edge-b (the last writer)",
			stations["PRESS-ST"])
	}
}
