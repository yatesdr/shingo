// catid_fire_reread_test.go — the fire acts on what still stands: a decision
// taken in auto is re-read under the door's mutex, the mode and the pin with
// it, so a process set off or a pin moved since the decision moves nothing.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/plc"
)

// decided takes the monitor's decision on v as its tick does, under cm.mu.
func decided(t *testing.T, cm *catidMonitor, pid int64, st *catidState, v string, at time.Time) autoCutoverIntent {
	t.Helper()
	cm.mu.Lock()
	in := cm.decideAutoArm(pid, st, v, at)
	cm.mu.Unlock()
	if in == nil {
		t.Fatalf("fixture: no decision on %s", v)
	}
	return *in
}

// forcedCutoverDecided is forceRig with the target's part declared (the gated
// door refuses the red gate), then counted, and the forced cutover decided in
// auto.
func forcedCutoverDecided(t *testing.T) (eng *Engine, pid, coID int64, cm *catidMonitor, in autoCutoverIntent) {
	t.Helper()
	var from int64
	eng, _, pid, from, coID, _, cm = forceRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	start := time.Now()
	settle(cm, pid, st, "1002", start)
	countedStroke(eng, pid, from)
	in = decided(t, cm, pid, st, "1002", start.Add(61*time.Second))
	if !in.force || in.mode != "auto" {
		t.Fatalf("fixture: decision = %+v, want a forced cutover in auto", in)
	}
	return eng, pid, coID, cm, in
}

func stillOpen(t *testing.T, eng *Engine, pid, coID int64, what string) {
	t.Helper()
	co, err := eng.db.GetActiveProcessChangeover(pid)
	if err != nil || co == nil || co.ID != coID {
		t.Fatalf("open changeover = %+v (%v), want %d still open: %s", co, err, coID, what)
	}
}

// TestFire_AModeTurnedOffSinceTheDecisionNeverCutsOver: decided in auto, the
// process set off before the fire: the changeover stays open.
func TestFire_AModeTurnedOffSinceTheDecisionNeverCutsOver(t *testing.T) {
	t.Parallel()
	eng, pid, coID, cm, in := forcedCutoverDecided(t)
	testutil.MustNoErr(t, eng.db.SetChangeoverAutoArm(pid, "off"), "mode off")
	cm.fireAutoCutover(in)
	stillOpen(t, eng, pid, coID, "the fire cut over after the mode went off")
}

// TestFire_APinMovedSinceTheDecisionNeverCutsOver: decided in auto, the
// outgoing style pinned to the decided part too before the fire: the predicate
// no longer holds, and the changeover stays open.
func TestFire_APinMovedSinceTheDecisionNeverCutsOver(t *testing.T) {
	t.Parallel()
	eng, pid, coID, cm, in := forcedCutoverDecided(t)
	co, err := eng.db.GetActiveProcessChangeover(pid)
	testutil.MustNoErr(t, err, "open changeover")
	testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(*co.FromStyleID, "1001,1002"), "pin the outgoing style to the part")
	cm.fireAutoCutover(in)
	stillOpen(t, eng, pid, coID, "the fire cut over on a part the outgoing style now runs")
}

// switchDecided is switchRig with B's pinned part counted off-plan and the
// switch to B decided in auto.
func switchDecided(t *testing.T) (eng *Engine, pid, a, b int64, cm *catidMonitor, in autoCutoverIntent) {
	t.Helper()
	eng, _, pid, a, b, cm = switchRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	if id := countedStroke(eng, pid, a); id.Kind != plc.TickOffPlan || id.StyleID != b {
		t.Fatalf("fixture: stroke = %+v, want off-plan on B", id)
	}
	in = decided(t, cm, pid, st, "1002", time.Now())
	if in.switchTo != b || in.mode != "auto" {
		t.Fatalf("fixture: decision = %+v, want the switch to B in auto", in)
	}
	return eng, pid, a, b, cm, in
}

// TestFire_AModeTurnedOffSinceTheDecisionNeverSwitches: decided in auto, the
// process set off before the fire: it stays on A.
func TestFire_AModeTurnedOffSinceTheDecisionNeverSwitches(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, cm, in := switchDecided(t)
	testutil.MustNoErr(t, eng.db.SetChangeoverAutoArm(pid, "off"), "mode off")
	cm.fireAutoCutover(in)
	if got := runningStyle(t, eng.db, pid); got != a {
		t.Fatalf("active style = %d, want A (%d): the switch fired after the mode went off", got, a)
	}
}

// TestFire_APinBlankedSinceTheDecisionNeverSwitches: decided in auto on B's
// pin, the pin blanked before the fire: no person's pin names B for the part
// any more, and the process stays on A.
func TestFire_APinBlankedSinceTheDecisionNeverSwitches(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, cm, in := switchDecided(t)
	testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(b, ""), "blank B's pin")
	cm.fireAutoCutover(in)
	if got := runningStyle(t, eng.db, pid); got != a {
		t.Fatalf("active style = %d, want A (%d): the switch fired on a pin blanked since the decision", got, a)
	}
}
