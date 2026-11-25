// catid_engine_clock_test.go — the CATID monitor keeps the engine's time: its
// reads, confirms, windows and tickers run on the process clock, so every
// stamp it writes compares with the rows, the ticks and the changeovers the
// engine stamps, on a clock that runs ahead of the wall as the sim's does.
package engine

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/plc"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// aheadOfTheWall is how far the sim stack's clock had run ahead of the wall
// when the monitor's wall stamps were found compared with it.
const aheadOfTheWall = 15 * time.Minute

// clockRig is a process counting on plc/MES_Cell.Prod_Counter_01 (its
// identity CATID_01 derived) on style A (pinned 1001) beside B (pinned 1002),
// both seen, the auto-arm off, the PLC on 1001, and the monitor's run loop
// going with the process clock a manual clock 15 minutes ahead of the wall.
type clockRig struct {
	t         *testing.T
	clk       *clock.Manual
	eng       *Engine
	cm        *catidMonitor
	pid, a, b int64
	mu        sync.Mutex
	logs      []string
}

func newClockRig(t *testing.T) *clockRig {
	t.Helper()
	r := &clockRig{t: t, clk: clock.NewManual(time.Now().Add(aheadOfTheWall))}
	prev := clock.Default()
	clock.SetDefault(r.clk)
	t.Cleanup(func() { clock.SetDefault(prev) })
	db := testEngineDB(t)
	r.eng = testEngine(t, db)
	r.eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, &recordingWarlink{})
	r.eng.logFn = func(format string, args ...any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.logs = append(r.logs, fmt.Sprintf(format, args...))
	}
	var err error
	r.pid, err = db.CreateProcess("CLOCK-PROC", "", "plc", "MES_Cell.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "process")
	r.a, err = db.CreateStyle("STYLE-A", "", r.pid)
	testutil.MustNoErr(t, err, "style A")
	r.b, err = db.CreateStyle("STYLE-B", "", r.pid)
	testutil.MustNoErr(t, err, "style B")
	testutil.MustNoErr(t, db.SetActiveStyle(r.pid, &r.a), "A running")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(r.a, "1001"), "pin A")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(r.b, "1002"), "pin B")
	testutil.MustNoErr(t, db.SetChangeoverAutoArm(r.pid, "off"), "auto-arm off")
	for _, v := range []string{"1001", "1002"} {
		testutil.MustNoErr(t, db.RecordPLCCATIDObservation(r.pid, "plc", v), "observed "+v)
	}
	r.set(1001)
	r.cm = &catidMonitor{eng: r.eng, states: map[int64]*catidState{}}
	r.cm.prime()
	r.eng.catidMon = r.cm
	r.eng.tickMonitor.Store(r.cm)
	r.loop(r.cm.run)
	r.firstRead()
	return r
}

// loop runs fn (a loop that returns when the engine stops) until the test
// ends.
func (r *clockRig) loop(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	r.t.Cleanup(func() {
		select {
		case <-r.eng.stopChan:
		default:
			close(r.eng.stopChan)
		}
		<-done
	})
}

// firstRead moves the process clock a poll interval at a time until the
// monitor has read the PLC once (its ticker is made as its loop starts).
func (r *clockRig) firstRead() {
	for i := 0; i < 100; i++ {
		before := r.snap()
		r.clk.Advance(plcPollInterval)
		if r.waitRead(before, 20*time.Millisecond) && !r.snap().lastOK.IsZero() {
			return
		}
	}
	r.t.Fatal("the monitor never read the PLC")
}

// set puts v on the PLC's CATID_01, as WarLink publishes it.
func (r *clockRig) set(v int64) {
	r.eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.CATID_01", float64(v))
}

func (r *clockRig) snap() *catidSnapshot {
	s, ok := r.cm.snapshot(r.pid)
	if !ok {
		r.t.Fatal("the monitor holds no snapshot for the process")
	}
	return s
}

// waitRead waits up to budget of wall time for the monitor's next read: its
// snapshot republished.
func (r *clockRig) waitRead(before *catidSnapshot, budget time.Duration) bool {
	for end := time.Now().Add(budget); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if s, _ := r.cm.snapshots.Load(r.pid); s != before {
			return true
		}
	}
	return false
}

// step moves the process clock by d and waits for the monitor's read and the
// rest of its tick under the lock (a decision's fire comes after: logged).
func (r *clockRig) step(d time.Duration) {
	before := r.snap()
	r.clk.Advance(d)
	r.waitRead(before, time.Second)
	r.facts() // takes the monitor's lock, so the tick's locked section is over
}

// confirm puts v on the PLC and steps the monitor at its poll interval until
// it has confirmed v, at most 20 reads.
func (r *clockRig) confirm(v int64) {
	r.t.Helper()
	r.set(v)
	want := strconv.FormatInt(v, 10)
	for i := 0; i < 20; i++ {
		r.step(plcPollInterval)
		if r.snap().value == want {
			return
		}
	}
	r.t.Fatalf("the monitor never confirmed %s in 20 reads", want)
}

