package store

import (
	"fmt"
	"time"
)

// EdgeLinesideReport is one persisted per-(station, node, payload) lineside
// level as Edge reported it — the R1 shadow read-model row. Its own table
// (edge_lineside_reports, v52); nothing here touches bins.uop_remaining.
type EdgeLinesideReport struct {
	Station      string
	CoreNodeName string
	PayloadCode  string
	BinCount     int
	BinUOP       int
	BucketQty    int
	ReportedAt   time.Time
}

// UpsertEdgeLinesideReport writes (or replaces) one edge_lineside_reports row,
// keyed by (station, core_node_name, payload_code). Called by the Core handler
// for each entry of an inbound LinesideLevelReport.
func (db *DB) UpsertEdgeLinesideReport(r EdgeLinesideReport) error {
	_, err := db.Exec(`
		INSERT INTO edge_lineside_reports
			(station, core_node_name, payload_code, bin_count, bin_uop, bucket_qty, reported_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, NOW())
		ON CONFLICT (station, core_node_name, payload_code) DO UPDATE SET
			bin_count   = EXCLUDED.bin_count,
			bin_uop     = EXCLUDED.bin_uop,
			bucket_qty  = EXCLUDED.bucket_qty,
			reported_at = EXCLUDED.reported_at,
			updated_at  = NOW()`,
		r.Station, r.CoreNodeName, r.PayloadCode, r.BinCount, r.BinUOP, r.BucketQty, r.ReportedAt)
	if err != nil {
		return fmt.Errorf("upsert edge_lineside_report %s/%s/%s: %w", r.Station, r.CoreNodeName, r.PayloadCode, err)
	}
	return nil
}

// ListLinesideReportsForPayload returns every edge_lineside_reports row for a
// payload across all stations/nodes. The caller (the shadow) splits fresh from
// stale by comparing reported_at to now − staleness window.
func (db *DB) ListLinesideReportsForPayload(payloadCode string) ([]EdgeLinesideReport, error) {
	rows, err := db.Query(`
		SELECT station, core_node_name, payload_code, bin_count, bin_uop, bucket_qty, reported_at
		FROM edge_lineside_reports
		WHERE payload_code = $1`, payloadCode)
	if err != nil {
		return nil, fmt.Errorf("list edge_lineside_reports for %s: %w", payloadCode, err)
	}
	defer rows.Close()
	var out []EdgeLinesideReport
	for rows.Next() {
		var r EdgeLinesideReport
		if err := rows.Scan(&r.Station, &r.CoreNodeName, &r.PayloadCode,
			&r.BinCount, &r.BinUOP, &r.BucketQty, &r.ReportedAt); err != nil {
			return nil, fmt.Errorf("scan edge_lineside_report: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
