// catid_health_test.go — the part-identity health strip: every item shows while
// its condition holds and clears itself, read from state on each call.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
)

// healthRig is a counter-bound process (its identity derived) running style A
// (pinned 1001), with a style B (pinned 1002), and a monitor holding the
// process with a fresh good read of live.
func healthRig(t *testing.T, live string) (eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
	t.Helper()
	db = testEngineDB(t)
	pid, _, a, _ = seedProduceNode(t, db, "two_robot")
	eng = testEngine(t, db)
	eng.plcMgr = seedRigManager()
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "MES_Cell.Prod_Counter_01", true, "", false), "counter")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(a, "1001"), "pin A")
	var err error
	b, err = db.CreateStyle("STYLE-B", "", pid)
	testutil.MustNoErr(t, err, "style B")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, "1002"), "pin B")
	cm = &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	eng.catidMon = cm
	setLive(cm, pid, live)
	return eng, db, pid, a, b, cm
}

// setLive puts the monitor on a confirmed value with a good read now.
func setLive(cm *catidMonitor, pid int64, v string) {
	cm.mu.Lock()
	cm.states[pid] = &catidState{plcName: "plc", tagName: "MES_Cell.CATID_01", processName: "PRODUCE-PROC",
		lastConfirmed: v, seenValue: v != "", confirmedAt: time.Now()}
	cm.mu.Unlock()
	cm.snapshots.Store(pid, &catidSnapshot{value: v, lastOK: time.Now()})
}

func healthKinds(t *testing.T, eng *Engine, pid int64) map[string]CatidHealthItem {
	t.Helper()
	items, err := eng.CatidHealth(pid)
	testutil.MustNoErr(t, err, "catid health")
	out := map[string]CatidHealthItem{}
	for _, it := range items {
		out[it.Kind] = it
	}
	return out
}

// TestCatidHealth_EveryItemShowsWhileTrueAndClears walks each kind available
// before the holds: its condition made true shows it, made false clears it.
func TestCatidHealth_EveryItemShowsWhileTrueAndClears(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind  string
		make  func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor)
		clear func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor)
	}{
		{catidKindWrongPart,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				setLive(cm, pid, "1002")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				setLive(cm, pid, "1001")
			}},
		{catidKindChangeoverWaits,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				_, err := eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
				testutil.MustNoErr(t, err, "open a changeover")
				setLive(cm, pid, "1002")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(pid), "cut over")
			}},
		{catidKindUnmapped,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, db.RecordPLCCATIDObservation(pid, "plc", "1003"), "observe")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, "1002,1003"), "map it")
			}},
		{catidKindShared,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, "1001,1002"), "share")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, "1002"), "unshare")
			}},
		{catidKindNoCATIDs,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, db.SetStyleExpectedCATID(a, ""), "unpin A")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, db.SetStyleExpectedCATID(a, "1001"), "pin A")
			}},
		{catidKindIdentity,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				cm.snapshots.Store(pid, &catidSnapshot{value: "1001", lastOK: time.Now().Add(-time.Minute)})
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				setLive(cm, pid, "1001")
			}},
		{catidKindPostCutover,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				co := seedChangeoverTo(t, eng, db, pid, a)
				testutil.MustNoErr(t, db.SetChangeoverVerifyMismatch(co, "1002"), "flag")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, eng.ClearPostCutoverFlag(pid), "confirm")
			}},
		{catidKindAutoDecisions,
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &b), "admin switch")
				testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &a), "and back")
			},
			func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
				_, err := db.Exec(`UPDATE process_changeovers SET completed_at = datetime('now', '-25 hours') WHERE process_id = ?`, pid)
				testutil.MustNoErr(t, err, "age the rows")
			}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.kind, func(t *testing.T) {
			t.Parallel()
			eng, db, pid, a, b, cm := healthRig(t, "1001")
			if _, ok := healthKinds(t, eng, pid)[c.kind]; ok {
				t.Fatalf("%s shows before its condition", c.kind)
			}
			c.make(t, eng, db, pid, a, b, cm)
			if _, ok := healthKinds(t, eng, pid)[c.kind]; !ok {
				t.Fatalf("%s does not show while its condition holds: %v", c.kind, healthKinds(t, eng, pid))
			}
			c.clear(t, eng, db, pid, a, b, cm)
			if it, ok := healthKinds(t, eng, pid)[c.kind]; ok {
				t.Errorf("%s still shows after its condition cleared: %+v", c.kind, it)
			}
		})
	}
}

