// catid_force_test.go — the counted-part force: a changeover whose press has
// changed to the incoming part, settled and counted it completes past its red
// gate (auto); prompt and off never fire it.
package engine

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// forceRig is a changeover with a red gate (a leg still moving) from a style
// running 1001 to one running 1002, opened ten minutes ago, the process in
// mode, and a monitor holding it whose door is the real one.
func forceRig(t *testing.T, mode string) (eng *Engine, db *store.DB, pid, from, coID int64, task *processes.NodeTask, cm *catidMonitor) {
	t.Helper()
	var to int64
	eng, db, pid, from, to, coID, task = openRedChangeover(t)
	eng.cutoverPokes = make(chan int64, 16)
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(from, "1001"), "pin from")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(to, "1002"), "pin to")
	testutil.MustNoErr(t, db.SetChangeoverAutoArm(pid, mode), "mode")
	_, err := db.DB.Exec(`UPDATE process_changeovers SET started_at = datetime('now', '-10 minutes') WHERE id = ?`, coID)
	testutil.MustNoErr(t, err, "date the changeover")
	cm = &catidMonitor{eng: eng, states: map[int64]*catidState{
		pid: {processName: "FORCE-PROC", plcName: "plc", tagName: "MES_Cell.CATID_01"},
	}}
	eng.catidMon = cm
	eng.tickMonitor.Store(cm)
	return eng, db, pid, from, coID, task, cm
}

// changedTo puts the monitor on v, confirmed as a change at confirmedAt, with a
// fresh good read and both styles' parts seen.
func changedTo(cm *catidMonitor, pid int64, v string, confirmedAt time.Time) *catidState {
	cm.mu.Lock()
	st := cm.states[pid]
	st.seenValue, st.lastConfirmed, st.confirmedAt, st.confirmedBaseline = true, v, confirmedAt, false
	cm.mu.Unlock()
	cm.snapshots.Store(pid, &catidSnapshot{value: v, lastOK: time.Now(), observed: set("1001", v)})
	return st
}

// countedStroke is one counted stroke as the poll makes it, attributed first.
func countedStroke(eng *Engine, pid, pointStyle int64) plc.TickIdentity {
	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, pointStyle)
	em.EmitCounterDelta(0, pid, 1, 1, "", id)
	return id
}

// drainPokes hands the pokes the poll sent to the monitor, as its run loop
// does; reports how many.
func drainPokes(eng *Engine, cm *catidMonitor) int {
	n := 0
	for {
		select {
		case pid := <-eng.cutoverPokes:
			cm.notePoke(pid)
			n++
		default:
			return n
		}
	}
}

// settle runs the monitor's decision on v at start, +30 s and +60 s: the
// declaration's window.
func settle(cm *catidMonitor, pid int64, st *catidState, v string, start time.Time) {
	for _, s := range []int{0, 30, 60} {
		tickAutoCutover(cm, pid, st, v, true, start.Add(time.Duration(s)*time.Second))
	}
}

// TestForcedCutover_CountedIncomingPartCompletesPastARedGate: the press changed
// to the target's part after the changeover opened, it held for the window, and
// a stroke on it was counted: the monitor completes the changeover past its red
// gate, writes what it overrode, and leaves the leg running. Before, the gated
// door refused it and the changeover stayed open behind the moving leg.
func TestForcedCutover_CountedIncomingPartCompletesPastARedGate(t *testing.T) {
	t.Parallel()
	eng, db, pid, from, coID, task, cm := forceRig(t, "auto")
	var notes []CATIDAutoCutoverEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if n, ok := evt.Payload.(CATIDAutoCutoverEvent); ok {
			notes = append(notes, n)
		}
	}, EventCATIDAutoCutover)
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	settle(cm, pid, st, "1002", time.Now())
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Fatalf("completed on the declaration alone with a red gate: %s by %q", co.State, co.TriggeredBy)
	}
	if id := countedStroke(eng, pid, from); id.Kind != plc.TickTarget {
		t.Fatalf("stroke = %+v, want the target's", id)
	}
	if drainPokes(eng, cm) == 0 {
		t.Fatal("the counted target stroke did not poke the monitor")
	}
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(61*time.Second))
	co := changeoverRow(t, db, coID)
	if co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredAutoCATID+triggeredForced {
		t.Fatalf("row = %s by %q, want completed by auto-catid-forced", co.State, co.TriggeredBy)
	}
	if !strings.Contains(co.Notes, "forced past: ") {
		t.Errorf("notes = %q, want the overridden blockers", co.Notes)
	}
	assertLegsUntouched(t, db, task)
	if len(notes) != 1 || !notes[0].Forced || len(notes[0].Blockers) == 0 {
		t.Errorf("station notice = %+v, want one, forced, with its blockers", notes)
	}
}

