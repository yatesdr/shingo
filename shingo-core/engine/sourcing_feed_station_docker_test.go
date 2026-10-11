//go:build docker

package engine

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/plantclaims"
)

// feedRow is one sourcing report as it left Core: the station it was addressed
// to (the outbox row's station_id — "*" is protocol.StationBroadcast), whether
// it was a snapshot, and the processes it carried.
type feedRow struct {
	station   string
	snapshot  bool
	processes []string
}

// sourcingFeed reads every sourcing-state report Core enqueued after outbox id
// `since`, in order. The outbox is where the wire starts, so this is what an
// Edge would receive, addressed as it would be addressed.
func sourcingFeed(t *testing.T, db *store.DB, since int64) []feedRow {
	t.Helper()
	rows, err := db.DB.Query(`SELECT station_id, payload FROM outbox WHERE id > $1 ORDER BY id`, since)
	testutil.MustNoErr(t, err, "read the outbox")
	defer rows.Close()
	var out []feedRow
	for rows.Next() {
		var station string
		var payload []byte
		testutil.MustNoErr(t, rows.Scan(&station, &payload), "scan outbox")
		var env struct {
			Payload protocol.Data `json:"p"`
		}
		if json.Unmarshal(payload, &env) != nil || env.Payload.Subject != protocol.SubjectSourcingState {
			continue
		}
		var body protocol.SourcingStateReport
		testutil.MustNoErr(t, json.Unmarshal(env.Payload.Body, &body), "decode a sourcing report")
		seen := map[string]bool{}
		var procs []string
		for _, s := range body.States {
			if !seen[s.ProcessID] {
				seen[s.ProcessID] = true
				procs = append(procs, s.ProcessID)
			}
		}
		sort.Strings(procs)
		out = append(out, feedRow{station: station, snapshot: body.Snapshot, processes: procs})
	}
	testutil.MustNoErr(t, rows.Err(), "outbox rows")
	return out
}

func outboxMax(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&id), "outbox max id")
	return id
}

// seedStationPlant mirrors three processes' claims, all on one payload at the
// storage node: PRESS-A published by station edge-a, PRESS-B by edge-b, and
// PRESS-C with no recorded station (as every process is until its Edge
// republishes after the migration, and as an older Edge's always are).
func seedStationPlant(t *testing.T, db *store.DB) (binID int64) {
	t.Helper()
	storageNode, _, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "BIN-A", storageNode.ID, "feed-a")
	for _, p := range []struct{ process, station string }{
		{"PRESS-A", "edge-a"}, {"PRESS-B", "edge-b"}, {"PRESS-C", ""},
	} {
		_, err := plantclaims.ReplaceProcessFromStation(db.DB, p.process, p.station,
			[]plantclaims.StyleRow{{ProcessID: p.process, StyleID: "S1"}},
			[]plantclaims.ClaimRow{{ProcessID: p.process, StyleID: "S1", CoreNodeName: storageNode.Name, PayloadCode: "BIN-A"}},
			nil, 0)
		testutil.MustNoErr(t, err, "seed "+p.process)
	}
	return bin.ID
}

// feedString renders a feed for a failure message and for comparing.
func feedString(feed []feedRow) string {
	var b []string
	for _, r := range feed {
		kind := "delta"
		if r.snapshot {
			kind = "snapshot"
		}
		b = append(b, r.station+" "+kind+" "+strings.Join(r.processes, ","))
	}
	return strings.Join(b, " | ")
}

