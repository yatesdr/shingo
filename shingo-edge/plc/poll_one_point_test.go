package plc

// poll_one_point_test.go — the poll counts the one point on each process's
// counter tag, and raises a read error for a point whose PLC WarLink lacks.

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
)

// TestPoll_OnePointPerProcess: a second enabled point on a process's style
// (left on another tag) is not read: the process counts on its counter tag's
// point only. Before, the poll read every enabled point, and the walk charged
// each one's strokes to the same carts.
func TestPoll_OnePointPerProcess(t *testing.T) {
	t.Parallel()
	em := &deltaRecorder{}
	r := newUnreadTickRig(t, em, 0)
	const strayTag = "Cell_A_Other_Counter"
	_, err := r.db.CreateReportingPoint(rigPLC, strayTag, r.sty)
	testutil.MustNoErr(t, err, "a second enabled point on the process's style")
	set := func(own, stray int64) {
		mp := r.mgr.plcs[rigPLC]
		mp.mu.Lock()
		mp.Values[rigTag] = TagValue{Name: rigTag, TypeStr: "DINT", Value: own}
		mp.Values[strayTag] = TagValue{Name: strayTag, TypeStr: "DINT", Value: stray}
		mp.mu.Unlock()
		r.mgr.pollAllReportingPoints()
	}
	set(10, 50)
	set(13, 57)
	if got := em.emitted(); len(got) != 1 || got[0].delta != 3 {
		t.Fatalf("emitted %+v, want one delta of 3 from the counter tag's point", got)
	}
}

// TestPoll_UnknownPLCRaisesAReadError: a point on a PLC WarLink does not have
// (or is not connected to) raises a read error naming it, as a station
// counter's does, so the strip names the dead counter. Before, it returned in
// silence and the process counted nothing with no sign.
func TestPoll_UnknownPLCRaisesAReadError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	em := &readErrRecorder{}
	mgr := newTestManager(db, nil, em, nil)
	pid, err := db.CreateProcess("PROC-G", "", "GHOST-PLC", "Ghost_Counter", true)
	testutil.MustNoErr(t, err, "process")
	sty, err := db.CreateStyle("STYLE-G", "", pid)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &sty), "active")
	_, err = db.CreateReportingPoint("GHOST-PLC", "Ghost_Counter", sty)
	testutil.MustNoErr(t, err, "point")
	mgr.pollAllReportingPoints()
	errs := em.readErrors()
	if len(errs) != 1 || !strings.Contains(errs[0], "GHOST-PLC") {
		t.Fatalf("read errors = %q, want one naming GHOST-PLC", errs)
	}
}