// TestForcedCutover_PLCAlreadyOnIncomingAtStartNeverForces: a program already
// on the incoming part when the changeover opened is not a change while it was
// open; forcing it would turn every leg just dispatched into a straggler. The
// gated declaration still waits on the gate.
func TestForcedCutover_PLCAlreadyOnIncomingAtStartNeverForces(t *testing.T) {
	t.Parallel()
	eng, db, pid, from, coID, _, cm := forceRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-20*time.Minute))
	for i := 0; i < 3; i++ {
		countedStroke(eng, pid, from)
	}
	drainPokes(eng, cm)
	settle(cm, pid, st, "1002", time.Now())
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(5*time.Minute))
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Fatalf("a part standing before the changeover opened forced it: %s by %q", co.State, co.TriggeredBy)
	}
	// The first read after a start (the baseline) is not a change either.
	cm.mu.Lock()
	st.confirmedAt, st.confirmedBaseline = time.Now().Add(-time.Minute), true
	cm.mu.Unlock()
	countedStroke(eng, pid, from)
	drainPokes(eng, cm)
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(6*time.Minute))
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Fatalf("a baseline read forced it: %s by %q", co.State, co.TriggeredBy)
	}
}

// TestForcedCutover_SharedCATIDNeverForces: a part both styles run is the
// running style's; it is never counted on the target and never forces.
func TestForcedCutover_SharedCATIDNeverForces(t *testing.T) {
	t.Parallel()
	eng, db, pid, from, coID, _, cm := forceRig(t, "auto")
	co := changeoverRow(t, db, coID)
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(from, "1001,1002"), "share 1002")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	if id := countedStroke(eng, pid, from); id.Kind != plc.TickToday {
		t.Fatalf("a shared part's stroke = %+v, want today's", id)
	}
	drainPokes(eng, cm)
	settle(cm, pid, st, "1002", time.Now())
	if got := changeoverRow(t, db, co.ID); got.State.IsTerminal() {
		t.Fatalf("a shared part forced it: %s by %q", got.State, got.TriggeredBy)
	}
}

// TestForcedCutover_PromptModeNeverFires: in prompt mode the monitor never
// fires, though the press changed to the incoming part, held it and counted
// it: the changeover stays open and its legs run.
func TestForcedCutover_PromptModeNeverFires(t *testing.T) {
	t.Parallel()
	eng, db, pid, from, coID, task, cm := forceRig(t, "prompt")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	countedStroke(eng, pid, from)
	drainPokes(eng, cm)
	settle(cm, pid, st, "1002", time.Now())
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Fatalf("prompt mode fired: %s by %q", co.State, co.TriggeredBy)
	}
	assertLegsUntouched(t, db, task)
}

// TestForcedCutover_FiredByTheMonitorNotThePoll (run with -race): the poll's
// counted stroke only records and pokes; the door is entered on the
// monitor's goroutine, never the poll's.
func TestForcedCutover_FiredByTheMonitorNotThePoll(t *testing.T) {
	t.Parallel()
	eng, _, pid, from, _, _, cm := forceRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	var inMonitor atomic.Bool
	var doorCalls, fromPoll atomic.Int32
	cm.completeCutover = func(_, _ int64, by string, mode CutoverMode) ([]domain.Blocker, error) {
		doorCalls.Add(1)
		if !inMonitor.Load() {
			fromPoll.Add(1)
		}
		return nil, errors.New("cannot cutover: spy")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the poll
		defer wg.Done()
		for i := 0; i < 20; i++ {
			countedStroke(eng, pid, from)
		}
	}()
	start := time.Now()
	for i := 0; i < 20; i++ { // the monitor
		inMonitor.Store(true)
		drainPokes(eng, cm)
		cm.mu.Lock()
		intent := cm.evaluateAutoArm(pid, st, true, start.Add(time.Duration(i)*30*time.Second))
		cm.mu.Unlock()
		if intent != nil {
			cm.fireAutoCutover(*intent)
		}
		inMonitor.Store(false)
	}
	wg.Wait()
	if fromPoll.Load() != 0 {
		t.Errorf("the door was entered %d time(s) off the monitor's goroutine", fromPoll.Load())
	}
	if doorCalls.Load() == 0 {
		t.Error("the monitor never decided")
	}
}

// TestForceHolds_TheRule walks forceHolds on the pure function.
func TestForceHolds_TheRule(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ok := catidFacts{value: "1002", confirmedAt: start.Add(time.Minute), stable: true,
		tick: countedTick{catid: "1002", kind: plc.TickTarget, at: start.Add(2 * time.Minute)}}
	if !forceHolds(ok, start) {
		t.Fatal("the full condition does not hold")
	}
	for name, mut := range map[string]func(f *catidFacts){
		"not stable":                      func(f *catidFacts) { f.stable = false },
		"a baseline":                      func(f *catidFacts) { f.baseline = true },
		"confirmed before the start":      func(f *catidFacts) { f.confirmedAt = start.Add(-time.Second) },
		"confirmed in the start's second": func(f *catidFacts) { f.confirmedAt = start.Add(500 * time.Millisecond) },
		"no counted stroke":               func(f *catidFacts) { f.tick = countedTick{} },
		"a stroke counted off-plan":       func(f *catidFacts) { f.tick.kind = plc.TickOffPlan },
		"a stroke on another part":        func(f *catidFacts) { f.tick.catid = "1003" },
		"a stroke before the confirm":     func(f *catidFacts) { f.tick.at = start.Add(30 * time.Second) },
		"a value that names no part":      func(f *catidFacts) { f.value, f.tick.catid = "-1", "-1" },
	} {
		f := ok
		mut(&f)
		if forceHolds(f, start) {
			t.Errorf("%s: holds", name)
		}
	}
}
