package store

// Phase 5b delegate file: reporting_points CRUD now lives in
// store/counters/. This file preserves the *store.DB method surface so
// external callers do not need to change.

import "shingoedge/store/counters"

// ReportingPoint maps a PLC tag to a style for counter tracking.
type ReportingPoint = counters.ReportingPoint

// ListReportingPoints returns every reporting_point row.
func (db *DB) ListReportingPoints() ([]ReportingPoint, error) {
	return counters.ListReportingPoints(db.DB)
}

// ListEnabledReportingPoints returns every enabled reporting_point,
// joined to styles so callers get the process_id without a per-poll
// lookup.
func (db *DB) ListEnabledReportingPoints() ([]ReportingPoint, error) {
	return counters.ListEnabledReportingPoints(db.DB)
}

// GetReportingPoint returns one reporting_point by id.
func (db *DB) GetReportingPoint(id int64) (*ReportingPoint, error) {
	return counters.GetReportingPoint(db.DB, id)
}

// CreateReportingPoint inserts a reporting_point row.
func (db *DB) CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error) {
	return counters.CreateReportingPoint(db.DB, plcName, tagName, styleID)
}

// UpdateReportingPoint modifies a reporting_point row.
func (db *DB) UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error {
	return counters.UpdateReportingPoint(db.DB, id, plcName, tagName, styleID, enabled)
}

// UpdateReportingPointCounter writes the latest counter value and poll
// time for a reporting_point.
func (db *DB) UpdateReportingPointCounter(id int64, count int64) error {
	return counters.UpdateReportingPointCounter(db.DB, id, count)
}

// DeleteReportingPoint removes a reporting_point row.
func (db *DB) DeleteReportingPoint(id int64) error {
	return counters.DeleteReportingPoint(db.DB, id)
}

// SetReportingPointManaged toggles the warlink_managed flag.
func (db *DB) SetReportingPointManaged(id int64, managed bool) error {
	return counters.SetReportingPointManaged(db.DB, id, managed)
}

// GetReportingPointByTag looks up a reporting_point by its (plc_name,
// tag_name) pair.
func (db *DB) GetReportingPointByTag(plcName, tagName string) (*ReportingPoint, error) {
	return counters.GetReportingPointByTag(db.DB, plcName, tagName)
}

// GetReportingPointByStyleID looks up a reporting_point by style_id.
func (db *DB) GetReportingPointByStyleID(styleID int64) (*ReportingPoint, error) {
	return counters.GetReportingPointByStyleID(db.DB, styleID)
}
