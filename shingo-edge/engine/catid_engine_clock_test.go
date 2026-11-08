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
	"shingoedge/plc"
	"shingoedge/store/counters"
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
		{"noteCountedTick and countedSince: a tick before the confirm is not since it", countedSinceTheConfirm},
		{"partItems: the strip sums the parts since the confirm", partsSinceTheConfirm},
		{"openPostCutoverVerify: the window closes in engine time", verifyWindowInEngineTime},
		{"runSlotWatch: the watch's minute is the engine's", slotWatchMinute},
		{"identityChangedAt: the anchor's confirm is on the poll's clock", anchorOnThePollsClock},
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

// anchorOnThePollsClock: the poll stamps a program change's reset with the
// engine's clock and anchors when the monitor's confirm falls within 10 s of
// it; the confirm is on that clock.
func anchorOnThePollsClock(t *testing.T, r *clockRig) {
	r.confirm(1002)
	if d := clock.Now().Sub(r.eng.identityChangedAt(r.pid)); d < 0 || d > 10*time.Second {
		t.Errorf("the change confirmed %v before the engine's now, want within the anchor's 10 s", d)
	}
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