func (r *clockRig) facts() catidFacts { return r.cm.factsOf(r.pid, clock.Now()) }

// open starts a changeover A → B, at the engine's time.
func (r *clockRig) open() int64 {
	id, err := r.eng.changeoverService.Create(r.pid, &r.a, r.b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(r.t, err, "start the changeover")
	return id
}

// logged waits up to budget of wall time for a log line containing sub.
func (r *clockRig) logged(sub string, budget time.Duration) bool {
	for end := time.Now().Add(budget); ; time.Sleep(time.Millisecond) {
		r.mu.Lock()
		for _, l := range r.logs {
			if strings.Contains(l, sub) {
				r.mu.Unlock()
				return true
			}
		}
		r.mu.Unlock()
		if !time.Now().Before(end) {
			return false
		}
	}
}

// TestCatidMonitor_RunsOnTheEnginesClock: with the process clock 15 minutes
// ahead of the wall (the sim stack's, at 10x), every place the monitor's
// times meet the engine's compares like with like — each row one such place —
// and the monitor's files read no wall clock. At a plant the two clocks are
// one; this is the sim's. Not parallel: it installs the process clock.
func TestCatidMonitor_RunsOnTheEnginesClock(t *testing.T) {
	for _, row := range []struct {
		site string
		run  func(t *testing.T, r *clockRig)
	}{
		{"attributeTick: a confirmed read is fresh", freshAtTheTick},
		{"the strip: a good read is not unreadable", readableOnTheStrip},
		{"evaluateAutoArm: the 60 s window elapses in engine time", windowInEngineTime},
		{"forceHolds: the change is after started_at", changedAfterTheStart},
		{"forceHolds: a change confirmed before the start never forces", changedBeforeTheStart},
		{"noteCountedTick and countedSince: a tick before the confirm is not since it", countedSinceTheConfirm},
		{"partItems: the strip sums the parts since the confirm", partsSinceTheConfirm},
		{"openPostCutoverVerify: the window closes in engine time", verifyWindowInEngineTime},
		{"runSlotWatch: the watch's minute is the engine's", slotWatchMinute},
	} {
		t.Run(row.site, func(t *testing.T) { row.run(t, newClockRig(t)) })
	}
	t.Run("no wall clock in the monitor's files", noWallClock)
}

// freshAtTheTick: a stroke right after a good read is attributed by it.
func freshAtTheTick(t *testing.T, r *clockRig) {
	if got := r.eng.attributeTick(r.pid, r.a); got != (plc.TickIdentity{StyleID: r.a, CATID: "1001", Kind: plc.TickToday}) {
		t.Errorf("attributeTick = %+v, want today on A with the PLC's 1001: the last good read is %v old on the engine's clock",
			got, clock.Now().Sub(r.snap().lastOK))
	}
}

// readableOnTheStrip: a good read the monitor just made is not the strip's
// "Part identity unreadable".
func readableOnTheStrip(t *testing.T, r *clockRig) {
	items, err := r.eng.CatidHealth(r.pid)
	testutil.MustNoErr(t, err, "the strip")
	for _, it := range items {
		if it.Kind == catidKindIdentity {
			t.Errorf("the strip says %q (%s) right after a good read", it.Headline, it.Detail)
		}
	}
}

// windowInEngineTime: a changeover is open and the PLC changes to its
// target's part; the monitor decides on it (auto-arm off: it logs what auto
// would have done) once 60 s of engine time have passed, not before.
func windowInEngineTime(t *testing.T, r *clockRig) {
	r.open()
	r.confirm(1002)
	r.step(15 * time.Second)
	r.step(15 * time.Second)
	if r.facts().stable || r.logged("would have cut over", 50*time.Millisecond) {
		t.Errorf("decided 30 s after the change (3 reads): the window measured against another clock")
	}
	r.step(30 * time.Second)
	if !r.facts().stable || !r.logged("would have cut over", time.Second) {
		t.Errorf("no decision 60 s of engine time after the change over 4 reads (stable %v)", r.facts().stable)
	}
}

// changedAfterTheStart: a change the monitor confirms after the changeover
// opened is after its started_at, and with the window and a counted stroke
// the force holds.
func changedAfterTheStart(t *testing.T, r *clockRig) {
	coID := r.open()
	r.confirm(1002)
	co := changeoverRow(t, r.eng.db, coID)
	f := r.facts()
	if f.confirmedAt.Before(co.StartedAt.Add(time.Second)) {
		t.Errorf("1002 confirmed at %v, before the changeover's start %v: the force never holds",
			f.confirmedAt.UTC().Format(time.TimeOnly), co.StartedAt.UTC().Format(time.TimeOnly))
	}
	r.eng.noteCountedTick(r.pid, plc.TickIdentity{StyleID: r.b, CATID: "1002", Kind: plc.TickTarget})
	r.step(30 * time.Second)
	r.step(30 * time.Second)
	if f = r.facts(); !forceHolds(f, co.StartedAt) {
		t.Errorf("the force does not hold: confirmed %s, started %s, stable %v, counted %s %s at %s",
			f.confirmedAt.UTC().Format(time.TimeOnly), co.StartedAt.UTC().Format(time.TimeOnly), f.stable, f.tick.kind,
			f.tick.catid, f.tick.at.UTC().Format(time.TimeOnly))
	}
}

// changedBeforeTheStart: a program the monitor confirmed before the changeover
// opened never forces it, whatever window passes and whatever is counted after.
// The monitor stamps confirmedAt and the store stamps started_at: were the two
// read from different clocks, the monitor's running ahead, this change would
// read as after the start and the force would hold (forceHolds).
func changedBeforeTheStart(t *testing.T, r *clockRig) {
	r.confirm(1002)
	r.step(5 * time.Second)
	co := changeoverRow(t, r.eng.db, r.open())
	r.eng.noteCountedTick(r.pid, plc.TickIdentity{StyleID: r.b, CATID: "1002", Kind: plc.TickTarget})
	for range 4 {
		r.step(30 * time.Second)
	}
	f := r.facts()
	if !f.stable || f.baseline || !countedSince(f, plc.TickTarget) {
		t.Fatalf("fixture: all but the start must hold (stable %v, baseline %v, counted %s %s at %s)",
			f.stable, f.baseline, f.tick.kind, f.tick.catid, f.tick.at.UTC().Format(time.TimeOnly))
	}
	if forceHolds(f, co.StartedAt) {
		t.Errorf("the force holds on 1002 confirmed %s, before the changeover started %s",
			f.confirmedAt.UTC().Format(time.TimeOnly), co.StartedAt.UTC().Format(time.TimeOnly))
	}
	if !r.logged("would have cut over", time.Second) {
		t.Fatal("fixture: the monitor never decided on the open changeover")
	}
	if r.logged("counted on the target since the change", 50*time.Millisecond) {
		t.Error("the monitor decided a forced cutover on a program confirmed before the start")
	}
}

// countedSinceTheConfirm: a stroke counted on the target after its confirm
// is since it; after the PLC leaves the part and comes back, that stroke is
// before the new confirm.
func countedSinceTheConfirm(t *testing.T, r *clockRig) {
	r.open()
	r.confirm(1002)
	r.eng.noteCountedTick(r.pid, plc.TickIdentity{StyleID: r.b, CATID: "1002", Kind: plc.TickTarget})
	if !countedSince(r.facts(), plc.TickTarget) {
		t.Errorf("a stroke counted after the confirm is not since it: %+v", r.facts())
	}
	r.confirm(1001)
	r.confirm(1002)
	if f := r.facts(); countedSince(f, plc.TickTarget) {
		t.Errorf("a stroke counted at %v reads as since the confirm at %v that came after it",
			f.tick.at.UTC().Format(time.TimeOnly), f.confirmedAt.UTC().Format(time.TimeOnly))
	}
}

// partsSinceTheConfirm: the PLC is on 1003, a part no style runs; of three
// strokes on it, one was counted before the monitor confirmed it. The strip
// counts two.
func partsSinceTheConfirm(t *testing.T, r *clockRig) {
	testutil.MustNoErr(t, r.eng.syncPrimaryPoint(r.pid, "", ""), "the point")
	rp, err := r.eng.db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	testutil.MustNoErr(t, err, "the point")
	stroke := func(n int64) {
		_, err := r.eng.db.InsertCounterSnapshot(rp.ID, n, 1, "", counters.TickStamp{RecordedAt: clock.Now().UTC(),
			ProcessID: r.pid, StyleID: r.a, CATID: "1003"})
		testutil.MustNoErr(t, err, "a stroke")
	}
	r.set(1003)
	r.step(plcPollInterval)
	stroke(1)
	r.confirm(1003)
	r.clk.Advance(time.Second)
	stroke(2)
	stroke(3)
	items, err := r.eng.CatidHealth(r.pid)
	testutil.MustNoErr(t, err, "the strip")
	for _, it := range items {
		if it.Kind == catidKindWrongPart {
			if it.Data["parts"] != int64(2) {
				t.Errorf("the strip counts %v parts on 1003 since its confirm, want 2: %s", it.Data["parts"], it.Detail)
			}
			return
		}
	}
	t.Errorf("no wrong-part item: %+v", items)
}

// verifyWindowInEngineTime: after a cutover onto A the PLC stays on 1002; the
// check flags it when the 2-minute window has passed on the engine's clock.
func verifyWindowInEngineTime(t *testing.T, r *clockRig) {
	r.confirm(1002)
	coID, err := r.eng.db.InsertCompletedChangeover(r.pid, &r.b, r.a, "test", "test", "")
	testutil.MustNoErr(t, err, "the cutover")
	r.eng.openPostCutoverVerify(r.pid, coID)
	r.step(plcPollInterval)
	if _, ok := r.eng.PostCutoverFlag(r.pid); ok {
		t.Fatal("flagged inside the window")
	}
	r.step(postCutoverVerifyWindow)
	if _, ok := r.eng.PostCutoverFlag(r.pid); !ok {
		t.Errorf("not flagged %v of engine time after the cutover", postCutoverVerifyWindow)
	}
}

// slotWatchMinute: the slot watch passes once a minute of engine time.
func slotWatchMinute(t *testing.T, r *clockRig) {
	r.loop(r.cm.runSlotWatch)
	for i := 0; i < 5; i++ {
		r.clk.Advance(slotWatchInterval)
		for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); time.Sleep(time.Millisecond) {
			if r.cm.slotFactsOf(r.pid) != nil {
				return
			}
		}
	}
	t.Error("no pass of the slot watch in 5 minutes of engine time")
}

