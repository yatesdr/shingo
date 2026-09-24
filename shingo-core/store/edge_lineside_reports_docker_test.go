//go:build docker

package store_test

import (
	"database/sql"
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
	rows, err := db.Query(`SELECT station, core_node_name, payload_code, bin_count, bin_uop, bucket_qty,
		       bin_id, COALESCE(bin_epoch, 0), COALESCE(flushed_seq, 0), reported_at
		FROM edge_lineside_reports WHERE payload_code = $1`, payload)
	if err != nil {
		t.Fatalf("list reports for %s: %v", payload, err)
	}
	defer rows.Close()
	var out []store.EdgeLinesideReport
	for rows.Next() {
		var r store.EdgeLinesideReport
		var bin sql.NullInt64
		if err := rows.Scan(&r.Station, &r.CoreNodeName, &r.PayloadCode, &r.BinCount, &r.BinUOP, &r.BucketQty,
			&bin, &r.BinEpoch, &r.FlushedSeq, &r.ReportedAt); err != nil {
			t.Fatalf("scan report row: %v", err)
		}
		if bin.Valid {
			v := bin.Int64
			r.BinID = &v
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
	write := func(at time.Time, binUOP int) bool {
		t.Helper()
		moved, err := db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: node,
			PayloadCode:  payload,
			BinCount:     1,
			BinUOP:       binUOP,
			BucketQty:    0,
			ReportedAt:   at,
		})
		if err != nil {
			t.Fatalf("upsert at %v: %v", at, err)
		}
		return moved
	}

	// Current report.
	if !write(now, 46) {
		t.Error("first write reported not moved — an insert moves the row")
	}
	if got := linesideRow(t, db, station, node, payload); got.BinUOP != 46 {
		t.Fatalf("bin_uop = %d after first write, want 46", got.BinUOP)
	}

	// A stale report arrives afterwards — a replay, or a reordered delivery.
	// It must not land.
	if write(now.Add(-time.Minute), 150) {
		t.Error("STALE write reported moved")
	}
	got := linesideRow(t, db, station, node, payload)
	if !got.ReportedAt.UTC().Equal(now) {
		t.Errorf("reported_at = %v after a STALE write, want %v — the row must "+
			"never move backwards in time", got.ReportedAt.UTC(), now)
	}
	if got.BinUOP != 46 {
		t.Errorf("bin_uop = %d after a STALE write, want 46 — the older report's "+
			"values must not overwrite the current ones", got.BinUOP)
	}

	// An exact duplicate is a no-op: strict `<`, so it neither errors nor
	// rewrites the row.
	if write(now, 999) {
		t.Error("same-timestamp write reported moved")
	}
	if got := linesideRow(t, db, station, node, payload); got.BinUOP != 46 {
		t.Errorf("bin_uop = %d after a same-timestamp write, want 46 — an exact "+
			"duplicate must be a no-op", got.BinUOP)
	}

	// A genuinely newer report still lands.
	newer := now.Add(time.Minute)
	if !write(newer, 12) {
		t.Error("NEWER write reported not moved")
	}
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

// THE CARRIER COLUMNS (v127) round-trip, and an old Edge's row stores NULL for
// all three: that NULL is how the comparison on ingest knows not to compare a
// carrier for it.
func TestUpsertEdgeLinesideReport_CarrierColumns(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const payload = "P-CARRIER-COLS"
	now := time.Now().UTC().Truncate(time.Millisecond)
	bin := int64(4242)
	for _, r := range []store.EdgeLinesideReport{
		{Station: "stn-new", CoreNodeName: "ALN_001", PayloadCode: payload, BinCount: 1, BinUOP: 40,
			BinID: &bin, BinEpoch: 3, FlushedSeq: 17, ReportedAt: now},
		{Station: "stn-old", CoreNodeName: "ALN_001", PayloadCode: payload, BinCount: 1, BinUOP: 40, ReportedAt: now},
	} {
		if _, err := db.UpsertEdgeLinesideReport(r); err != nil {
			t.Fatalf("upsert %s: %v", r.Station, err)
		}
	}
	got := linesideRow(t, db, "stn-new", "ALN_001", payload)
	if got.BinID == nil || *got.BinID != bin || got.BinEpoch != 3 || got.FlushedSeq != 17 {
		t.Errorf("new Edge row = bin %v epoch %d flushed %d, want 4242/3/17", got.BinID, got.BinEpoch, got.FlushedSeq)
	}
	var nulls int
	if err := db.QueryRow(`SELECT count(*) FROM edge_lineside_reports
		WHERE station = 'stn-old' AND bin_id IS NULL AND bin_epoch IS NULL AND flushed_seq IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("read old row: %v", err)
	}
	if nulls != 1 {
		t.Error("an old Edge's row did not store NULL carrier columns")
	}
}
