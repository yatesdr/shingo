// Package counters holds counter-snapshot, hourly-count, and
// reporting-point persistence for shingo-edge. All three sit on the
// same PLC-counter aggregate: a reporting_point points at a PLC tag, a
// counter_snapshot records each poll, and hourly_counts roll the
// per-poll deltas up by hour for reporting.
//
// Phase 5b of the architecture plan moved this CRUD out of the flat
// store/ package and into this sub-package. The outer store/ keeps
// type aliases (`store.CounterSnapshot = counters.Snapshot`,
// `store.HourlyCount = counters.HourlyCount`,
// `store.ReportingPoint = counters.ReportingPoint`) and one-line
// delegate methods on *store.DB so external callers see no API change.
package counters

import (
	"database/sql"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Snapshot, HourlyCount, and ReportingPoint are the counter-aggregate
// data types. The structs live in shingoedge/domain (Stage 2A.2);
// these aliases keep the unprefixed counters.X names used by every
// scan helper, Insert/Upsert call site, and the outer store/
// re-exports.
//
// The domain rename `Snapshot` → `CounterSnapshot` exists so the
// type is self-describing once outside this package; the alias here
// keeps the local counters.Snapshot name for backward compatibility.
type (
	Snapshot       = domain.CounterSnapshot
	HourlyCount    = domain.HourlyCount
	ReportingPoint = domain.ReportingPoint
)

// --- counter snapshots ---

// InsertSnapshot writes one counter_snapshots row.
func InsertSnapshot(db *sql.DB, rpID int64, countValue, delta int64, anomaly string, confirmed bool) (int64, error) {
	var anomalyPtr *string
	if anomaly != "" {
		anomalyPtr = &anomaly
	}
	res, err := db.Exec(`INSERT INTO counter_snapshots (reporting_point_id, count_value, delta, anomaly, operator_confirmed) VALUES (?, ?, ?, ?, ?)`,
		rpID, countValue, delta, anomalyPtr, confirmed)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListUnconfirmedAnomalies returns every counter snapshot tagged as a
// "jump" anomaly that the operator has not yet confirmed.
func ListUnconfirmedAnomalies(db *sql.DB) ([]Snapshot, error) {
	rows, err := db.Query(`
		SELECT id, reporting_point_id, count_value, delta, anomaly, operator_confirmed, recorded_at
		FROM counter_snapshots
		WHERE anomaly = 'jump' AND operator_confirmed = 0
		ORDER BY recorded_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snaps []Snapshot
	for rows.Next() {
		var s Snapshot
		var recordedAt string
		if err := rows.Scan(&s.ID, &s.ReportingPointID, &s.CountValue, &s.Delta, &s.Anomaly, &s.OperatorConfirmed, &recordedAt); err != nil {
			return nil, err
		}
		s.RecordedAt = helpers.ScanTime(recordedAt)
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

// ConfirmedJump carries what a confirmation that actually flipped a row
// hands back: everything the counter-delta path needs to account for the
// units the operator just accepted. ProcessID and StyleID are resolved
// through the reporting point exactly as ListEnabledReportingPoints
// resolves them for the live poll.
type ConfirmedJump struct {
	ReportingPointID int64
	ProcessID        int64
	StyleID          int64
	Delta            int64
	CountValue       int64
}

// ConfirmAnomaly marks an unconfirmed jump snapshot as operator-confirmed
// and returns the row's accounting fields so the caller can release the
// delta downstream. Returns (nil, nil) when nothing was flipped.
//
// The UPDATE is now guarded on `anomaly = 'jump' AND operator_confirmed = 0`
// — the same predicate ListUnconfirmedAnomalies selects on and
// DismissAnomaly deletes on. It was a bare `WHERE id = ?`, which would
// happily "confirm" an ordinary snapshot and reported success when
// re-confirming a row already at 1. Once confirmation has a downstream
// effect, that second case is a double-count: the popover button is a
// plain POST with no client-side debounce (static/js/anomaly-handlers.js,
// confirmAnomaly), so a double-tap or a retried request is the ordinary
// case rather than the exotic one. RowsAffected is the idempotency token
// — only the caller that actually moved the row 0 → 1 gets a
// *ConfirmedJump back.
//
// The read-back deliberately runs outside a transaction. After a winning
// UPDATE the row holds operator_confirmed = 1, which is exactly the state
// that excludes it from a concurrent DismissAnomaly's DELETE and from a
// second ConfirmAnomaly's UPDATE, and nothing else writes these columns.
// So the row this SELECT reads cannot change or vanish underneath it —
// and store.Open pins MaxOpenConns(1) besides.
//
// The style is read from the reporting point AS IT IS NOW, not as it was
// when the jump was recorded: counter_snapshots stores no style. A jump
// confirmed after a changeover is therefore attributed to the new style.
// That is the same identity the live poll path uses and the only one the
// schema can answer — a known limitation, not an oversight.
func ConfirmAnomaly(db *sql.DB, id int64) (*ConfirmedJump, error) {
	res, err := db.Exec(`UPDATE counter_snapshots SET operator_confirmed = 1
		WHERE id = ? AND anomaly = 'jump' AND operator_confirmed = 0`, id)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	var cj ConfirmedJump
	// COALESCE(s.process_id, 0) mirrors ListEnabledReportingPoints: a
	// reporting point whose style row is missing yields process_id 0, which
	// every downstream consumer already treats as unattributable.
	err = db.QueryRow(`SELECT cs.reporting_point_id, cs.delta, cs.count_value,
			COALESCE(s.process_id, 0), rp.style_id
		FROM counter_snapshots cs
		JOIN reporting_points rp ON rp.id = cs.reporting_point_id
		LEFT JOIN styles s ON s.id = rp.style_id
		WHERE cs.id = ?`, id).
		Scan(&cj.ReportingPointID, &cj.Delta, &cj.CountValue, &cj.ProcessID, &cj.StyleID)
	if err != nil {
		return nil, err
	}
	return &cj, nil
}

// DismissAnomaly deletes an unconfirmed anomaly snapshot.
func DismissAnomaly(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM counter_snapshots WHERE id = ? AND anomaly = 'jump' AND operator_confirmed = 0`, id)
	return err
}

// --- hourly counts ---

// UpsertHourly adds delta to the existing count for the given
// process/style/date/hour, or inserts a new row if none exists.
func UpsertHourly(db *sql.DB, processID, styleID int64, countDate string, hour int, delta int64) error {
	_, err := db.Exec(
		`INSERT INTO hourly_counts (process_id, style_id, count_date, hour, delta)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(process_id, style_id, count_date, hour)
		 DO UPDATE SET delta = delta + excluded.delta, updated_at = datetime('now')`,
		processID, styleID, countDate, hour, delta,
	)
	return err
}

// ListHourly returns all hourly count rows for a given
// process/style/date.
func ListHourly(db *sql.DB, processID, styleID int64, countDate string) ([]HourlyCount, error) {
	rows, err := db.Query(
		`SELECT id, process_id, style_id, count_date, hour, delta
		 FROM hourly_counts
		 WHERE process_id = ? AND style_id = ? AND count_date = ?
		 ORDER BY hour`,
		processID, styleID, countDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []HourlyCount
	for rows.Next() {
		var c HourlyCount
		if err := rows.Scan(&c.ID, &c.ProcessID, &c.StyleID, &c.CountDate, &c.Hour, &c.Delta); err != nil {
			return nil, err
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// HourlyTotals returns per-hour totals for a process/date, summed
// across all styles.
func HourlyTotals(db *sql.DB, processID int64, countDate string) (map[int]int64, error) {
	rows, err := db.Query(
		`SELECT hour, SUM(delta) FROM hourly_counts
		 WHERE process_id = ? AND count_date = ?
		 GROUP BY hour ORDER BY hour`,
		processID, countDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := make(map[int]int64)
	for rows.Next() {
		var hour int
		var sum int64
		if err := rows.Scan(&hour, &sum); err != nil {
			return nil, err
		}
		totals[hour] = sum
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