// TestCatidHealth_ForcedLegsOpenShowsUntilTheLegsEnd: a changeover completed
// past its gate shows its open legs until they end.
func TestCatidHealth_ForcedLegsOpenShowsUntilTheLegsEnd(t *testing.T) {
	t.Parallel()
	eng, db, processID, _, toID, coID, task := openRedChangeover(t)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &toID), "force")
	if _, ok := healthKinds(t, eng, processID)[catidKindForcedLegsOpen]; !ok {
		t.Fatal("a forced changeover with open legs shows nothing")
	}
	for _, id := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if id != nil {
			markOrderTerminal(db, *id)
		}
	}
	tasks, err := db.ListChangeoverNodeTasks(coID)
	testutil.MustNoErr(t, err, "tasks")
	for _, tk := range tasks {
		testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(tk.ID, domain.NodeTaskSwitched), "end the task")
	}
	if it, ok := healthKinds(t, eng, processID)[catidKindForcedLegsOpen]; ok {
		t.Errorf("the forced item still shows after its legs ended: %+v", it)
	}
}

// TestCatidHealth_MismatchAfterAFlipWithUnchangedCATID: an admin switch to a
// style the PLC's part is not in leaves the CATID unchanged, so no confirm edge
// raises the wrong-part alert. The strip shows it anyway: it reads the state.
func TestCatidHealth_MismatchAfterAFlipWithUnchangedCATID(t *testing.T) {
	t.Parallel()
	eng, _, pid, _, b, _ := healthRig(t, "1001")
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &b), "admin switch to B")
	it, ok := healthKinds(t, eng, pid)[catidKindWrongPart]
	if !ok {
		t.Fatal("no wrong-part item after a switch the PLC's part is not in")
	}
	if it.Data["catid"] != "1001" {
		t.Errorf("item = %+v, want the PLC's 1001", it)
	}
}

// TestCatidHealth_IdentityNoneIsNotAnItem: "none" is configuration. A process
// with no identity tag raises no identity item and no part items.
func TestCatidHealth_IdentityNoneIsNotAnItem(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, _, cm := healthRig(t, "1002")
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "MES_Cell.Prod_Counter_01", true, "", true), "identity none")
	cm.forget(pid)
	for kind := range healthKinds(t, eng, pid) {
		switch kind {
		case catidKindIdentity, catidKindWrongPart, catidKindNoCATIDs, catidKindChangeoverWaits:
			t.Errorf("identity none raised %s", kind)
		}
	}
}

// TestCatidHealth_NoCATIDsOnlyForActiveAndTarget: a style with no CATIDs is an
// item only when it is the running style or the open changeover's target.
func TestCatidHealth_NoCATIDsOnlyForActiveAndTarget(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, _ := healthRig(t, "1001")
	_, err := db.CreateStyle("STYLE-C", "", pid)
	testutil.MustNoErr(t, err, "style C")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(a, ""), "unpin A")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover to B")

	items, err := eng.CatidHealth(pid)
	testutil.MustNoErr(t, err, "health")
	var styles []any
	for _, it := range items {
		if it.Kind == catidKindNoCATIDs {
			styles = append(styles, it.Data["style_id"])
		}
	}
	if len(styles) != 2 || styles[0] != a || styles[1] != b {
		t.Errorf("no-CATID items for styles %v, want the running %d and the target %d only", styles, a, b)
	}
}
