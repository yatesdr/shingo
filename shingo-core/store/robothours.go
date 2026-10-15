package store

// Delegate file: the hourly robot record lives in store/robothours/. The
// engine's tap and lidar loop call that package directly on db.DB; this is the
// read side's door.

import (
	"time"

	"shingocore/store/robothours"
)

// RobotDayRows sums the hourly record into plant-local days in loc, fromDay to
// toDay inclusive (YYYY-MM-DD).
func (db *DB) RobotDayRows(loc *time.Location, fromDay, toDay string) ([]robothours.DayRow, error) {
	return robothours.DayRows(db.DB, loc, fromDay, toDay)
}
