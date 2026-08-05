package store

import (
	"time"

	"shingocore/store/robotconfidence"
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
