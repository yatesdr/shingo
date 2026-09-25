//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
)

// TestHandleEdgeRegister_SendsTheStationItsSourcingSnapshot: a (re)registering
// Edge is sent its own sourcing snapshot at once. The feed sends full snapshots
// only on a 30-minute backstop now (Stage A2), so without this a Pi that
// reconnects would read a stale cache until the backstop.
func TestHandleEdgeRegister_SendsTheStationItsSourcingSnapshot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "ST-SRC-SNAP"
	_, err := db.EnrollEdge(st, "", st)
	testutil.MustNoErr(t, err, "enroll edge")

	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	var got []string
	svc.SetSourcingSnapshotter(func(station string) { got = append(got, station) })
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})

	if len(got) != 1 || got[0] != st {
		t.Fatalf("registration asked for sourcing snapshots %v, want exactly [%s]", got, st)
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