// wallClock is a read of the wall clock or a ticker on it.
var wallClock = regexp.MustCompile(`\btime\.(Now|NewTicker|Since)\(|datetime\('now'`)

// noWallClock: the monitor's files take their times and tickers from the
// process clock, and the counters' store stamps its rows with it (its
// reporting point's last poll is differenced against a station counter's,
// decision 48): neither Go's wall nor SQLite's datetime('now').
func noWallClock(t *testing.T) {
	for _, f := range []string{"plc_catid_monitor.go", "catid_attribute.go", "catid_holds.go", "admin_style_switch.go",
		"catid_post_cutover.go", "catid_slot_watch.go", "../store/counters/counters.go"} {
		src, err := os.ReadFile(f)
		testutil.MustNoErr(t, err, f)
		for i, line := range strings.Split(string(src), "\n") {
			if wallClock.MatchString(line) {
				t.Errorf("%s:%d: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// ── The production windows, one read at a time ──────────────────────────────
//
// The orders of events the integration module's production-window leaves
// waited the wall clock out for (S2's two gates, S4 untapped, S5), at
// production's values: the declaration's 60 s window (autoArmStableWindow),
// the minute a refused cutover waits (cutoverRetryInterval) and the
// post-cutover verify's two minutes (postCutoverVerifyWindow), on the engine's
// manual clock, the monitor's own loop and the real door. The durations are
// written out, not read from the constants: a change to one of those values
// is a change these tests see. Not parallel: they install the process clock.

// newWindowRig is a clockRig on the phase-3 swap process: P3-NODE's part
// differs between the styles, so a changeover started through the door has a
// leg there and its gate is red until the leg lands. The process counts on
// plc/MES_Cell.Prod_Counter_01, A (the running style) pinned 1001 and B 1002,
// both seen, in auto, with the handlers wired, the pokes carried to the
// monitor's loop and the door the real one; the PLC is on 1001, read once.
func newWindowRig(t *testing.T) *clockRig {
	t.Helper()
	r := &clockRig{t: t, clk: clock.NewManual(time.Now().Add(aheadOfTheWall))}
	prev := clock.Default()
	clock.SetDefault(r.clk)
	t.Cleanup(func() { clock.SetDefault(prev) })
	db := testEngineDB(t)
	r.pid, _, r.a, r.b = seedPhase3SwapScenario(t, db)
	testutil.MustNoErr(t, db.SetProcessCounter(r.pid, "plc", "MES_Cell.Prod_Counter_01", true, "", false), "counter")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(r.a, "1001"), "pin A")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(r.b, "1002"), "pin B")
	testutil.MustNoErr(t, db.SetChangeoverAutoArm(r.pid, "auto"), "auto")
	for _, v := range []string{"1001", "1002"} {
		testutil.MustNoErr(t, db.RecordPLCCATIDObservation(r.pid, "plc", v), "observed "+v)
	}
	r.eng = testEngine(t, db)
	r.eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, &recordingWarlink{})
	r.eng.cutoverPokes = make(chan int64, 64)
	r.eng.logFn = func(format string, args ...any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.logs = append(r.logs, fmt.Sprintf(format, args...))
	}
	r.eng.wireEventHandlers()
	r.set(1001)
	r.cm = &catidMonitor{eng: r.eng, states: map[int64]*catidState{}}
	r.cm.completeCutover = r.eng.completeCutoverLocked
	r.cm.prime()
	r.eng.catidMon = r.cm
	r.eng.tickMonitor.Store(r.cm)
	r.loop(r.cm.run)
	r.firstRead()
	return r
}

// tickAt moves the process clock by d, a whole number of poll intervals so the
// monitor's ticker fires once, and returns when the monitor has read the PLC
// and its loop is idle again: the tick's decision fired, every poke taken.
func (r *clockRig) tickAt(d time.Duration) {
	r.t.Helper()
	before := r.snap()
	r.clk.Advance(d)
	if !r.waitRead(before, 2*time.Second) {
		r.t.Fatalf("the monitor did not read the PLC after %v", d)
	}
	r.idle()
}

// idle returns once the monitor's loop has taken every poke sent to it. A
// poke for no process goes in behind them; the loop takes a poke only between
// ticks, after a tick's fire has returned.
func (r *clockRig) idle() {
	r.t.Helper()
	r.eng.cutoverPokes <- 0
	for end := time.Now().Add(2 * time.Second); len(r.eng.cutoverPokes) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			r.t.Fatal("the monitor's loop did not take its pokes")
		}
	}
}

