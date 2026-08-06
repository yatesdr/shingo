package store

import (
	"time"

	"database/sql"
	"shingocore/store/robotconfidence"
	"shingocore/store/sceneversion"
)

// Delegates to the robotconfidence sub-package, matching the shape of
// store/heartbeat.go and store/downtime.go so callers see one *store.DB API.

func (db *DB) EnsureRobotConfidencePartitions(ref time.Time) error {
	return robotconfidence.EnsurePartitions(db.DB, ref)
}

func (db *DB) EnsureRobotConfidencePartitionsRange(start, end time.Time) error {
	return robotconfidence.EnsurePartitionsRange(db.DB, start, end)
}

// DropOldRobotConfidencePartitions expires the raw sample table. The low-
// confidence trail has its own, much longer retention and is dropped
// separately — see DropOldRobotConfidenceLowPartitions.
func (db *DB) DropOldRobotConfidencePartitions(keepDays int, now time.Time) (int, error) {
	return robotconfidence.DropOldPartitions(db.DB, robotconfidence.TableSamples, keepDays, now)
}

func (db *DB) DropOldRobotConfidenceLowPartitions(keepDays int, now time.Time) (int, error) {
	return robotconfidence.DropOldPartitions(db.DB, robotconfidence.TableLow, keepDays, now)
}

// RollUpRobotConfidence computes and stores one day's aggregates. Must run
// BEFORE the retention drop for that day's raw rows.
func (db *DB) RollUpRobotConfidence(day time.Time, cfg robotconfidence.RollUpConfig) (robotconfidence.RollUpResult, error) {
	return robotconfidence.RollUp(db.DB, day, cfg)
}

// CatchUpRobotConfidence rolls up every completed day inside the retention
// window that has samples but no aggregates yet. This is the scheduler, not a
// repair tool: Core restarts far too often for an elapsed-time ticker to be
// what decides whether the roll-up runs. See store/robotconfidence/catchup.go.
func (db *DB) CatchUpRobotConfidence(now time.Time, retentionDays int, cfg robotconfidence.RollUpConfig) ([]robotconfidence.RollUpResult, error) {
	return robotconfidence.CatchUp(db.DB, now, retentionDays, cfg)
}

// LaneVersionResolver adapts store/sceneversion to robotconfidence's
// VersionResolver so the roll-up can ask "which geometry did this lane have
// when this reading was taken".
//
// It lives HERE rather than in either package because Go matches interfaces
// nominally, not structurally, on a return type: sceneversion cannot name
// robotconfidence.VersionLookup without importing it, and having the two
// import each other for one adapter would be a cycle waiting to happen. This
// file already delegates between them.
type LaneVersionResolver struct{}

// Load reads every lane version overlapping the window, once, rather than
// issuing a query per sample.
func (LaneVersionResolver) Load(db *sql.DB, from, to time.Time) (robotconfidence.VersionLookup, error) {
	return sceneversion.LoadLaneVersionIndex(db, from, to)
}
