// Package counters holds counter-snapshot, hourly-count, and
// reporting-point persistence for shingo-edge. All three sit on the
// same PLC-counter aggregate: a reporting_point points at a PLC tag, a
// counter_snapshot records each poll, and hourly_counts roll the
// per-poll deltas up by hour for reporting.
//
// Phase 5b of the architecture plan moved this CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// one-line delegate methods on *store.DB; callers name counters.Snapshot,
// counters.HourlyCount and counters.ReportingPoint directly.
package counters

import (
	"database/sql"
	"fmt"
	"time"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// HourlyCount and ReportingPoint are the counter-aggregate data types.
// The structs live in shingoedge/domain (Stage 2A.2); these aliases keep
// the unprefixed counters.X names used by every scan helper,
// Insert/Upsert call site, and the outer store/ re-exports.
//
// There is no snapshot struct: the only read that scanned a whole
// counter_snapshots row was the unconfirmed-jump list behind the navbar
// bell, deleted with it (close-out 2b). The shipper reads its own
// projection (ShippableTick).
type (
	HourlyCount    = domain.HourlyCount
	ReportingPoint = domain.ReportingPoint
)

// --- counter snapshots ---

// TickStamp is what the poll knows about a tick at stroke time and the
// snapshot row keeps for the production tick shipper: the Go clock read just
// before the INSERT, and the reporting point's process and style. A zero
// RecordedAt stores NULL (a row the shipper's filter never selects).
type TickStamp struct {
	RecordedAt time.Time
	ProcessID  int64
	StyleID    int64
}

// InsertSnapshot writes one counter_snapshots row, stamp included, in one
// statement.
//
// operator_confirmed is written 1 on every row. Nothing reads it any more: a
// jump is counted at the poll like any delta, so there is nothing to confirm.
// The column stays because SQLite keeps it, and 1 is the value that keeps a
// previous build honest after a rollback — that build's navbar bell lists
// `anomaly = 'jump' AND operator_confirmed = 0`, and a Confirm there would
// release units this build already counted.
func InsertSnapshot(db *sql.DB, rpID int64, countValue, delta int64, anomaly string, stamp TickStamp) (int64, error) {
	var anomalyPtr *string
	if anomaly != "" {
		anomalyPtr = &anomaly
	}
	var recordedMS *int64
	if !stamp.RecordedAt.IsZero() {
		ms := stamp.RecordedAt.UnixMilli()
		recordedMS = &ms
	}
	res, err := db.Exec(`INSERT INTO counter_snapshots
		(reporting_point_id, count_value, delta, anomaly, operator_confirmed, recorded_ms, process_id, style_id)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?)`,
		rpID, countValue, delta, anomalyPtr, recordedMS, stamp.ProcessID, stamp.StyleID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// --- hourly counts ---

// DateLayout is the shape a plant-local calendar date is rendered in.
//
// NOTHING STORED IS KEYED BY IT. It names a day only on the way out — in
// DayBounds, which turns a date into the UTC range covering it. The one table
// still holding dates in this shape is hourly_counts_local_legacy, the parked
// pre-2026-09 hours, which nothing reads or writes.
const DateLayout = "2006-01-02"

// HourBucket is the single definition of which bucket an instant belongs to:
// the start of its UTC hour, in unix seconds. Writer and reader both go
// through this, so they cannot disagree about a boundary.
func HourBucket(t time.Time) int64 {
	return t.UTC().Truncate(time.Hour).Unix()
}

// DayBounds renders a plant-local calendar day as the half-open UTC range
// [from, to) that covers it.
//
// THIS IS WHERE THE PLANT ZONE ENTERS, and the only place it does on the read
// path. time.Date resolves the local midnights, so a DST day is 23 or 25 hours
// wide and the range is still exactly that day — which is the property local
// bucketing could not hold: it had no way to represent a 25-hour day, so the
// repeated hour collided on the unique key and summed.
func DayBounds(countDate string, loc *time.Location) (int64, int64, error) {
	d, err := time.ParseInLocation(DateLayout, countDate, loc)
	if err != nil {
		return 0, 0, fmt.Errorf("day bounds %q: %w", countDate, err)
	}
	return d.UTC().Unix(), d.AddDate(0, 0, 1).UTC().Unix(), nil
}

// UpsertHourly adds delta to the UTC hour bucket, or inserts it if new.
func UpsertHourly(db *sql.DB, processID, styleID, bucketStart, delta int64) error {
	_, err := db.Exec(
		`INSERT INTO hourly_counts (process_id, style_id, bucket_start, delta)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(process_id, style_id, bucket_start)
		 DO UPDATE SET delta = delta + excluded.delta, updated_at = datetime('now')`,
		processID, styleID, bucketStart, delta,
	)
	return err
}

// ListHourly returns the hourly rows for one process/style whose buckets fall
// in the half-open UTC range [from, to).
func ListHourly(db *sql.DB, processID, styleID, from, to int64) ([]HourlyCount, error) {
	rows, err := db.Query(
		`SELECT id, process_id, style_id, bucket_start, delta
		 FROM hourly_counts
		 WHERE process_id = ? AND style_id = ?
		   AND bucket_start >= ? AND bucket_start < ?
		 ORDER BY bucket_start`,
		processID, styleID, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []HourlyCount
	for rows.Next() {
		var c HourlyCount
		if err := rows.Scan(&c.ID, &c.ProcessID, &c.StyleID, &c.BucketStart, &c.Delta); err != nil {
			return nil, err
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// HourlyTotals returns per-bucket totals for a process over the half-open UTC
// range [from, to), summed across styles and keyed by bucket_start. Callers
// that want plant-local hours map the keys through the plant zone — see
// service.CounterService.HourlyTotals.
func HourlyTotals(db *sql.DB, processID, from, to int64) (map[int64]int64, error) {
	rows, err := db.Query(
		`SELECT bucket_start, SUM(delta) FROM hourly_counts
		 WHERE process_id = ? AND bucket_start >= ? AND bucket_start < ?
		 GROUP BY bucket_start ORDER BY bucket_start`,
		processID, from, to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := make(map[int64]int64)
	for rows.Next() {
		var bucket, sum int64
		if err := rows.Scan(&bucket, &sum); err != nil {
			return nil, err
		}
		totals[bucket] = sum
	}
	return totals, rows.Err()
}

// --- reporting points ---

func scanReportingPoint(rp *ReportingPoint, scanner interface{ Scan(...any) error }) error {
	var lastPollAt sql.NullString
	if err := scanner.Scan(&rp.ID, &rp.PLCName, &rp.TagName, &rp.StyleID, &rp.LastCount, &lastPollAt, &rp.Enabled, &rp.WarlinkManaged); err != nil {
		return err
	}
	rp.LastPollAt = helpers.ScanTimePtr(lastPollAt)
	return nil
}

// ListReportingPoints returns every reporting_point row.
func ListReportingPoints(db *sql.DB) ([]ReportingPoint, error) {
	// Join styles to resolve process_id (same as ListEnabledReportingPoints).
	// Without it ProcessID stays 0, and the Q-034 cell catalog (BuildCellCatalog,
	// fed by this list) reports every cell with process_id=0 — so the auto-derived
	// heartbeat tiles query process_id=0, match no cell_part_events (keyed by the
	// real process_id) and render "No data" despite live production. The whole
	// no-manual-setup auto-populate path depends on this resolving.
	rows, err := db.Query(`SELECT rp.id, rp.plc_name, rp.tag_name, rp.style_id, rp.last_count, rp.last_poll_at, rp.enabled, rp.warlink_managed, COALESCE(js.process_id, 0)
		FROM reporting_points rp
		LEFT JOIN styles js ON js.id = rp.style_id
		ORDER BY rp.plc_name, rp.tag_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReportingPointsWithLine(rows)
}

// ListEnabledReportingPoints returns every enabled reporting_point,
// joined to styles so callers get the process_id without a per-poll
// lookup.
func ListEnabledReportingPoints(db *sql.DB) ([]ReportingPoint, error) {
	rows, err := db.Query(`SELECT rp.id, rp.plc_name, rp.tag_name, rp.style_id, rp.last_count, rp.last_poll_at, rp.enabled, rp.warlink_managed, COALESCE(js.process_id, 0)
		FROM reporting_points rp
		LEFT JOIN styles js ON js.id = rp.style_id
		WHERE rp.enabled = 1
		ORDER BY rp.plc_name, rp.tag_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReportingPointsWithLine(rows)
}

func scanReportingPointsWithLine(rows *sql.Rows) ([]ReportingPoint, error) {
	var rps []ReportingPoint
	for rows.Next() {
		var rp ReportingPoint
		var lastPollAt sql.NullString
		if err := rows.Scan(&rp.ID, &rp.PLCName, &rp.TagName, &rp.StyleID, &rp.LastCount, &lastPollAt, &rp.Enabled, &rp.WarlinkManaged, &rp.ProcessID); err != nil {
			return nil, err
		}
		rp.LastPollAt = helpers.ScanTimePtr(lastPollAt)
		rps = append(rps, rp)
	}
	return rps, rows.Err()
}

// GetReportingPoint returns one reporting_point by id.
func GetReportingPoint(db *sql.DB, id int64) (*ReportingPoint, error) {
	rp := &ReportingPoint{}
	if err := scanReportingPoint(rp, db.QueryRow(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points WHERE id = ?`, id)); err != nil {
		return nil, err
	}
	return rp, nil
}

// CreateReportingPoint inserts a reporting_point row.
func CreateReportingPoint(db *sql.DB, plcName, tagName string, styleID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO reporting_points (plc_name, tag_name, style_id) VALUES (?, ?, ?)`, plcName, tagName, styleID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateReportingPoint modifies a reporting_point row.
func UpdateReportingPoint(db *sql.DB, id int64, plcName, tagName string, styleID int64, enabled bool) error {
	_, err := db.Exec(`UPDATE reporting_points SET plc_name=?, tag_name=?, style_id=?, enabled=? WHERE id=?`, plcName, tagName, styleID, enabled, id)
	return err
}

// UpdateReportingPointCounter writes the latest counter value and poll
// time for a reporting_point.
func UpdateReportingPointCounter(db *sql.DB, id int64, count int64) error {
	_, err := db.Exec(`UPDATE reporting_points SET last_count=?, last_poll_at=datetime('now') WHERE id=?`, count, id)
	return err
}

// DeleteReportingPoint removes a reporting_point row.
func DeleteReportingPoint(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM reporting_points WHERE id=?`, id)
	return err
}

// SetReportingPointManaged toggles the warlink_managed flag.
func SetReportingPointManaged(db *sql.DB, id int64, managed bool) error {
	_, err := db.Exec(`UPDATE reporting_points SET warlink_managed=? WHERE id=?`, managed, id)
	return err
}

// GetReportingPointByTag looks up a reporting_point by its (plc_name,
// tag_name) pair.
func GetReportingPointByTag(db *sql.DB, plcName, tagName string) (*ReportingPoint, error) {
	rp := &ReportingPoint{}
	if err := scanReportingPoint(rp, db.QueryRow(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points WHERE plc_name = ? AND tag_name = ? LIMIT 1`, plcName, tagName)); err != nil {
		return nil, err
	}
	return rp, nil
}

// GetReportingPointByStyleID looks up a reporting_point by style_id.
func GetReportingPointByStyleID(db *sql.DB, styleID int64) (*ReportingPoint, error) {
	rp := &ReportingPoint{}
	if err := scanReportingPoint(rp, db.QueryRow(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points WHERE style_id = ? LIMIT 1`, styleID)); err != nil {
		return nil, err
	}
	return rp, nil
}
