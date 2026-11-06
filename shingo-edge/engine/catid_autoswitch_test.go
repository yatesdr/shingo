// catid_autoswitch_test.go — the automatic switch: with no changeover open, a
// part only one other style runs, by a person's pin, held a minute and
// counted, switches the process and records it; a part several styles run
// asks the operator instead.
package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// switchRig is healthRig (A runs 1001, B 1002, both pinned) with the monitor
// published for the poll, the process in mode and the handlers wired.
func switchRig(t *testing.T, mode string) (eng *Engine, db *store.DB, pid, a, b int64, cm *catidMonitor) {
	t.Helper()
	eng, db, pid, a, b, cm = healthRig(t, "1001")
	eng.tickMonitor.Store(cm)
	testutil.MustNoErr(t, db.SetChangeoverAutoArm(pid, mode), "mode")
	eng.wireEventHandlers()
	return eng, db, pid, a, b, cm
}

// flipRows returns the process's automatic-switch rows.
func flipRows(t *testing.T, db *store.DB, pid int64) []processes.Changeover {
	t.Helper()
	rows, err := db.ListChangeoversTriggeredSince(pid, []string{triggeredAutoCATIDFlip}, time.Now().Add(-time.Hour))
	testutil.MustNoErr(t, err, "flip rows")
	return rows
}

func runningStyle(t *testing.T, db *store.DB, pid int64) int64 {
	t.Helper()
	p := mustProcess(t, db, pid)
	if p.ActiveStyleID == nil {
		return 0
	}
	return *p.ActiveStyleID
}

// TestAutoSwitch_CountedPartOnAPinnedStyleSwitchesAndRecords: the press runs
// B's pinned part, held it a minute and a stroke on it was counted: the
// process is switched to B and a completed auto-catid-flip row records it
// with what follows. Before, with no changeover open, the monitor did nothing.
func TestAutoSwitch_CountedPartOnAPinnedStyleSwitchesAndRecords(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, cm := switchRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	if id := countedStroke(eng, pid, a); id.Kind != plc.TickOffPlan || id.StyleID != b {
		t.Fatalf("stroke = %+v, want off-plan on B", id)
	}
	settle(cm, pid, st, "1002", time.Now())
	if got := runningStyle(t, db, pid); got != b {
		t.Fatalf("active style = %d, want B (%d)", got, b)
	}
	rows := flipRows(t, db, pid)
	if len(rows) != 1 || rows[0].FromStyleID == nil || *rows[0].FromStyleID != a || rows[0].ToStyleID != b || rows[0].CalledBy != "auto" {
		t.Fatalf("flip rows = %+v, want one completed A → B by auto", rows)
	}
	if !strings.Contains(rows[0].Notes, "switched from") || !strings.Contains(rows[0].Notes, "request guard") {
		t.Errorf("notes = %q, want the consequences named", rows[0].Notes)
	}
	// Decided once: the next tick on the same part does not switch again.
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(2*time.Minute))
	if n := len(flipRows(t, db, pid)); n != 1 {
		t.Errorf("flip rows after another tick = %d, want 1", n)
	}
}

// TestAutoSwitch_DeclarationAloneDoesNothing: a part held a minute with no
// stroke counted on it moves nothing.
func TestAutoSwitch_DeclarationAloneDoesNothing(t *testing.T) {
	t.Parallel()
	_, db, pid, a, _, cm := switchRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	settle(cm, pid, st, "1002", time.Now())
	tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(5*time.Minute))
	if got := runningStyle(t, db, pid); got != a {
		t.Fatalf("active style = %d, want A (%d): nothing was counted", got, a)
	}
	if rows := flipRows(t, db, pid); len(rows) != 0 {
		t.Errorf("flip rows = %+v, want none", rows)
	}
}

// TestAutoSwitch_NeverOnADerivedSet: a style whose set the catalog derived
// may have the stroke attributed to it, but never moves the cell.
func TestAutoSwitch_NeverOnADerivedSet(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, cm := switchRig(t, "auto")
	testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
	seedProduceClaim(t, db, b, "N-B", "PART-B2")
	putCatalog(t, db, 91, "PART-B2", "1002")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	if id := countedStroke(eng, pid, a); id.Kind != plc.TickOffPlan || id.StyleID != b {
		t.Fatalf("stroke = %+v, want off-plan on B by its derived set", id)
	}
	settle(cm, pid, st, "1002", time.Now())
	if got := runningStyle(t, db, pid); got != a {
		t.Fatalf("active style = %d, want A: B's set is the catalog's, not a person's", got)
	}
}

