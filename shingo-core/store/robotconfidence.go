package store

import (
	"time"

	"database/sql"

	"shingocore/scenemap"
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

// LaneWindows sums lane_confidence_daily over [from, to) into one distribution
// per lane, so a board window is a read of the permanent record rather than a
// re-run of the snap over raw. See robotconfidence.LaneWindows.
func (db *DB) LaneWindows(from, to time.Time) (map[string]*robotconfidence.LaneWindow, error) {
	return robotconfidence.LaneWindows(db.DB, from, to)
}

// LaneRobotWindows is LaneWindows scoped to one vehicle. The map filter on the
// board calls this; an empty vehicleID never reaches here (the caller uses
// LaneWindows for fleet). See robotconfidence.LaneRobotWindows.
func (db *DB) LaneRobotWindows(from, to time.Time, vehicleID string) (map[string]*robotconfidence.LaneWindow, error) {
	return robotconfidence.LaneRobotWindows(db.DB, from, to, vehicleID)
}

// AreaWindows sums area_confidence_daily over [from, to) into one distribution
// per zone. See robotconfidence.AreaWindows -- zone statistics and zone
// geometry arrive on different transports, so this is keyed on the zone id
// alone and needs no polygon.
func (db *DB) AreaWindows(from, to time.Time) (map[string]*robotconfidence.AreaWindow, error) {
	return robotconfidence.AreaWindows(db.DB, from, to)
}

// LaneWindowBetween and PlantWindowBetween back the change annotation: the
// days either side of an edit, and what the whole plant did over the same days.
func (db *DB) LaneWindowBetween(area, lane string, from, to time.Time) (*robotconfidence.LaneWindow, error) {
	return robotconfidence.LaneWindowBetween(db.DB, area, lane, from, to)
}

func (db *DB) PlantWindowBetween(from, to time.Time) (*robotconfidence.LaneWindow, error) {
	return robotconfidence.PlantWindowBetween(db.DB, from, to)
}

// AreaClassLookup adapts store/sceneversion to robotconfidence's
// AreaClassResolver, so the zone roll-up can label each row with the class of
// zone it describes without the two packages importing each other.
type AreaClassLookup struct{}

// ClassesAt reads the areas in force at an instant and returns id -> class.
//
// TEMPORAL, not current. A zone re-declared from LocConfigArea to
// ReflectorArea is a different zone for the purpose of reading a historical
// day, and labelling last Tuesday's rows with today's class is the same defect
// the lane versioning exists to prevent, one table over.
func (AreaClassLookup) ClassesAt(db *sql.DB, at time.Time) (map[string]string, error) {
	areas, err := sceneversion.AreasAt(db, at)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(areas))
	for _, a := range areas {
		// The map stores "08" and the robot reports "8". Both sides are
		// normalised so the join is on one spelling.
		out[scenemap.NormalizeAreaID(a.Name)] = a.Class
	}
	return out, nil
}