// to steps the process clock to from+at, at most 10 s a read.
func (r *clockRig) to(from time.Time, at time.Duration) {
	r.t.Helper()
	for left := from.Add(at).Sub(r.clk.Now()); left > 0; left = from.Add(at).Sub(r.clk.Now()) {
		r.tickAt(min(left, 10*time.Second))
	}
	if now := r.clk.Now(); !now.Equal(from.Add(at)) {
		r.t.Fatalf("the clock is %v past the change, want %v", now.Sub(from), at)
	}
}

// said is how many logged lines contain sub.
func (r *clockRig) said(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// lines is every logged line, for a failure message.
func (r *clockRig) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs...)
}

// changedTo puts v on the PLC, steps the monitor until it has confirmed it,
// and returns the confirm's time: the declaration's window opens on it.
func (r *clockRig) changedTo(v int64) time.Time {
	r.t.Helper()
	r.confirm(v)
	r.idle()
	at := r.clk.Now()
	if f := r.facts(); !f.confirmedAt.Equal(at) || f.baseline {
		r.t.Fatalf("fixture: %d confirmed at %v (baseline %v), want a change confirmed now", v, f.confirmedAt, f.baseline)
	}
	return at
}

// stroke is one counted stroke on the point's style A, as the poll makes it,
// whose attribution must be kind; it returns once the monitor took its poke.
func (r *clockRig) stroke(kind plc.TickKind) {
	r.t.Helper()
	if id := countedStroke(r.eng, r.pid, r.a); id.Kind != kind {
		r.t.Fatalf("fixture: the stroke is %+v, want %s", id, kind)
	}
	r.idle()
}

