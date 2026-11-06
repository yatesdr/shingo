// catid_attribute_test.go — a counted tick is attributed to the style that
// runs the part the PLC reported, read from the monitor's snapshot.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// attributeRig is healthRig with the monitor published for the poll, its
// snapshot on live with a fresh good read, and observed values.
func attributeRig(t *testing.T, live string, observed ...string) (eng *Engine, pid, a, b int64, cm *catidMonitor) {
	t.Helper()
	eng, _, pid, a, b, cm = healthRig(t, live)
	obs := map[string]struct{}{}
	for _, v := range observed {
		obs[v] = struct{}{}
	}
	cm.snapshots.Store(pid, &catidSnapshot{value: live, lastOK: time.Now(), observed: obs})
	eng.tickMonitor.Store(cm)
	return eng, pid, a, b, cm
}

// onPart puts the monitor on v, confirmed as a change at confirmedAt, and
// publishes it through the monitor's own read, with observed already seen.
func onPart(cm *catidMonitor, pid int64, v string, confirmedAt time.Time, observed ...string) *catidState {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	st := cm.states[pid]
	st.seenValue, st.lastConfirmed, st.confirmedAt, st.confirmedBaseline = true, v, confirmedAt, false
	cm.snapshots.Store(pid, &catidSnapshot{observed: set(observed...)})
	cm.publishRead(pid, st, time.Now(), true)
	return st
}

// TestTick_RunOutAfterAnEarlyCutoverPrepaysTheIncomingCart: A → B is completed
// by "Mark as running" while the PLC still reports A's part. A run-out stroke
// after the landing prepays B's bound cart, as the same stroke did before the
// cutover; nothing is held, and the process stays on B. Before, the stroke
// after the landing was off-plan to A and its draw went to the holds ledger.
func TestTick_RunOutAfterAnEarlyCutoverPrepaysTheIncomingCart(t *testing.T) {
	t.Parallel()
	eng, db, pid, nodeID, from, to := runOutRig(t)
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(from, "1001"), "pin A")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(to, "1002"), "pin B")
	cm := &catidMonitor{eng: eng, states: map[int64]*catidState{
		pid: {processName: "CO-PROC", plcName: "plc", tagName: "MES_Cell.CATID_01"},
	}}
	eng.catidMon = cm
	eng.tickMonitor.Store(cm)
	onPart(cm, pid, "1001", time.Now().Add(-5*time.Minute), "1001", "1002")
	testutil.MustNoErr(t, eng.carrierLeft(nodeID, runOutNode), "A's cart lifted")
	landCart(t, eng, nodeID, 9102, "PART-NEW", 40)

	if id := strokeAt(eng, pid, from, 2); id.Kind != plc.TickToday {
		t.Fatalf("before the cutover: attribution %+v, want today", id)
	}
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 38 {
		t.Fatalf("B's cart before the cutover = %d, want 38", got)
	}
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "mark B as running")
	id := strokeAt(eng, pid, to, 2)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 36 {
		t.Errorf("B's cart after a run-out stroke past the landing = %d (attribution %+v), want 36", got, id)
	}
	if hs := openHolds(t, eng, pid); len(hs) != 0 {
		t.Errorf("holds = %+v, want none: a run-out stroke is today's", hs)
	}
	if p := mustProcess(t, db, pid); p.ActiveStyleID == nil || *p.ActiveStyleID != to {
		t.Errorf("active style = %v, want B (%d)", p.ActiveStyleID, to)
	}
}

// TestTick_NoIdentityAttributesAsToday: with no monitor, or a process it does
// not hold, every tick is its point's style, as before the part was read.
func TestTick_NoIdentityAttributesAsToday(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, cm := attributeRig(t, "1002")
	want := plc.TickIdentity{StyleID: a, Kind: plc.TickToday}
	eng.tickMonitor.Store(nil)
	if got := eng.attributeTick(pid, a); got != want {
		t.Errorf("no monitor: %+v, want %+v", got, want)
	}
	eng.tickMonitor.Store(cm)
	cm.forget(pid)
	if got := eng.attributeTick(pid, a); got != want {
		t.Errorf("a process the monitor does not hold: %+v, want %+v", got, want)
	}
}

