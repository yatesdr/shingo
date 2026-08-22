//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// edge_lineside_reports_docker_test.go — the upsert is latest-wins.
//
// UpsertEdgeLinesideReport used to overwrite unconditionally, including
// reported_at. That let a replayed or out-of-order report move a row BACKWARDS
// in time, and reported_at is what the threshold monitor's freshness test reads
// (linesideReportStaleness, 3 minutes). A row pushed behind that window stops
// contributing its adjustment, so the node falls back to the pure ledger — the
// "ledger reads STOCKED while the line starves" case the R1 read-model exists
// to correct, reintroduced by the write path.
//
// Replay is not hypothetical: the outbox is at-least-once, and requeueing a
// dead letter re-delivers whatever it was carrying.

func linesideRow(t *testing.T, db *store.DB, station, node, payload string) store.EdgeLinesideReport {
	t.Helper()
	rows, err := db.ListLinesideReportsForPayload(payload)
	if err != nil {
		t.Fatalf("list reports for %s: %v", payload, err)
	}
	for _, r := range rows {
		if r.Station == station && r.CoreNodeName == node {
			return r
		}
	}
	t.Fatalf("no edge_lineside_reports row for %s/%s/%s", station, node, payload)
	return store.EdgeLinesideReport{}
}

func TestUpsertEdgeLinesideReport_LatestWins(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	const (
		station = "station-latest-wins"
		node    = "ALN_001"
		payload = "P-LATEST-WINS"
	)
	now := time.Now().UTC().Truncate(time.Millisecond)

	write := func(at time.Time, binUOP int) {
		t.Helper()
		if err := db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: node,
			PayloadCode:  payload,
			BinCount:     1,
			BinUOP:       binUOP,
			BucketQty:    0,
			ReportedAt:   at,
		}); err != nil {
			t.Fatalf("upsert at %v: %v", at, err)
		}
	}

	// Current report.
	write(now, 46)
	if got := linesideRow(t, db, station, node, payload); got.BinUOP != 46 {
		t.Fatalf("bin_uop = %d after first write, want 46", got.BinUOP)
	}

	// A stale report arrives afterwards — a replay, or a reordered delivery.
	// It must not land.
	write(now.Add(-time.Minute), 150)
	got := linesideRow(t, db, station, node, payload)
	if !got.ReportedAt.UTC().Equal(now) {
		t.Errorf("reported_at = %v after a STALE write, want %v — moving the row "+
			"backwards ages it past the staleness window and drops the node out "+
			"of the adjustment", got.ReportedAt.UTC(), now)
	}
	if got.BinUOP != 46 {
		t.Errorf("bin_uop = %d after a STALE write, want 46 — the older report's "+
			"values must not overwrite the current ones", got.BinUOP)
	}

	// An exact duplicate is a no-op: strict `<`, so it neither errors nor
	// rewrites the row.
	write(now, 999)
	if got := linesideRow(t, db, station, node, payload); got.BinUOP != 46 {
		t.Errorf("bin_uop = %d after a same-timestamp write, want 46 — an exact "+
			"duplicate must be a no-op", got.BinUOP)
	}

	// A genuinely newer report still lands.
	newer := now.Add(time.Minute)
	write(newer, 12)
	got = linesideRow(t, db, station, node, payload)
	if !got.ReportedAt.UTC().Equal(newer) {
		t.Errorf("reported_at = %v after a NEWER write, want %v — latest-wins must "+
			"not become never-updates", got.ReportedAt.UTC(), newer)
	}
	if got.BinUOP != 12 {
		t.Errorf("bin_uop = %d after a NEWER write, want 12", got.BinUOP)
	}
}

// A row nothing will ever update again has to age out on its own. Latest-wins
// cannot clear it — there is no newer report coming — so Springfield carried
// two rows for station stn-4c2bcb20ebfac90f, an id that no longer exists,
// logging an R1 STALE fallback on every monitor cycle 570 hours after the last
// one arrived.
func TestPurgeStaleLinesideReports(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	const (
		station = "stn-purge-test"
		payload = "P-PURGE-TEST"
	)
	now := time.Now().UTC()

	write := func(node string, age time.Duration) {
		t.Helper()
		if err := db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: node,
			PayloadCode:  payload,
			BinCount:     1,
			BinUOP:       10,
			ReportedAt:   now.Add(-age),
		}); err != nil {
			t.Fatalf("seed %s: %v", node, err)
		}
	}
	write("ALN_ANCIENT", 8*24*time.Hour)
	write("ALN_RECENT", 6*24*time.Hour)
	write("ALN_FRESH", time.Minute)

	if _, err := db.PurgeStaleLinesideReports(store.LinesideReportRetentionPeriod); err != nil {
		t.Fatalf("purge: %v", err)
	}

	rows, err := db.ListLinesideReportsForPayload(payload)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	survived := map[string]bool{}
	for _, r := range rows {
		if r.Station == station {
			survived[r.CoreNodeName] = true
		}
	}

	if survived["ALN_ANCIENT"] {
		t.Error("a row 8 days stale survived the 7-day cutoff — nothing else will " +
			"ever clear it, and it logs a fallback on every monitor cycle")
	}
	if !survived["ALN_RECENT"] {
		t.Error("a row 6 days stale was purged — inside the window, and a station " +
			"that comes back must find its row intact")
	}
	if !survived["ALN_FRESH"] {
		t.Error("a row reported a minute ago was purged")
	}
}