// TestSourcingFeed_EachStationGetsItsOwnProcesses is the per-station feed
// (Stage A2, the Pi diet; the mapping is process_styles.station_id by owner
// ruling 2026-09-25). It replaced a characterisation pinned at the base
// (3e9c09b6): every full pass sent a snapshot of the WHOLE plant to every
// station ("*") — PRESS-B's verdicts written on PRESS-A's Pi, where nothing
// reads them. Ruling, brief A2 item 1:
//   - a station's snapshot is its own processes plus every process with no
//     recorded station (so its full replace stays complete for what it reads);
//   - snapshots go out on the first pass, on the 30-minute backstop, on
//     (re)registration, and to a station whose style disappeared;
//   - between backstops a full pass sends only what changed, per station, and a
//     station with no change is sent nothing;
//   - an unrouted process's delta goes to every station, as before.
func TestSourcingFeed_EachStationGetsItsOwnProcesses(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	binID := seedStationPlant(t, db)
	eng := newTestEngine(t, db, simulator.New()) // Start runs the first full pass
	m := eng.SourceabilityMonitor()

	want := "edge-a snapshot PRESS-A,PRESS-C | edge-b snapshot PRESS-B,PRESS-C"
	if got := feedString(sourcingFeed(t, db, 0)); got != want {
		t.Fatalf("the first pass sent [%s], want [%s]: each station its own processes plus the unrouted one, "+
			"and no whole-plant broadcast", got, want)
	}

	// A second full pass inside the backstop, nothing changed: nothing is sent.
	since := outboxMax(t, db)
	m.recomputeAll()
	if got := sourcingFeed(t, db, since); len(got) != 0 {
		t.Fatalf("a quiet full pass inside the backstop sent [%s], want nothing", feedString(got))
	}

	// The backstop falls due: every station gets its snapshot again.
	m.mu.Lock()
	m.lastSnapshot = m.lastSnapshot.Add(-sourceabilitySnapshotInterval)
	m.mu.Unlock()
	since = outboxMax(t, db)
	m.recomputeAll()
	if got := feedString(sourcingFeed(t, db, since)); got != want {
		t.Fatalf("the backstop sent [%s], want [%s]", got, want)
	}

	// A change moves all three verdicts: each goes to its own station, the
	// unrouted one to every station.
	_, err := db.DB.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, binID)
	testutil.MustNoErr(t, err, "drain the pool")
	since = outboxMax(t, db)
	m.recomputeKeys([]plantclaims.ProcessKey{{ProcessID: "PRESS-A", StyleID: "S1"},
		{ProcessID: "PRESS-B", StyleID: "S1"}, {ProcessID: "PRESS-C", StyleID: "S1"}})
	got := sourcingFeed(t, db, since)
	sort.Slice(got, func(i, j int) bool { return got[i].station < got[j].station })
	if s := feedString(got); s != "* delta PRESS-C | edge-a delta PRESS-A | edge-b delta PRESS-B" {
		t.Fatalf("the change sent [%s], want each delta to its own station and the unrouted one to every station", s)
	}

	// An Edge re-registers: it gets its own snapshot at once.
	since = outboxMax(t, db)
	m.SnapshotStation("edge-a")
	if s := feedString(sourcingFeed(t, db, since)); s != "edge-a snapshot PRESS-A,PRESS-C" {
		t.Fatalf("the registration snapshot was [%s], want edge-a's own", s)
	}

	// PRESS-B's style disappears: a delta cannot say so, so edge-b gets a
	// snapshot now, without it.
	_, err = plantclaims.ReplaceProcessFromStation(db.DB, "PRESS-B", "edge-b", nil, nil, nil, 0)
	testutil.MustNoErr(t, err, "withdraw PRESS-B's styles")
	since = outboxMax(t, db)
	m.recomputeAll()
	if s := feedString(sourcingFeed(t, db, since)); s != "edge-b snapshot PRESS-C" {
		t.Fatalf("after PRESS-B's style went, the feed was [%s], want edge-b's snapshot without it", s)
	}
}

// TestSourcingFeed_UnreadableStationsFallBackToTheWholePlant: if the stations
// cannot be listed, the snapshot goes to every station with the whole plant —
// today's snapshot, which every Edge applies correctly — rather than to nobody.
func TestSourcingFeed_UnreadableStationsFallBackToTheWholePlant(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	seedStationPlant(t, db)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := eng.SourceabilityMonitor()
	m.stationsFn = func() ([]string, error) { return nil, errors.New("registry unreadable") }
	since := outboxMax(t, db)
	m.recomputeAll()
	if s := feedString(sourcingFeed(t, db, since)); s != "* snapshot PRESS-A,PRESS-B,PRESS-C" {
		t.Fatalf("with the stations unreadable the snapshot was [%s], want the whole plant to every station", s)
	}
}

// TestPlantClaims_TheLastPublishingStationIsKept: the mirror records the
// station that published a process; a second station publishing the same
// process is reported back (the ingest logs it) and becomes the recorded one —
// the last writer, owner ruling 2026-09-25.
func TestPlantClaims_TheLastPublishingStationIsKept(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	style := []plantclaims.StyleRow{{ProcessID: "SNF9", StyleID: "S1"}}
	prev, err := plantclaims.ReplaceProcessFromStation(db.DB, "SNF9", "edge-a", style, nil, nil, 0)
	testutil.MustNoErr(t, err, "first publish")
	if prev != "" {
		t.Fatalf("first publish reported previous station %q, want none", prev)
	}
	prev, err = plantclaims.ReplaceProcessFromStation(db.DB, "SNF9", "edge-b", style, nil, nil, 0)
	testutil.MustNoErr(t, err, "second station publishes the same process")
	if prev != "edge-a" {
		t.Fatalf("second publish reported previous station %q, want edge-a (so the ingest can name both)", prev)
	}
	stations, err := plantclaims.ProcessStations(db.DB)
	testutil.MustNoErr(t, err, "read stations")
	if stations["SNF9"] != "edge-b" {
		t.Fatalf("SNF9 is recorded against %q, want edge-b — the last writer", stations["SNF9"])
	}
}
