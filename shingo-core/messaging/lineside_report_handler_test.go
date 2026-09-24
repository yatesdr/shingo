//go:build docker

package messaging

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

// lineside_report_handler_test.go — what the handler stores, and what one
// report costs.
//
// The report is a checksum: the handler upserts each row (latest-wins on
// reported_at) and, when a row moved, compares the report against Core's
// replica (lineside_divergence_test.go pins the comparison). It triggers no
// evaluation: the fire paths read Core's count and nothing else.
//
// These tests used to pin which payloads the handler passed to the monitor's
// OnLinesideReports (round 4 lane D). That trigger is deleted with the ruling
// that decisions read Core's count; the row-level halves of those tests, which
// the comparison's gate still stands on, are kept here.

func newLinesideService(t *testing.T, db *store.DB) *CoreDataService {
	t.Helper()
	return NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
}

func linesideEnvelope(station string) *protocol.Envelope {
	return &protocol.Envelope{
		ID:   "env-lineside-" + station,
		Type: protocol.TypeData,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: station},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
}

func linesideEntry(node, payload string, binUOP int) protocol.LinesideLevelEntry {
	return protocol.LinesideLevelEntry{CoreNodeName: node, PayloadCode: payload, BinCount: 1, BinUOP: binUOP}
}

// storedReportedAt returns the reported_at of the station's row for (node,
// payload), and whether the row exists.
func storedReportedAt(t *testing.T, db *store.DB, station, node, payload string) (time.Time, bool) {
	t.Helper()
	var at time.Time
	err := db.QueryRow(`SELECT reported_at FROM edge_lineside_reports
		WHERE station = $1 AND core_node_name = $2 AND payload_code = $3`, station, node, payload).Scan(&at)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// A NEWER report carrying exactly the values already stored still moves the row
// (reported_at advances). The comparison runs only for a report that moved a
// row, so this is what keeps a quiet line's seats checked: a line that has not
// moved for an hour is still reporting, and still compared.
func TestLinesideReport_NewerIdenticalValuesStillMovesTheRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newLinesideService(t, db)

	const (
		station = "stn-lineside-refresh"
		node    = "ALN_002"
		payload = "P-REFRESH"
	)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	entries := []protocol.LinesideLevelEntry{linesideEntry(node, payload, 25)}

	svc.HandleLinesideLevelReport(linesideEnvelope(station), &protocol.LinesideLevelReport{Station: station, ReportedAt: t0, Entries: entries})
	t1 := t0.Add(time.Minute)
	svc.HandleLinesideLevelReport(linesideEnvelope(station), &protocol.LinesideLevelReport{Station: station, ReportedAt: t1, Entries: entries})

	if at, ok := storedReportedAt(t, db, station, node, payload); !ok || !at.UTC().Equal(t1) {
		t.Errorf("reported_at = %v (row present %v), want %v — the refresh must move the row", at, ok, t1)
	}
}

// An upsert that fails skips that entry and only that entry: the others are
// still written. The failure is forced with a value the INTEGER column cannot
// hold. The failed entry is also left out of the comparison.
func TestLinesideReport_UpsertErrorSkipsOnlyThatEntry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newLinesideService(t, db)

	const station = "stn-lineside-err"
	svc.HandleLinesideLevelReport(linesideEnvelope(station), &protocol.LinesideLevelReport{
		Station:    station,
		ReportedAt: time.Now().UTC().Truncate(time.Millisecond),
		Entries: []protocol.LinesideLevelEntry{
			linesideEntry("ALN_003", "P-ERR-BAD", 1<<40),
			linesideEntry("ALN_004", "P-ERR-GOOD", 30),
		},
	})

	if _, ok := storedReportedAt(t, db, station, "ALN_003", "P-ERR-BAD"); ok {
		t.Error("the bad entry was stored")
	}
	if _, ok := storedReportedAt(t, db, station, "ALN_004", "P-ERR-GOOD"); !ok {
		t.Error("the good entry was not stored")
	}
}

// WHAT ONE REPORT COSTS CORE. One upsert per well-formed entry, whatever the
// delivery turns out to be; a blank entry costs nothing. A report that moved a
// row then costs the comparison's two reads — Core's side of the report (the
// named carriers with their dedup rows, the counted carriers at the station's
// seats, and the bucket mirror, as one statement) and the station's open
// episodes — whatever its row count, plus, only when an episode opens or
// closes, one transaction: an INSERT per opened episode and one UPDATE for all
// the closed ones. A redelivery moves no row and costs the upserts only.
//
// Before lane A of the memory build this was 3 on both deliveries (the upserts;
// the evaluation it triggered was the monitor's cost, 5-7 statements per
// payload, and is gone).
func TestLinesideReport_StatementsPerEnvelope(t *testing.T) {
	t.Parallel()
	_, cfg := testdb.OpenWithConfig(t)
	cdb, counter, err := store.OpenCounting(cfg)
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	svc := newLinesideService(t, cdb)

	const station = "stn-lineside-count"
	r := &protocol.LinesideLevelReport{
		Station:    station,
		ReportedAt: time.Now().UTC().Truncate(time.Millisecond),
		Entries: []protocol.LinesideLevelEntry{
			linesideEntry("ALN_005", "P-COUNT-A", 10),
			linesideEntry("ALN_006", "P-COUNT-A", 11),
			linesideEntry("ALN_007", "P-COUNT-B", 12),
			{CoreNodeName: "", PayloadCode: "P-COUNT-C"},
		},
	}
	for _, c := range []struct {
		delivery string
		want     int64
	}{{"first delivery", 5}, {"redelivery", 3}} {
		counter.Reset()
		svc.HandleLinesideLevelReport(linesideEnvelope(station), r)
		if got := counter.Count(); got != c.want {
			t.Errorf("%s: %d statements, want %d", c.delivery, got, c.want)
		}
	}

	// A newer report whose one bucket disagrees with Core's (empty) mirror
	// opens one episode: the same 3 + 2, then the transition's transaction —
	// BEGIN, one INSERT, COMMIT. The counter sees the BEGIN and COMMIT
	// (store/query_count.go).
	r.ReportedAt = r.ReportedAt.Add(time.Minute)
	r.Entries[0].BucketQty = 40
	counter.Reset()
	svc.HandleLinesideLevelReport(linesideEnvelope(station), r)
	if got := counter.Count(); got != 8 {
		t.Errorf("a report opening one episode: %d statements, want 8", got)
	}

	// A newer report with no rows (the station runs no consume seat now) costs
	// one read of the station's latest row instead of the upserts, then the
	// comparison's two reads, and closes the episode: BEGIN, one UPDATE,
	// COMMIT. The next one finds nothing to close and costs the three reads.
	empty := &protocol.LinesideLevelReport{Station: station, ReportedAt: r.ReportedAt.Add(time.Minute)}
	for _, c := range []struct {
		report string
		want   int64
	}{{"no rows, closing one episode", 6}, {"no rows, nothing to close", 3}} {
		counter.Reset()
		svc.HandleLinesideLevelReport(linesideEnvelope(station), empty)
		if got := counter.Count(); got != c.want {
			t.Errorf("%s: %d statements, want %d", c.report, got, c.want)
		}
		empty.ReportedAt = empty.ReportedAt.Add(time.Minute)
	}
}
