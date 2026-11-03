package store

import (
	"testing"

	"shingo/protocol/testutil"
)

// The cell catalog the Edge sends Core lists the points that read a live
// process's counter: a stray point is not a cell, and a counter switched off
// still is one.

// TestCatalog_OmitsAnEnabledStrayPoint: an enabled point no process counts on
// (left by a re-point, or by a process since deleted) is not in the catalog.
func TestCatalog_OmitsAnEnabledStrayPoint(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "CELL", "RUN")
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "TAG_A", true, "", false), "counter")
	_, err := db.CreateReportingPoint("plc", "TAG_A", sid)
	testutil.MustNoErr(t, err, "own point")
	_, err = db.CreateReportingPoint("plc", "TAG_STRAY", sid)
	testutil.MustNoErr(t, err, "stray point")

	pts, err := db.ListCountedReportingPoints()
	testutil.MustNoErr(t, err, "catalog")
	if len(pts) != 1 || pts[0].TagName != "TAG_A" || pts[0].ProcessID != pid {
		t.Errorf("catalog = %+v, want only TAG_A for process %d", pts, pid)
	}
}

// TestCatalog_KeepsADisabledCounter: a process whose counter is switched off is
// still a cell Core shows.
func TestCatalog_KeepsADisabledCounter(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, sid := seedProcessStyle(t, db, "CELL", "RUN")
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "TAG_A", false, "", false), "counter off")
	rp, err := db.CreateReportingPoint("plc", "TAG_A", sid)
	testutil.MustNoErr(t, err, "point")
	testutil.MustNoErr(t, db.UpdateReportingPoint(rp, "plc", "TAG_A", sid, false), "disable point")

	pts, err := db.ListCountedReportingPoints()
	testutil.MustNoErr(t, err, "catalog")
	if len(pts) != 1 || pts[0].ID != rp || pts[0].Enabled {
		t.Errorf("catalog = %+v, want the disabled point %d", pts, rp)
	}
}
