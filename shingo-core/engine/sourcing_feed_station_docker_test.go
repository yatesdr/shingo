//go:build docker

package engine

import (
	"encoding/json"
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
//   - the snapshot is what the heartbeat compare sends (StationReport, the
//     FeedSourcing row) when the Edge's digest differs, and the monitor
//     itself pushes one only to a station whose style disappeared. The first
//     pass's snapshot to every station, the 30-minute backstop and the
//     registration snapshot went with the feed's descriptor row;
//   - a full pass sends only what changed, per station, and a station with no
//     change is sent nothing;
//   - an unrouted process's delta goes to every station, as before.
func TestSourcingFeed_EachStationGetsItsOwnProcesses(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	binID := seedStationPlant(t, db)
	eng := newTestEngine(t, db, simulator.New()) // Start runs the first full pass
	m := eng.SourceabilityMonitor()

	if got := sourcingFeed(t, db, 0); len(got) != 0 {
		t.Fatalf("the first pass sent [%s], want nothing: each Edge's cache is the heartbeat compare's", feedString(got))
	}
	report := func(station string) string {
		t.Helper()
		r, ok := m.StationReport(station)
		if !ok {
			t.Fatalf("StationReport(%s) not ready after the first pass", station)
		}
		seen := map[string]bool{}
		var procs []string
		for _, st := range r.States {
			if !seen[st.ProcessID] {
				seen[st.ProcessID] = true
				procs = append(procs, st.ProcessID)
			}
		}
		sort.Strings(procs)
		return station + " snapshot " + strings.Join(procs, ",")
	}
	if got := report("edge-a") + " | " + report("edge-b"); got != "edge-a snapshot PRESS-A,PRESS-C | edge-b snapshot PRESS-B,PRESS-C" {
		t.Fatalf("the stations' snapshots are [%s], want each its own processes plus the unrouted one", got)
	}

	// A second full pass, nothing changed: nothing is sent.
	since := outboxMax(t, db)
	m.recomputeAll()
	if got := sourcingFeed(t, db, since); len(got) != 0 {
		t.Fatalf("a quiet full pass sent [%s], want nothing", feedString(got))
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
