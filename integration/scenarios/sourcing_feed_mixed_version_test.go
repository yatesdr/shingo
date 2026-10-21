// The per-station sourcing feed across the Core/Edge version boundary, both
// ways, through the real wire (Core outbox → Bus → Edge ingestor, which drops a
// message addressed to another station exactly as production does).
//
//   - AN EDGE THAT HOLDS THE WHOLE PLANT, as every Edge did under an older Core.
//     Its cache's digest (FeedSourcing) is not the one Core builds for its
//     station, so Core's heartbeat compare sends it the station's snapshot
//     (StationReport; the compare itself is pinned in Core's messaging tests).
//     Over the real wire, with the other station's snapshot on it too, the Edge
//     must end up with exactly its own processes and the unrouted ones, quoting
//     Core's digest.
//
//   - NEW EDGE + OLD CORE. An older Core sends the whole plant to every station
//     on every full pass. The Edge from this phase must still apply that and
//     read its own processes from it.
//
//go:build docker

package scenarios

import (
	"context"
	"sort"
	"testing"

	"shingo/integration/harness"
	"shingo/protocol"

	"shingocore/config"
	coreengine "shingocore/engine"
	coreplantclaims "shingocore/store/plantclaims"
	coreharness "shingocore/testharness"

	edgeharness "shingoedge/testharness"
)

const (
	mixedOwnStation   = "edge.test"
	mixedOtherStation = "edge.other"
)

// mixedCore builds a Core with three mirrored processes — one published by
// the Edge under test, one by another station, one with no recorded station —
// and a Bus to the Edge under test.
func mixedCore(t *testing.T) (*coreengine.Engine, *harness.Bus, *edgeharness.Edge) {
	t.Helper()
	coreDB := coreharness.OpenDB(t)
	sd := coreharness.SetupStandardData(t, coreDB)
	coreharness.CreateBinAtNode(t, coreDB, sd.Payload.Code, sd.StorageNode.ID, "MIXED-BIN")
	for _, p := range []struct{ process, station string }{
		{"PRESS-OWN", mixedOwnStation}, {"PRESS-OTHER", mixedOtherStation}, {"PRESS-NULL", ""},
	} {
		if _, err := coreDB.ReplacePlantClaimsFromStation(p.process, p.station,
			[]coreplantclaims.StyleRow{{ProcessID: p.process, StyleID: "S1"}},
			[]coreplantclaims.ClaimRow{{ProcessID: p.process, StyleID: "S1",
				CoreNodeName: sd.StorageNode.Name, PayloadCode: sd.Payload.Code}},
			nil, 0); err != nil {
			t.Fatalf("mirror %s: %v", p.process, err)
		}
	}

	cfg := config.Defaults()
	cfg.Messaging.StationID = "core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	eng := coreengine.New(coreengine.Config{AppConfig: cfg, DB: coreDB,
		Fleet: coreharness.NewTrackingBackend(), LogFunc: t.Logf})

	edge := edgeharness.NewEdge(t, mixedOwnStation)
	coreIngestor := protocol.NewIngestor(nil)
	coreIngestor.Dispatch = func(*protocol.Envelope) {}
	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
	)
	return eng, bus, edge
}

// edgeCache lists the processes in the Edge's sourcing cache.
func edgeCache(t *testing.T, edge *edgeharness.Edge) []string {
	t.Helper()
	rows, err := edge.DB.Query(`SELECT DISTINCT process_id FROM sourcing_state ORDER BY process_id`)
	if err != nil {
		t.Fatalf("read the edge's sourcing cache: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// oldCoreSnapshot is the wire an older Core sends: every process, to every
// station.
func oldCoreSnapshot(t *testing.T, eng *coreengine.Engine) {
	t.Helper()
	report := protocol.SourcingStateReport{Snapshot: true}
	for _, p := range []string{"PRESS-NULL", "PRESS-OTHER", "PRESS-OWN"} {
		report.States = append(report.States, protocol.SourcingState{ProcessID: p, StyleID: "S1", Status: "green"})
	}
	if err := eng.SendDataToEdge(protocol.SubjectSourcingState, protocol.StationBroadcast, report); err != nil {
		t.Fatalf("send the old Core's snapshot: %v", err)
	}
}

func TestScenario_SourcingFeed_AWholePlantCacheIsHealedByTheStationsSnapshot(t *testing.T) {
	eng, bus, edge := mixedCore(t)

	// The Edge has been fed the whole plant, as every Edge was before.
	oldCoreSnapshot(t, eng)
	bus.PumpAll()
	if got := edgeCache(t, edge); len(got) != 3 {
		t.Fatalf("fixture: after the whole-plant snapshot the edge caches %v, want all three processes", got)
	}

	// The Core's first pass readies each station's snapshot and sends none.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := eng.SourceabilityMonitor()
	m.Run(ctx)
	own, ok := m.StationReport(mixedOwnStation)
	other, okOther := m.StationReport(mixedOtherStation)
	if !ok || !okOther {
		t.Fatal("the station snapshots are not ready after the first pass")
	}
	want, err := protocol.SourcingDigest(own.States)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if held := edge.Engine.FeedDigests()[protocol.FeedSourcing]; held == want {
		t.Fatalf("a whole-plant cache quotes the station's digest %q: the compare would never send the snapshot", held)
	}

	// What the compare sends this station, and another station's on the wire.
	for _, r := range []struct {
		station string
		report  protocol.SourcingStateReport
	}{{mixedOtherStation, other}, {mixedOwnStation, own}} {
		if err := eng.SendDataToEdge(protocol.SubjectSourcingState, r.station, r.report); err != nil {
			t.Fatalf("send %s's snapshot: %v", r.station, err)
		}
	}
	bus.PumpAll()

	got := edgeCache(t, edge)
	if len(got) != 2 || got[0] != "PRESS-NULL" || got[1] != "PRESS-OWN" {
		t.Fatalf("after the per-station snapshot the edge caches %v, want [PRESS-NULL PRESS-OWN]: its own "+
			"process and the unrouted one, with the other station's rows gone", got)
	}
	if states := edge.Engine.SourcingStateForProcess("PRESS-OWN"); len(states) != 1 {
		t.Fatalf("the edge reads %d states for its own process, want 1", len(states))
	}
	if held := edge.Engine.FeedDigests()[protocol.FeedSourcing]; held != want {
		t.Errorf("after the snapshot the edge quotes %q, want Core's digest for the station %q", held, want)
	}
}

func TestScenario_SourcingFeed_NewEdgeAppliesAnOldCoresWholePlantSnapshot(t *testing.T) {
	eng, bus, edge := mixedCore(t)

	oldCoreSnapshot(t, eng)
	bus.PumpAll()

	if got := edgeCache(t, edge); len(got) != 3 {
		t.Fatalf("the edge caches %v from an old Core's whole-plant snapshot, want all three processes", got)
	}
	if own := edge.Engine.SourcingStateForProcess("PRESS-OWN"); len(own) != 1 || own[0].Status != "green" {
		t.Fatalf("the edge reads %+v for its own process from an old Core's snapshot, want one green state", own)
	}
}