// TestAutoSwitch_NeverPlansAChangeover: the switch opens no changeover, plans
// no node task and creates or ends no order.
func TestAutoSwitch_NeverPlansAChangeover(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, cm := switchRig(t, "auto")
	before, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	countedStroke(eng, pid, a)
	settle(cm, pid, st, "1002", time.Now())
	if got := runningStyle(t, db, pid); got != b {
		t.Fatalf("active style = %d, want B", got)
	}
	if co, err := db.GetActiveProcessChangeover(pid); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("a changeover is open after the switch: %+v (%v)", co, err)
	}
	rows := flipRows(t, db, pid)
	tasks, err := db.ListChangeoverNodeTasks(rows[0].ID)
	testutil.MustNoErr(t, err, "tasks")
	if len(tasks) != 0 {
		t.Errorf("the switch row has %d node tasks, want none", len(tasks))
	}
	after, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	if len(after) != len(before) {
		t.Errorf("live orders %d → %d; the switch creates and ends none", len(before), len(after))
	}
}

// TestAutoSwitch_AmbiguousPrompts: a part two styles run is held, not switched
// to either; in auto the change prompt asks, naming both. Before, auto asked
// nothing.
func TestAutoSwitch_AmbiguousPrompts(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, _, cm := switchRig(t, "auto")
	pinnedStyle(t, eng, pid, "STYLE-C", "1002")
	var prompts []CATIDChangePromptEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if p, ok := evt.Payload.(CATIDChangePromptEvent); ok {
			prompts = append(prompts, p)
		}
	}, EventCATIDChangePrompt)
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	if id := countedStroke(eng, pid, a); id.Kind != plc.TickHeld {
		t.Fatalf("stroke = %+v, want held: two styles run 1002", id)
	}
	settle(cm, pid, st, "1002", time.Now())
	if got := runningStyle(t, db, pid); got != a {
		t.Fatalf("active style = %d, want A: an ambiguous part switches nothing", got)
	}
	if len(prompts) != 1 || len(prompts[0].Candidates) != 2 || prompts[0].HasTarget {
		t.Fatalf("prompts = %+v, want one naming both candidates", prompts)
	}
}

// TestAutoSwitch_PromptAndOffNeverSwitch: only auto switches.
func TestAutoSwitch_PromptAndOffNeverSwitch(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"prompt", "off"} {
		eng, db, pid, a, _, cm := switchRig(t, mode)
		st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		countedStroke(eng, pid, a)
		settle(cm, pid, st, "1002", time.Now())
		if got := runningStyle(t, db, pid); got != a {
			t.Errorf("%s: active style = %d, want A", mode, got)
		}
	}
}

// TestAutoSwitch_ReleasesHoldsOnTheNewStyle: a hold on the part the switch
// lands on is charged under the new style, once, and the strip's automatic
// decisions list the switch and the release.
func TestAutoSwitch_ReleasesHoldsOnTheNewStyle(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, cm := switchRig(t, "auto")
	node := cartNode(t, eng, pid, b, "B-NODE", "PART-B", "PART-B", 9711)
	_, err := db.HoldCatidDraw(pid, "1002", 0, "", holdReasonHeld, 2, time.Now().Add(-time.Minute))
	testutil.MustNoErr(t, err, "an older hold on 1002")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	countedStroke(eng, pid, a) // off-plan on B: B's cart, of B's part, is charged 1
	settle(cm, pid, st, "1002", time.Now())
	if got := runningStyle(t, db, pid); got != b {
		t.Fatalf("active style = %d, want B", got)
	}
	if got := runtimeOf(t, eng, node).RemainingUOPCached; got != 37 {
		t.Errorf("B's cart = %d, want 37: the stroke and the released hold of 2", got)
	}
	if hs := openHolds(t, eng, pid); len(hs) != 0 {
		t.Errorf("open holds = %+v, want none", hs)
	}
	it, ok := healthKinds(t, eng, pid)[catidKindAutoDecisions]
	if !ok {
		t.Fatal("no automatic decisions item")
	}
	if d, _ := it.Data["decisions"].([]map[string]any); len(d) != 1 {
		t.Errorf("decisions = %v, want the switch", it.Data["decisions"])
	}
	if r, _ := it.Data["hold_releases"].([]map[string]any); len(r) != 1 {
		t.Errorf("hold releases = %v, want the one", it.Data["hold_releases"])
	}
}

// TestAutoSwitch_OpensTheVerifyWatch: the switch row gets the post-cutover
// watch on the new style, like any completion.
func TestAutoSwitch_OpensTheVerifyWatch(t *testing.T) {
	t.Parallel()
	eng, db, pid, a, b, cm := switchRig(t, "auto")
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	countedStroke(eng, pid, a)
	settle(cm, pid, st, "1002", time.Now())
	rows := flipRows(t, db, pid)
	if len(rows) != 1 {
		t.Fatalf("flip rows = %+v", rows)
	}
	cm.mu.Lock()
	pv := cm.verify[pid]
	cm.mu.Unlock()
	if pv == nil || pv.changeoverID != rows[0].ID || pv.styleID != b {
		t.Fatalf("verify watch = %+v, want one on changeover %d for B", pv, rows[0].ID)
	}
}
