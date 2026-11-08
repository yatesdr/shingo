package plc

// line_counters_test.go — the poll's station-counter arm: a first read seeds,
// a counted read emits its strokes, a station on a process whose counter is
// off is not read, and a read after the rollback gap re-seeds.

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol/clock"
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
	// stationDown: the station tag is not in the PLC's values (pass leaves it).
	stationDown bool
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
	lc, err := db.CreateLineCounter(pid, "load station", rigPLC, rigStationTag)
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
	if !r.stationDown {
		mp.Values[rigStationTag] = TagValue{Name: rigStationTag, TypeStr: "DINT", Value: station}
	}
	mp.mu.Unlock()
	r.mgr.pollAllReportingPoints()
}

// age sets every row of table (line_counters or reporting_points) as last
// polled at the SQLite offset ago ("-30 minutes").
func (r *lineRig) age(table, ago string) {
	r.t.Helper()
	_, err := r.db.Exec(`UPDATE `+table+` SET last_poll_at = datetime('now', ?)`, ago)
	testutil.MustNoErr(r.t, err, "age "+table)
}

// reboot is the Edge starting again on the same database: a new manager, the
// PLC as it was.
func (r *lineRig) reboot() {
	plcs := r.mgr.plcs
	r.mgr = newTestManager(r.db, nil, r.em, nil)
	r.mgr.plcs = plcs
}

// stationOut makes the station tag unreadable until stationBack.
func (r *lineRig) stationOut() {
	mp := r.mgr.plcs[rigPLC]
	mp.mu.Lock()
	delete(mp.Values, rigStationTag)
	mp.mu.Unlock()
	r.stationDown = true
}

func (r *lineRig) stationBack() { r.stationDown = false }

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
// process's point was last counted by the run before this boot (an Edge rolled
// back to a build that does not read it, then forward) re-seeds instead of
// charging the whole gap as one stroke.
func TestLinePoll_RollbackGapReseeds(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	r.age(`line_counters`, "-2 hours")
	r.age(`reporting_points`, "-1 minutes") // the rolled-back build counted on it
	r.reboot()
	r.pass(900, 1400)
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

// TestLinePoll_ARollbackOnAClockAheadOfTheWall — decision 48, the clocks rule
// a third time. bootVerdict differences the reporting point's last poll
// against the station counter's, so both are the engine's clock: with the
// process clock 15 minutes ahead of the wall (the sim stack's at 10x), a
// station counter six minutes behind its point re-seeds. Before, the point's
// stamp was SQLite's wall, the difference read negative, and the verdict
// never fired. Not parallel: it installs the process clock.
func TestLinePoll_ARollbackOnAClockAheadOfTheWall(t *testing.T) {
	prev := clock.Default()
	clock.SetDefault(clock.NewManual(time.Now().Add(15 * time.Minute)))
	t.Cleanup(func() { clock.SetDefault(prev) })
	r := newLineRig(t, true)
	r.pass(10, 500)
	_, err := r.db.Exec(`UPDATE line_counters SET last_poll_at = datetime(last_poll_at, '-6 minutes') WHERE id = ?`, r.lc)
	testutil.MustNoErr(t, err, "the station six minutes behind its point")
	r.reboot()
	r.pass(900, 560)
	if got := r.em.emitted(); len(got) != 0 {
		t.Fatalf("emitted %+v after a rollback on a clock ahead of the wall, want a re-seed", got)
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

// TestLinePoll_AGapWithinARunIsCounted: the station tag unreadable for half an
// hour while the line runs is not a rollback: the build that reads stations
// was running, nothing charged the bound positions, and the first good read
// counts the outage's clicks. Before, a station read trailing its point by
// over 15 minutes re-seeded, and the carts read high by the outage.
func TestLinePoll_AGapWithinARunIsCounted(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	r.stationOut()
	r.pass(20, 0)
	r.age(`line_counters`, "-30 minutes")
	r.stationBack()
	r.pass(900, 520)
	if got := r.em.emitted(); len(got) != 1 || got[0].delta != 20 {
		t.Fatalf("emitted %+v, want the outage's 20 clicks", got)
	}
}

// TestLinePoll_AShortRollbackReseeds: a rollback of a few minutes, during which
// the older build counted the point (and charged the bound positions through
// the production walk), re-seeds at the first read of the next boot. Before,
// only a gap over 15 minutes did, so a short rollback charged its clicks twice.
func TestLinePoll_AShortRollbackReseeds(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	r.age(`line_counters`, "-6 minutes")
	r.age(`reporting_points`, "-1 minutes")
	r.reboot()
	r.pass(900, 560)
	if got := r.em.emitted(); len(got) != 0 {
		t.Fatalf("emitted %+v after a short rollback, want a re-seed", got)
	}
	if last, _ := r.counter(); last != 560 {
		t.Errorf("last_count = %d, want 560 (re-seeded)", last)
	}
}

// TestLinePoll_ADowntimeIsCountedNotReseeded: the Edge down for half an hour
// (no build polled anything) counts the station's clicks at its first read,
// as the production point counts its own. Before, the point's first read of
// the boot left the station trailing by the downtime, and it re-seeded.
func TestLinePoll_ADowntimeIsCountedNotReseeded(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	r.age(`line_counters`, "-30 minutes")
	r.age(`reporting_points`, "-30 minutes")
	r.reboot()
	r.pass(900, 560)
	if got := r.em.emitted(); len(got) != 1 || got[0].delta != 60 {
		t.Fatalf("emitted %+v after a downtime, want the 60 clicks counted", got)
	}
}

// TestLinePoll_AnOutageBeforeARestartIsCounted: the station tag unreadable
// while the line runs, then a restart: the failed reads kept the row's poll
// time with the point's, so the next boot's first read counts the outage.
func TestLinePoll_AnOutageBeforeARestartIsCounted(t *testing.T) {
	t.Parallel()
	r := newLineRig(t, true)
	r.pass(10, 500)
	r.age(`line_counters`, "-30 minutes")
	r.stationOut()
	r.pass(20, 0) // the point counts; the station's read fails
	r.reboot()
	r.stationBack()
	r.pass(30, 530)
	if got := r.em.emitted(); len(got) != 1 || got[0].delta != 30 {
		t.Fatalf("emitted %+v, want the outage's 30 clicks", got)
	}
}