// decisions is how many door decisions the monitor logged, a cut over or a
// refusal: each "changeover N: predicate held on CATID ...".
func (r *clockRig) decisions() int { return r.said(": predicate held on CATID") }

// TestCatidMonitor_GreenGateCompletesAtTheProductionWindow (S2, the gate
// green): a changeover A → B is open with nothing left to land, the PLC
// changes to B's part and a stroke on it is counted before the window closes,
// as the cell's next strokes are. The monitor completes the changeover on the
// read that closes the 60 s window, and not on the read before: auto-catid,
// one decision, the counted part's force meeting a green gate.
func TestCatidMonitor_GreenGateCompletesAtTheProductionWindow(t *testing.T) {
	r := newWindowRig(t)
	coID := r.open()
	if ok, _, err := r.eng.canCompleteChangeover(coID); err != nil || !ok {
		t.Fatalf("fixture: the gate is %v (%v), want green", ok, err)
	}
	changed := r.changedTo(1002)
	r.stroke(plc.TickTarget)
	r.to(changed, 59500*time.Millisecond)
	if co := changeoverRow(t, r.eng.db, coID); co.State.IsTerminal() || r.decisions() != 0 {
		t.Fatalf("59.5 s after the change: changeover %s by %q, %d decisions; want it open and none: the window is 60 s",
			co.State, co.TriggeredBy, r.decisions())
	}
	r.tickAt(500 * time.Millisecond)
	co := changeoverRow(t, r.eng.db, coID)
	if co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredAutoCATID || runningStyle(t, r.eng.db, r.pid) != r.b {
		t.Fatalf("60 s after the change: changeover %s by %q, the process on %d; want completed by auto-catid onto B (%d)",
			co.State, co.TriggeredBy, runningStyle(t, r.eng.db, r.pid), r.b)
	}
	if r.decisions() != 1 || r.said("counted on the target since the change, gate green, mode=auto — cut over") != 1 {
		t.Errorf("decision lines %q, want one: the counted part's force on a green gate", r.lines())
	}
}

