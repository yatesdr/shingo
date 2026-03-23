package store

import (
	"database/sql"
	"time"
)

// ReportingPoint maps a PLC tag to a style for counter tracking.
type ReportingPoint struct {
	ID             int64      `json:"id"`
	PLCName        string     `json:"plc_name"`
	TagName        string     `json:"tag_name"`
	StyleID        int64      `json:"style_id"`
	LastCount      int64      `json:"last_count"`
	LastPollAt     *time.Time `json:"last_poll_at"`
	Enabled        bool       `json:"enabled"`
	WarlinkManaged bool       `json:"warlink_managed"`
	ProcessID      int64      `json:"process_id"` // joined from styles; avoids per-poll lookup
}

func scanReportingPoint(rp *ReportingPoint, scanner interface{ Scan(...interface{}) error }) error {
	var lastPollAt sql.NullString
	if err := scanner.Scan(&rp.ID, &rp.PLCName, &rp.TagName, &rp.StyleID, &rp.LastCount, &lastPollAt, &rp.Enabled, &rp.WarlinkManaged); err != nil {
		return err
	}
	rp.LastPollAt = scanTimePtr(lastPollAt)
	return nil
}

func scanReportingPoints(rows *sql.Rows) ([]ReportingPoint, error) {
	var rps []ReportingPoint
	for rows.Next() {
		var rp ReportingPoint
		if err := scanReportingPoint(&rp, rows); err != nil {
			return nil, err
		}
		rps = append(rps, rp)
	}
	return rps, rows.Err()
}

func (db *DB) ListReportingPoints() ([]ReportingPoint, error) {
	rows, err := db.Query(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points ORDER BY plc_name, tag_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReportingPoints(rows)
}

func (db *DB) ListEnabledReportingPoints() ([]ReportingPoint, error) {
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
		rp.LastPollAt = scanTimePtr(lastPollAt)
		rps = append(rps, rp)
	}
	return rps, rows.Err()
}

func (db *DB) GetReportingPoint(id int64) (*ReportingPoint, error) {
	rp := &ReportingPoint{}
	if err := scanReportingPoint(rp, db.QueryRow(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points WHERE id = ?`, id)); err != nil {
		return nil, err
	}
	return rp, nil
}

func (db *DB) CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO reporting_points (plc_name, tag_name, style_id) VALUES (?, ?, ?)`, plcName, tagName, styleID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error {
	_, err := db.Exec(`UPDATE reporting_points SET plc_name=?, tag_name=?, style_id=?, enabled=? WHERE id=?`, plcName, tagName, styleID, enabled, id)
	return err
}

func (db *DB) UpdateReportingPointCounter(id int64, count int64) error {
	_, err := db.Exec(`UPDATE reporting_points SET last_count=?, last_poll_at=datetime('now') WHERE id=?`, count, id)
	return err
}

func (db *DB) DeleteReportingPoint(id int64) error {
	_, err := db.Exec(`DELETE FROM reporting_points WHERE id=?`, id)
	return err
}

func (db *DB) SetReportingPointManaged(id int64, managed bool) error {
	_, err := db.Exec(`UPDATE reporting_points SET warlink_managed=? WHERE id=?`, managed, id)
	return err
}

func (db *DB) GetReportingPointByTag(plcName, tagName string) (*ReportingPoint, error) {
	rp := &ReportingPoint{}
	if err := scanReportingPoint(rp, db.QueryRow(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points WHERE plc_name = ? AND tag_name = ? LIMIT 1`, plcName, tagName)); err != nil {
		return nil, err
	}
	return rp, nil
}

func (db *DB) GetReportingPointByStyleID(styleID int64) (*ReportingPoint, error) {
	rp := &ReportingPoint{}
	if err := scanReportingPoint(rp, db.QueryRow(`SELECT id, plc_name, tag_name, style_id, last_count, last_poll_at, enabled, warlink_managed FROM reporting_points WHERE style_id = ? LIMIT 1`, styleID)); err != nil {
		return nil, err
	}
	return rp, nil
}
