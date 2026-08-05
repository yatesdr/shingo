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

// CatchUpRobotConfidence rolls up every completed day inside the retention
// window that has samples but no aggregates yet. This is the scheduler, not a
// repair tool: Core restarts far too often for an elapsed-time ticker to be
// what decides whether the roll-up runs. See store/robotconfidence/catchup.go.
func (db *DB) CatchUpRobotConfidence(now time.Time, retentionDays int, cfg robotconfidence.RollUpConfig) ([]robotconfidence.RollUpResult, error) {
	return robotconfidence.CatchUp(db.DB, now, retentionDays, cfg)
}