// TestCatidMonitor_RedGateForcedAtTheProductionWindow (S2, the gate red): a
// changeover A → B whose leg at P3-NODE has not landed, the PLC changed to
// B's part and nobody presses CUTOVER. Counted before the window closes (the
// order the cell's strokes come in), the part forces the changeover on the
// read that closes the window, past the leg. Counted only after it, the
// window's decision is refused, decided again a minute after the refusal and
// not a read sooner (the window plus the minute: 120 s from the change), and
// the first counted incoming part forces it on the monitor's next read, never
// on the poll's stroke. Either way auto-catid-forced, the leg left running.
func TestCatidMonitor_RedGateForcedAtTheProductionWindow(t *testing.T) {
	start := func(t *testing.T) (*clockRig, int64, *processes.NodeTask) {
		t.Helper()
		r := newWindowRig(t)
		co, task := startChangeover(t, r.eng, r.eng.db, r.pid, r.b)
		if ok, _, err := r.eng.canCompleteChangeover(co.ID); err != nil || ok {
			t.Fatalf("fixture: the gate is %v (%v), want red: the leg at P3-NODE is still moving", ok, err)
		}
		return r, co.ID, task
	}
	open := func(t *testing.T, r *clockRig, coID int64, at string, decided int) {
		t.Helper()
		if co := changeoverRow(t, r.eng.db, coID); co.State.IsTerminal() || r.decisions() != decided {
			t.Fatalf("%s: changeover %s by %q, %d decisions; want it open after %d (lines %q)", at, co.State, co.TriggeredBy,
				r.decisions(), decided, r.lines())
		}
	}
	forced := func(t *testing.T, r *clockRig, coID int64, task *processes.NodeTask, at string) {
		t.Helper()
		co := changeoverRow(t, r.eng.db, coID)
		if co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredAutoCATID+triggeredForced ||
			!strings.Contains(co.Notes, "forced past: ") || runningStyle(t, r.eng.db, r.pid) != r.b {
			t.Fatalf("%s: changeover %s by %q (notes %q); want completed by auto-catid-forced past the leg onto B", at,
				co.State, co.TriggeredBy, co.Notes)
		}
		assertLegsUntouched(t, r.eng.db, task)
		if r.said("counted on the target since the change, forced past task at node Phase3 Swap Node") != 1 {
			t.Errorf("%s: decision lines %q, want one forced cut over", at, r.lines())
		}
	}

	t.Run("counted before the window", func(t *testing.T) {
		r, coID, task := start(t)
		changed := r.changedTo(1002)
		r.stroke(plc.TickTarget)
		r.to(changed, 59500*time.Millisecond)
		open(t, r, coID, "59.5 s after the change", 0)
		r.tickAt(500 * time.Millisecond)
		forced(t, r, coID, task, "60 s after the change")
		if r.decisions() != 1 {
			t.Errorf("decision lines %q, want the one at the window", r.lines())
		}
	})

	t.Run("counted after the refusal", func(t *testing.T) {
		r, coID, task := start(t)
		changed := r.changedTo(1002)
		r.to(changed, 59500*time.Millisecond)
		open(t, r, coID, "59.5 s after the change", 0)
		r.tickAt(500 * time.Millisecond)
		open(t, r, coID, "60 s after the change", 1)
		if r.said("waiting on cannot cutover: task at node Phase3 Swap Node") != 1 || r.said("counted on the target") != 0 {
			t.Fatalf("60 s after the change: decision lines %q, want one refusal on the leg, not forced", r.lines())
		}
		r.to(changed, 119500*time.Millisecond)
		open(t, r, coID, "a minute less a read after the refusal", 1)
		r.tickAt(500 * time.Millisecond)
		open(t, r, coID, "a minute after the refusal", 2)
		if r.said("— not cut over") != 2 {
			t.Fatalf("120 s after the change: decision lines %q, want the refusal decided again", r.lines())
		}
		r.to(changed, 125*time.Second)
		open(t, r, coID, "5 s after the retry", 2)
		r.stroke(plc.TickTarget)
		open(t, r, coID, "the first counted incoming part, before the monitor reads again", 2)
		r.tickAt(500 * time.Millisecond)
		forced(t, r, coID, task, "the read after the counted part")
		if r.decisions() != 3 {
			t.Errorf("decision lines %q, want the window's, the retry's and the force", r.lines())
		}
	})
}

