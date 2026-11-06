package plc

// line_counters_test.go — the poll's station-counter arm: a first read seeds,
// a counted read emits its strokes, a station on a process whose counter is
// off is not read, and a read after the rollback gap re-seeds.

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/internal/testdb"
	"shingoedge/store"
)

const rigStationTag = "Cell_A_Station_01"

type lineDelta struct {
	counter, process, delta int64
	id                      TickIdentity
}

type lineRecorder struct {
	mockEmitter
	lmu   sync.Mutex
	lines []lineDelta
}

func (r *lineRecorder) EmitLineCounterDelta(counterID, processID, delta int64, _ string, id TickIdentity) {
	r.lmu.Lock()
	r.lines = append(r.lines, lineDelta{counterID, processID, delta, id})
	r.lmu.Unlock()
}

func (r *lineRecorder) emitted() []lineDelta {
	r.lmu.Lock()
	defer r.lmu.Unlock()
	return append([]lineDelta(nil), r.lines...)
}

type lineRig struct {
	t   *testing.T
	db  *store.DB
	mgr *Manager
	em  *lineRecorder
	pid int64
	lc  int64
}

// newLineRig is a process with its production counter wired (enabled or not),
// an active style, its reporting point, and one station counter.
func newLineRig(t *testing.T, counterEnabled bool) *lineRig {
	t.Helper()
	db := testdb.Open(t)
	cfg := config.Defaults()
	cfg.Messaging.StationID = "stn-test"
	em := &lineRecorder{}
	mgr := newTestManager(db, cfg, em, nil)
	pid, err := db.CreateProcess("PROC-L", "", rigPLC, rigTag, counterEnabled)
	testutil.MustNoErr(t, err, "process")
	sty, err := db.CreateStyle("STYLE-L", "", pid)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &sty), "active")
	_, err = db.CreateReportingPoint(rigPLC, rigTag, sty)
	testutil.MustNoErr(t, err, "reporting point")
	lc, err := db.CreateLineCounter(pid, "op 10", rigPLC, rigStationTag)
	testutil.MustNoErr(t, err, "station counter")
	mgr.plcs[rigPLC] = &ManagedPLC{Name: rigPLC, Status: "Connected", Values: map[string]TagValue{}}
	return &lineRig{t: t, db: db, mgr: mgr, em: em, pid: pid, lc: lc}
}

// pass runs one poll pass with the production counter at count and the
// station counter at station.
func (r *lineRig) pass(count, station int64) {
	mp := r.mgr.plcs[rigPLC]
	mp.mu.Lock()
	mp.Values[rigTag] = TagValue{Name: rigTag, TypeStr: "DINT", Value: count}
	mp.Values[rigStationTag] = TagValue{Name: rigStationTag, TypeStr: "DINT", Value: station}
	mp.mu.Unlock()
	r.mgr.pollAllReportingPoints()
}

func (r *lineRig) counter() (last int64, polled *time.Time) {
	r.t.Helper()
	c, err := r.db.GetLineCounter(r.lc)
	testutil.MustNoErr(r.t, err, "station counter")
	return c.LastCount, c.LastPollAt
}

// TestLinePoll_FirstReadSeedsThenCounts: the first read is where counting
// starts; the next emits the difference, attributed against the active style.
func TestLinePoll_FirstReadSeedsThenCounts(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	if got := r.em.emitted(); len(got) != 0 {
		t.Fatalf("first read emitted %+v, want nothing", got)
	}
	if last, polled := r.counter(); last != 500 || polled == nil {
		t.Fatalf("after the first read: last %d polled %v, want 500 and a time", last, polled)
	}
	r.pass(11, 503)
	got := r.em.emitted()
	if len(got) != 1 || got[0].counter != r.lc || got[0].process != r.pid || got[0].delta != 3 {
		t.Fatalf("emitted %+v, want one of 3 for station counter %d", got, r.lc)
	}
}

// TestStation_CounterOffChargesNothing: a station counter is read only while
// its process's production counter is wired and enabled.
func TestStation_CounterOffChargesNothing(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, false)
	r.pass(10, 500)
	r.pass(11, 505)
	if got := r.em.emitted(); len(got) != 0 {
		t.Errorf("emitted %+v with the process's counter off, want nothing", got)
	}
	if _, polled := r.counter(); polled != nil {
		t.Errorf("last_poll_at = %v, want never read", polled)
	}
}

// TestLinePoll_RollbackGapReseeds: a station counter last read long before its
// process's point (an Edge rolled back to a build that does not read it, then
// forward) re-seeds instead of charging the whole gap as one stroke.
func TestLinePoll_RollbackGapReseeds(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	_, err := r.db.Exec(`UPDATE line_counters SET last_poll_at = datetime('now', '-2 hours') WHERE id = ?`, r.lc)
	testutil.MustNoErr(t, err, "the gap")
	r.pass(900, 1400) // the point counts on; it was read all along
	if got := r.em.emitted(); len(got) != 0 {
		t.Fatalf("after the gap emitted %+v, want a re-seed", got)
	}
	if last, _ := r.counter(); last != 1400 {
		t.Errorf("last_count = %d, want 1400 (re-seeded)", last)
	}
	r.pass(901, 1402)
	if got := r.em.emitted(); len(got) != 1 || got[0].delta != 2 {
		t.Errorf("after the re-seed emitted %+v, want one of 2", got)
	}
}

// TestLinePoll_IdleStationIsNotAGap: a station that reads the same count while
// its process's point keeps counting is touched, so it never trails into a
// re-seed that would drop its next strokes.
func TestLinePoll_IdleStationIsNotAGap(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	_, err := r.db.Exec(`UPDATE line_counters SET last_poll_at = datetime('now', '-10 minutes') WHERE id = ?`, r.lc)
	testutil.MustNoErr(t, err, "an idle station")
	r.pass(20, 500)
	_, polled := r.counter()
	if polled == nil || time.Since(*polled) > time.Minute {
		t.Fatalf("an unchanged read did not touch last_poll_at: %v", polled)
	}
	r.pass(30, 504)
	if got := r.em.emitted(); len(got) != 1 || got[0].delta != 4 {
		t.Errorf("emitted %+v, want one of 4", got)
	}
}
