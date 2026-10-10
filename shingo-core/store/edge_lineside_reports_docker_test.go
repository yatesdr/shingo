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
// in time. The row's moved answer is what gates the comparison on ingest, so a
// late report written over a current one would also re-open divergences the
// current one had closed.
//
// Replay is not hypothetical: the outbox is at-least-once, and requeueing a
// dead letter re-delivers whatever it was carrying.

// reportRows reads every edge_lineside_reports row for a payload. The store
// has no reader for this table any more (nothing on a fire path reads it), so
// the tests read it directly.
func reportRows(t *testing.T, db *store.DB, payload string) []store.EdgeLinesideReport {
	t.Helper()
	rows, err := db.Query(`SELECT station, core_node_name, payload_code, reported_at
		FROM edge_lineside_reports WHERE payload_code = $1`, payload)
	if err != nil {
		t.Fatalf("list reports for %s: %v", payload, err)
	}
	defer rows.Close()
	var out []store.EdgeLinesideReport
	for rows.Next() {
		var r store.EdgeLinesideReport
		if err := rows.Scan(&r.Station, &r.CoreNodeName, &r.PayloadCode, &r.ReportedAt); err != nil {
			t.Fatalf("scan report row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("report rows: %v", err)
	}
	return out
}

func linesideRow(t *testing.T, db *store.DB, station, node, payload string) store.EdgeLinesideReport {
	t.Helper()
	for _, r := range reportRows(t, db, payload) {
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

	// write returns whether the row moved: the handler compares a report only
	// when a row moved, so a wrong answer here either re-runs the comparison on
	// stale values or stops a fresh report from being compared at all.
	write := func(at time.Time) bool {
		t.Helper()
		moved, err := db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: node,
			PayloadCode:  payload,
			ReportedAt:   at,
		})
		if err != nil {
			t.Fatalf("upsert at %v: %v", at, err)
		}
		return moved
	}

	// Current report.
	if !write(now) {
		t.Error("first write reported not moved — an insert moves the row")
	}

	// A stale report arrives afterwards — a replay, or a reordered delivery.
	// It must not land.
	if write(now.Add(-time.Minute)) {
		t.Error("STALE write reported moved")
	}
	got := linesideRow(t, db, station, node, payload)
	if !got.ReportedAt.UTC().Equal(now) {
		t.Errorf("reported_at = %v after a STALE write, want %v — the row must "+
			"never move backwards in time", got.ReportedAt.UTC(), now)
	}

	// An exact duplicate is a no-op: strict `<`, so it neither errors nor
	// rewrites the row.
	if write(now) {
		t.Error("same-timestamp write reported moved")
	}

	// A genuinely newer report still lands.
	newer := now.Add(time.Minute)
	if !write(newer) {
		t.Error("NEWER write reported not moved")
	}
	got = linesideRow(t, db, station, node, payload)
	if !got.ReportedAt.UTC().Equal(newer) {
		t.Errorf("reported_at = %v after a NEWER write, want %v — latest-wins must "+
			"not become never-updates", got.ReportedAt.UTC(), newer)
	}
}

// A row nothing will ever update again has to age out on its own. Latest-wins
// cannot clear it — there is no newer report coming — so Springfield carried
// two rows for a station id that no longer exists, 570 hours after the last
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
		if _, err := db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: node,
			PayloadCode:  payload,
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

	survived := map[string]bool{}
	for _, r := range reportRows(t, db, payload) {
		if r.Station == station {
			survived[r.CoreNodeName] = true
		}
	}

	if survived["ALN_ANCIENT"] {
		t.Error("a row 8 days stale survived the 7-day cutoff — nothing else will " +
			"ever clear it, and it keeps the seat in the station's checked set")
	}
	if !survived["ALN_RECENT"] {
		t.Error("a row 6 days stale was purged — inside the window, and a station " +
			"that comes back must find its row intact")
	}
	if !survived["ALN_FRESH"] {
		t.Error("a row reported a minute ago was purged")
	}
}