// TestCatidMonitor_NoChangeoverMovesNothingPastTheProductionWindow (S4
// untapped): the PLC jumps to B's part with no changeover open, and a stroke
// on it is counted, off-plan. On the read that closes the 60 s window, and not
// on the read before, the monitor says once that nothing moves; decided again
// on two pokes, past the minute a refusal would wait, the process still runs
// A, no changeover row is written and no order made, the line is not said
// again, and the strip's wrong-part item offers the changeover to B. Its red
// is the parts counted on the part, counter_snapshots rows the poll writes
// and this rig does not; S4's integration halves and
// TestPinStrip_TheLedgersItemsAreInfoTheWrongPartStaysRed hold it.
func TestCatidMonitor_NoChangeoverMovesNothingPastTheProductionWindow(t *testing.T) {
	r := newWindowRig(t)
	const line = "CATID 1002 counted, which only Style-P3-TO runs, with no changeover open — nothing moves"
	nothingMoved := func(at string) {
		t.Helper()
		if got := runningStyle(t, r.eng.db, r.pid); got != r.a {
			t.Fatalf("%s: the process runs %d, want A (%d): nothing switches without a changeover", at, got, r.a)
		}
		rows, err := r.eng.db.ListRecentProcessChangeovers(r.pid, 0)
		testutil.MustNoErr(t, err, "changeover rows")
		orders, err := r.eng.db.ListActiveOrders()
		testutil.MustNoErr(t, err, "orders")
		if len(rows) != 0 || len(orders) != 0 {
			t.Fatalf("%s: changeover rows %+v and %d orders, want none: the part change moves nothing", at, rows, len(orders))
		}
	}
	changed := r.changedTo(1002)
	r.stroke(plc.TickOffPlan)
	r.to(changed, 59500*time.Millisecond)
	if n := r.said(line); n != 0 {
		t.Fatalf("59.5 s after the jump: %q said %d times, want not yet: the window is 60 s", line, n)
	}
	r.tickAt(500 * time.Millisecond)
	if n := r.said(line); n != 1 {
		t.Fatalf("60 s after the jump: %q said %d times, want once (lines %q)", line, n, r.lines())
	}
	nothingMoved("60 s after the jump")
	for _, at := range []time.Duration{90 * time.Second, 150 * time.Second} {
		r.to(changed, at)
		r.eng.pokeCutover(r.pid)
		r.idle()
		r.stroke(plc.TickOffPlan)
	}
	r.to(changed, 180*time.Second)
	nothingMoved("180 s after the jump, decided again on two pokes")
	if n := r.said(line); n != 1 {
		t.Errorf("180 s after the jump: %q said %d times, want once however often it is decided", line, n)
	}
	it := healthKinds(t, r.eng, r.pid)[catidKindWrongPart]
	if it.Action == nil || it.Action.Kind != catidActionStartChangeover || it.Action.ToStyleID != r.b {
		t.Errorf("the wrong-part item %+v (action %+v), want it offering the changeover to B", it, it.Action)
	}
}