// TestTick_IdentityNoneAttributesAsToday: identity "none" primes no state, so
// the tick is the point's style.
func TestTick_IdentityNoneAttributesAsToday(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, cm := attributeRig(t, "1002")
	testutil.MustNoErr(t, eng.db.SetProcessCounter(pid, "plc", "MES_Cell.Prod_Counter_01", true, "", true), "identity none")
	cm.reprime(pid)
	if got := eng.attributeTick(pid, a); got.StyleID != a || got.Kind != plc.TickToday || got.CATID != "" {
		t.Errorf("identity none: %+v, want the point's style, today, no part", got)
	}
}

// TestTick_ActiveStylesCATIDUnchanged: the running style's own part is
// attributed to it, its part on the row.
func TestTick_ActiveStylesCATIDUnchanged(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := attributeRig(t, "1001")
	if got := eng.attributeTick(pid, a); got != (plc.TickIdentity{StyleID: a, CATID: "1001", Kind: plc.TickToday}) {
		t.Errorf("active part: %+v", got)
	}
}

// TestTick_StaleIdentityFallsBackToToday: a last good read older than 5 s says
// nothing about this stroke.
func TestTick_StaleIdentityFallsBackToToday(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, cm := attributeRig(t, "1002")
	cm.snapshots.Store(pid, &catidSnapshot{value: "1002", lastOK: time.Now().Add(-6 * time.Second)})
	if got := eng.attributeTick(pid, a); got != (plc.TickIdentity{StyleID: a, Kind: plc.TickToday}) {
		t.Errorf("stale read: %+v, want today with no part", got)
	}
}

// TestTick_UnprovenActiveSetAttributesAsToday: a part one other style runs,
// with the running style's set never seen on the PLC (a catalog set the plant
// has not confirmed), is attributed as today.
func TestTick_UnprovenActiveSetAttributesAsToday(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, _ := attributeRig(t, "1003")
	c, err := eng.db.CreateStyle("STYLE-C", "", pid)
	testutil.MustNoErr(t, err, "style C")
	testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(c, "1003"), "pin C")
	if got := eng.attributeTick(pid, a); got.StyleID != a || got.Kind != plc.TickToday {
		t.Errorf("unproven: %+v, want today on the point's style", got)
	}
}

// TestAttributeCATID_TheRule walks the rule's rows on the pure function.
func TestAttributeCATID_TheRule(t *testing.T) {
	t.Parallel()
	const point, a, tg, s1, s2 = 9, 1, 2, 3, 4
	sets := []styleCATIDEntry{
		{ID: a, Set: set("1001", "1009"), Active: true},
		{ID: tg, Set: set("1002", "1009"), Target: true},
		{ID: s1, Set: set("1003", "1005")},
		{ID: s2, Set: set("1005")},
	}
	proven := set("1001")
	for _, c := range []struct {
		name       string
		v          string
		observed   map[string]struct{}
		sets       []styleCATIDEntry
		runningOut int64
		want       plc.TickIdentity
	}{
		{"active's part", "1001", proven, sets, 0, plc.TickIdentity{StyleID: a, CATID: "1001", Kind: plc.TickToday}},
		{"shared with the target is the active's", "1009", proven, sets, 0, plc.TickIdentity{StyleID: a, CATID: "1009", Kind: plc.TickToday}},
		{"the target's", "1002", proven, sets, 0, plc.TickIdentity{StyleID: tg, CATID: "1002", Kind: plc.TickTarget}},
		{"one other style's, proven", "1003", proven, sets, 0, plc.TickIdentity{StyleID: s1, CATID: "1003", Kind: plc.TickOffPlan}},
		{"several styles', proven", "1005", proven, sets, 0, plc.TickIdentity{StyleID: point, CATID: "1005", Kind: plc.TickHeld}},
		{"no style's, proven", "1007", proven, sets, 0, plc.TickIdentity{StyleID: point, CATID: "1007", Kind: plc.TickHeld}},
		{"one other style's, unproven", "1003", set("-1"), sets, 0, plc.TickIdentity{StyleID: point, CATID: "1003", Kind: plc.TickToday}},
		{"the active's set empty", "1003", proven, []styleCATIDEntry{{ID: a, Active: true}, sets[2]}, 0,
			plc.TickIdentity{StyleID: point, CATID: "1003", Kind: plc.TickToday}},
		{"the running-out style's part", "1003", proven, sets, s1, plc.TickIdentity{StyleID: point, CATID: "1003", Kind: plc.TickToday}},
		{"shared by the running-out style", "1005", proven, sets, s2, plc.TickIdentity{StyleID: point, CATID: "1005", Kind: plc.TickToday}},
		{"another style's, a style running out", "1003", proven, sets, s2, plc.TickIdentity{StyleID: s1, CATID: "1003", Kind: plc.TickOffPlan}},
	} {
		if got := attributeCATID(c.v, point, c.sets, c.observed, c.runningOut); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestTick_TargetCATIDChargesAnIncomingOnlyNode: mid-changeover, a stroke the
// PLC made on the target's part is the target's: a position only the target
// claims is charged. Before, the poll stamped the point's style and the walk
// did not charge it.
func TestTick_TargetCATIDChargesAnIncomingOnlyNode(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, _ := attributeRig(t, "1002", "1001")
	eng.wireEventHandlers()
	nodeID, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "TO-ONLY", Code: "TO-ONLY", Name: "TO-ONLY", Enabled: true})
	testutil.MustNoErr(t, err, "node")
	claimID, err := upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: b, CoreNodeName: "TO-ONLY", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-NEW", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "claim")
	_, err = eng.db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, eng.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, binPtr(9401), 40), "a cart")
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover to B")

	em := &plcEmitter{bus: eng.Events, eng: eng}
	id := em.AttributeTick(pid, a)
	if id.Kind != plc.TickTarget || id.StyleID != b {
		t.Fatalf("attribution = %+v, want target B", id)
	}
	em.EmitCounterDelta(0, pid, 1, 1, "", id)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 39 {
		t.Errorf("incoming-only cart = %d, want 39", got)
	}
}

