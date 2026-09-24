package store

import (
	"fmt"
	"time"
)

// EdgeLinesideReport is one persisted per-(station, node, payload) lineside
// level as the Edge reported it. Its own table (edge_lineside_reports, v52;
// carrier columns v127); nothing here touches bins.uop_remaining.
//
// A CHECKSUM, NOT A DECISION INPUT. Nothing on a fire path reads this table:
// every replenishment decision reads Core's count (SystemUOPForPayload). The
// report handler compares each report against Core's replica on ingest
// (service/lineside_divergence.go) and records a disagreement as a
// report_divergence episode. The table keeps the latest report per seat, and
// which seats a station has reported is the seat set that comparison checks.
//
// BinID is nil on a row an Edge that predates the carrier keys wrote (and on
// any row with no carrier bound); BinEpoch and FlushedSeq are then unset too.
type EdgeLinesideReport struct {
	Station      string
	CoreNodeName string
	PayloadCode  string
	BinCount     int
	BinUOP       int
	BucketQty    int
	BinID        *int64
	BinEpoch     int64
	FlushedSeq   int64
	ReportedAt   time.Time
}

// UpsertEdgeLinesideReport writes (or replaces) one edge_lineside_reports row,
// keyed by (station, core_node_name, payload_code). Called by the Core handler
// for each entry of an inbound LinesideLevelReport.
//
// LATEST-WINS on Edge's reported_at. The upsert used to overwrite
// unconditionally, which let an out-of-order or replayed report move a row
// BACKWARDS in time. The comparison on ingest runs only for a report that
// moved a row, so an old report written over a current one would also re-open
// divergences a current report had closed.
//
// Replay is not hypothetical. The outbox is at-least-once by construction and
// a requeued dead letter re-delivers whatever it was carrying, so a report from
// an hour ago can arrive after a current one.
//
// Strict `<`, so an exact duplicate is a no-op rather than a pointless write,
// and updated_at stays inside the SET so it only moves when the row does.
//
// moved reports whether the row was inserted or updated — false when the
// condition turned the write into a no-op (a duplicate, an older report, or a
// report stamped at the stored instant). The handler compares a report only
// when at least one of its rows moved, because the inbox dedup does not gate
// data envelopes and this condition is the only thing that tells a redelivery
// or a late report from a current one. RowsAffected reads the count from the
// statement's own CommandComplete tag (pgx stdlib), so it is not another round
// trip.
func (db *DB) UpsertEdgeLinesideReport(r EdgeLinesideReport) (moved bool, err error) {
	var binID, binEpoch, flushed any
	if r.BinID != nil {
		binID, binEpoch, flushed = *r.BinID, r.BinEpoch, r.FlushedSeq
	}
	res, err := db.Exec(`
		INSERT INTO edge_lineside_reports
			(station, core_node_name, payload_code, bin_count, bin_uop, bucket_qty,
			 bin_id, bin_epoch, flushed_seq, reported_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, NOW())
		ON CONFLICT (station, core_node_name, payload_code) DO UPDATE SET
			bin_count   = EXCLUDED.bin_count,
			bin_uop     = EXCLUDED.bin_uop,
			bucket_qty  = EXCLUDED.bucket_qty,
			bin_id      = EXCLUDED.bin_id,
			bin_epoch   = EXCLUDED.bin_epoch,
			flushed_seq = EXCLUDED.flushed_seq,
			reported_at = EXCLUDED.reported_at,
			updated_at  = NOW()
		WHERE edge_lineside_reports.reported_at < EXCLUDED.reported_at`,
		r.Station, r.CoreNodeName, r.PayloadCode, r.BinCount, r.BinUOP, r.BucketQty,
		binID, binEpoch, flushed, r.ReportedAt)
	if err != nil {
		return false, fmt.Errorf("upsert edge_lineside_report %s/%s/%s: %w", r.Station, r.CoreNodeName, r.PayloadCode, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("upsert edge_lineside_report %s/%s/%s: rows affected: %w", r.Station, r.CoreNodeName, r.PayloadCode, err)
	}
	return n > 0, nil
}

// LinesideReportRetentionPeriod is how long a per-(station, node, payload) row
// is kept after its last report.
//
// Seven days: a week is inside any plausible "that station came back" story,
// and a seat a station stopped reporting a week ago is no longer one of the
// seats the comparison on ingest checks for it. The divergence episodes
// themselves live in bin_uop_exception, which has no retention.
//
// It exists because latest-wins cannot clear a row nothing will ever update
// again. Springfield carried two rows for a station id that no longer exists,
// reported 2026-07-29 and still read by every monitor cycle 570 hours later.
const LinesideReportRetentionPeriod = 7 * 24 * time.Hour

// PurgeStaleLinesideReports deletes rows whose last report is older than
// olderThan, and returns how many went.
//
// Deliberately keyed on reported_at (EDGE's clock, the same field the
// latest-wins condition reads) rather than updated_at, so a row that keeps being
// rewritten with an unchanging old timestamp still ages out.
func (db *DB) PurgeStaleLinesideReports(olderThan time.Duration) (int64, error) {
	// Bind a time.Time, not a formatted string: reported_at is TIMESTAMPTZ and
	// a zoneless literal would be compared in the session TimeZone.
	res, err := db.Exec(`DELETE FROM edge_lineside_reports WHERE reported_at < $1`,
		time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("purge stale lineside reports: %w", err)
	}
	return res.RowsAffected()
}