// TestCatidMonitor_UnmappedPartAtTheProductionVerifyWindow (S5): during a
// changeover A → B the PLC changes to 1003, which no style pins. Its strokes'
// draw is held whole on the node-0 row and no cart is drawn; a person's
// CUTOVER completes the changeover inside the declaration's window. The
// post-cutover verify closes on the read two minutes after the CUTOVER, not on
// the read before: it records the PLC's 1003 on the row and flags it, and the
// strip offers to pin 1003 to B on that changeover. Pinned, the hold's Charge
// draws the held count from every position's B cart, through the sink, with
// no second production record; a later 1004 is held whole and written off,
// nothing drawn.
func TestCatidMonitor_UnmappedPartAtTheProductionVerifyWindow(t *testing.T) {
	r := newWindowRig(t)
	db := r.eng.db
	p3 := mustNodeIDByName(t, r.eng, "P3-NODE")
	common := cartNode(t, r.eng, r.pid, r.a, "C-COMMON", "COMMON-1", "COMMON-1", 9961)
	commonB, err := upsertClaimRetiredMode(db, processes.NodeClaimInput{StyleID: r.b, CoreNodeName: "C-COMMON", Role: "consume",
		SwapMode: "simple", PayloadCode: "COMMON-1", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "B's claim at C-COMMON")
	coID := r.open()
	testutil.MustNoErr(t, db.SetChangeoverStartCATID(coID, "1001"), "the start's CATID, as the start records it")
	unmapped := func() (CatidHealthItem, bool) {
		it, ok := healthKinds(t, r.eng, r.pid)[catidKindUnmapped]
		return it, ok && it.Data["catid"] == "1003"
	}

	r.changedTo(1003)
	for i := 0; i < 4; i++ {
		r.stroke(plc.TickHeld)
	}
	hs := openHolds(t, r.eng, r.pid)
	if len(hs) != 1 || hs[0].ProcessNodeID != 0 || hs[0].CATID != "1003" || hs[0].Qty != 4 {
		t.Fatalf("1003's strokes: holds %+v, want one open node-0 row of 4", hs)
	}
	if got := runtimeOf(t, r.eng, common).RemainingUOPCached; got != 40 {
		t.Fatalf("1003's strokes drew the C-COMMON cart: %d, want 40", got)
	}

	r.tickAt(10 * time.Second)
	testutil.MustNoErr(t, r.eng.CompleteProcessProductionCutover(r.pid), "CUTOVER")
	r.idle()
	cut := r.clk.Now()
	if co := changeoverRow(t, db, coID); co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredManual {
		t.Fatalf("the CUTOVER: changeover %s by %q, want completed by %s ahead of the monitor", co.State, co.TriggeredBy, triggeredManual)
	}
	// The landings the changeover's legs make: B's carts at both positions.
	toP3, err := db.GetStyleNodeClaimByNode(r.b, "P3-NODE")
	testutil.MustNoErr(t, err, "B's claim at P3-NODE")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(p3, &toP3.ID, binPtr(9962), 40), "the PART-NEW cart")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(p3, "PART-NEW", true, "landing"), "landed PART-NEW")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(common, &commonB, binPtr(9961), 40), "the common cart under B")

	r.to(cut, 119500*time.Millisecond)
	if co := changeoverRow(t, db, coID); co.EndCATID != "" || co.VerifyLiveCATID != "" {
		t.Fatalf("119.5 s after the CUTOVER: end %q, flag %q; want the watch still open: it is two minutes", co.EndCATID, co.VerifyLiveCATID)
	}
	if it, ok := unmapped(); !ok || it.Data["suggestion"] != nil {
		t.Fatalf("119.5 s after the CUTOVER: the 1003 item %+v (%v), want it with no suggestion yet", it, ok)
	}
	r.tickAt(500 * time.Millisecond)
	if co := changeoverRow(t, db, coID); co.EndCATID != "1003" || co.VerifyLiveCATID != "1003" {
		t.Fatalf("120 s after the CUTOVER: end %q, flag %q; want 1003 recorded and flagged", co.EndCATID, co.VerifyLiveCATID)
	}
	it, ok := unmapped()
	sug, _ := it.Data["suggestion"].(map[string]any)
	if !ok || sug == nil || sug["style_id"] != r.b || sug["changeover_id"] != coID {
		t.Fatalf("120 s after the CUTOVER: the 1003 item %+v, want it suggesting B on changeover %d", it, coID)
	}

	testutil.MustNoErr(t, r.eng.AddStyleCATID(r.pid, r.b, "1003", "engineer", fmt.Sprintf("changeover #%d ended on 1003", coID)), "pin 1003 to B")
	if it, ok := unmapped(); ok {
		t.Fatalf("pinned, 1003 still maps to no style: %+v", it)
	}
	pv, err := r.eng.PreviewCatidHold(hs[0].ID)
	testutil.MustNoErr(t, err, "the Charge's preview")
	bins := map[int64]int64{}
	for _, row := range pv.Rows {
		if !row.WrongPart {
			bins[row.BinID] = row.Qty
		}
	}
	if pv.Refusal != "" || pv.StyleID != r.b || len(pv.Rows) != 2 || bins[9961] != 4 || bins[9962] != 4 {
		t.Fatalf("the preview %+v (rows %+v), want B drawing 4 from each position's cart", pv, pv.Rows)
	}
	sink := r.eng.deltaSink().(*fakeDeltaSink)
	sink.mu.Lock()
	mark := len(sink.binCalls)
	sink.mu.Unlock()
	deltas := 0
	r.eng.Events.SubscribeTypes(func(Event) { deltas++ }, EventCounterDelta)
	testutil.MustNoErr(t, r.eng.ChargeCatidHold(hs[0].ID, "engineer"), "Charge")
	for node, name := range map[int64]string{p3: "P3-NODE", common: "C-COMMON"} {
		if got := runtimeOf(t, r.eng, node).RemainingUOPCached; got != 36 {
			t.Errorf("after the Charge %s's B cart reads %d, want 36", name, got)
		}
	}
	sink.mu.Lock()
	drawn := map[int64]fakeBinCall{}
	for _, c := range sink.binCalls[mark:] {
		drawn[c.BinID] = c
	}
	sink.mu.Unlock()
	if len(drawn) != 2 || drawn[9961].Delta != -4 || drawn[9961].PayloadCode != "COMMON-1" ||
		drawn[9962].Delta != -4 || drawn[9962].PayloadCode != "PART-NEW" || deltas != 0 {
		t.Errorf("the Charge sent %+v and %d counter deltas; want -4 on each B cart, stamped its part, and no second record",
			drawn, deltas)
	}

	r.changedTo(1004)
	for i := 0; i < 3; i++ {
		r.stroke(plc.TickHeld)
	}
	later := openHolds(t, r.eng, r.pid)
	if len(later) != 1 || later[0].ProcessNodeID != 0 || later[0].CATID != "1004" || later[0].Qty != 3 {
		t.Fatalf("1004's strokes: holds %+v, want one open node-0 row of 3", later)
	}
	testutil.MustNoErr(t, r.eng.WriteOffCatidHold(later[0].ID, "engineer"), "Write off")
	h, err := db.GetCatidHold(later[0].ID)
	testutil.MustNoErr(t, err, "the written-off hold")
	if left := openHolds(t, r.eng, r.pid); len(left) != 0 || h.Resolution != holdWrittenOff || h.Qty != 3 {
		t.Errorf("after the Write off: open %+v, the hold %+v; want none open and 3 written off", left, h)
	}
	for node, name := range map[int64]string{p3: "P3-NODE", common: "C-COMMON"} {
		if got := runtimeOf(t, r.eng, node).RemainingUOPCached; got != 36 {
			t.Errorf("after the Write off %s's B cart reads %d, want 36: nothing drawn", name, got)
		}
	}
}