// TestWalk_TargetTickBeforeTheLiftChargesTheBoundCart — named behaviour, read
// with TestRegression_TickDuringChangeoverRunout and
// TestRegression_ChangeoverDoesNotCarryUOPAcrossStyles: at a node both styles
// claim, a stroke attributed to the target before the outgoing cart's lift is
// charged to that cart, as R1 charges it for the running style's strokes, and
// stamped with its resident part.
func TestWalk_TargetTickBeforeTheLiftChargesTheBoundCart(t *testing.T) {
	t.Parallel()
	eng, _, pid, nodeID, _, to := runOutRig(t)
	sink := eng.inventoryDelta.(*fakeDeltaSink)
	mark := len(sink.binCalls)
	tickConsume(eng, pid, to, 1)
	if got := runtimeOf(t, eng, nodeID).RemainingUOPCached; got != 16 {
		t.Errorf("outgoing cart after a target stroke = %d, want 16", got)
	}
	if len(sink.binCalls) <= mark || sink.binCalls[mark].BinID != 9101 || sink.binCalls[mark].PayloadCode != "PART-OLD" {
		t.Errorf("delta = %+v, want -1 on the outgoing bin 9101 stamped PART-OLD", sink.binCalls[mark:])
	}
}

// TestHourly_CountsUnderTheAttributedStyle: hourly counts the stroke under the
// style the part attributed it to.
func TestHourly_CountsUnderTheAttributedStyle(t *testing.T) {
	t.Parallel()
	eng, pid, a, b, _ := attributeRig(t, "1002", "1001")
	eng.wireEventHandlers()
	_, err := eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover to B")
	em := &plcEmitter{bus: eng.Events, eng: eng}
	em.EmitCounterDelta(0, pid, 3, 3, "", em.AttributeTick(pid, a))
	bucket := counters.HourBucket(time.Now())
	rows, err := eng.db.ListHourlyCounts(pid, b, bucket-3600, bucket+3600)
	testutil.MustNoErr(t, err, "hourly")
	n := int64(0)
	for _, r := range rows {
		n += r.Delta
	}
	if n != 3 {
		t.Errorf("hourly under B = %d (%+v), want 3", n, rows)
	}
}

// TestTickIdentity_NeverTakesTheMonitorLock: the poll attributes while the
// monitor holds its lock (a slow tick, a stalled DB read): it must not wait.
func TestTickIdentity_NeverTakesTheMonitorLock(t *testing.T) {
	t.Parallel()
	eng, pid, a, _, cm := attributeRig(t, "1001")
	cm.mu.Lock()
	defer cm.mu.Unlock()
	done := make(chan plc.TickIdentity, 1)
	go func() { done <- eng.attributeTick(pid, a) }()
	select {
	case got := <-done:
		if got.StyleID != a {
			t.Errorf("attribution under the held lock = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll's attribution waited on the monitor's lock")
	}
}
