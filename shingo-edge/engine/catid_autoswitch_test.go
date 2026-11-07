// catid_autoswitch_test.go — no switch without a changeover: with none open,
// a part only one other style runs, by a person's pin, held a minute and
// counted, moves nothing, and the wrong-part item offers the changeover to
// that style; a part several styles run asks the operator instead.
package engine

import (
	"fmt"
	"strings"
	"sync"
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

func runningStyle(t *testing.T, db *store.DB, pid int64) int64 {
	t.Helper()
	p := mustProcess(t, db, pid)
	if p.ActiveStyleID == nil {
		return 0
	}
	return *p.ActiveStyleID
}

// mirrorNode adds a consume position whose part differs between the styles:
// A claims partA there, B partB, and a cart of partA (known) holds 40.
func mirrorNode(t *testing.T, eng *Engine, pid, a, b int64, name, partA, partB string, bin int64) int64 {
	t.Helper()
	node := cartNode(t, eng, pid, a, name, partA, partA, bin)
	_, err := upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: b, CoreNodeName: name, Role: "consume",
		SwapMode: "simple", PayloadCode: partB, UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "B's claim at "+name)
	return node
}

// TestAutoSwitch_NeverSwitchesWithoutAChangeover: the press runs B's pinned
// part with no changeover open, held past the window and counted, in every
// mode: the process stays on A, no changeover row is written and none opens,
// no order is made, the stroke's share at the position whose part differs is
// held there, the wrong-part item offers the changeover to B, and the monitor
// says so once however often it decides again. Before, auto switched the
// process to B and wrote an auto-catid-flip row.
func TestAutoSwitch_NeverSwitchesWithoutAChangeover(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "prompt", "off"} {
		eng, db, pid, a, b, cm := switchRig(t, mode)
		var mu sync.Mutex
		said := 0
		eng.logFn = func(format string, args ...any) {
			if strings.Contains(fmt.Sprintf(format, args...), "nothing moves") {
				mu.Lock()
				said++
				mu.Unlock()
			}
		}
		node := mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-A1", "PART-B1", 9731)
		st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		if id := countedStroke(eng, pid, a); id.Kind != plc.TickOffPlan || id.StyleID != b {
			t.Fatalf("%s: stroke = %+v, want off-plan on B", mode, id)
		}
		settle(cm, pid, st, "1002", time.Now())
		tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(2*time.Minute))
		if got := runningStyle(t, db, pid); got != a {
			t.Errorf("%s: active style = %d, want A (%d): nothing switches without a changeover", mode, got, a)
		}
		rows, err := db.ListRecentProcessChangeovers(pid, 0)
		testutil.MustNoErr(t, err, "changeover rows")
		if len(rows) != 0 {
			t.Errorf("%s: changeover rows %+v, want none", mode, rows)
		}
		orders, err := db.ListActiveOrders()
		testutil.MustNoErr(t, err, "orders")
		if len(orders) != 0 {
			t.Errorf("%s: %d orders, want none: the part change orders nothing", mode, len(orders))
		}
		if got := runtimeOf(t, eng, node).RemainingUOPCached; got != 40 {
			t.Errorf("%s: the PART-A1 cart = %d, want 40: B's stroke is held there, not drawn", mode, got)
		}
		if hs := openHolds(t, eng, pid); len(hs) != 1 || hs[0].ProcessNodeID != node || hs[0].PayloadCode != "PART-B1" {
			t.Errorf("%s: open holds %+v, want B's share held at N-MIRROR", mode, hs)
		}
		it := healthKinds(t, eng, pid)[catidKindWrongPart]
		if it.Action == nil || it.Action.Kind != catidActionStartChangeover || it.Action.ToStyleID != b {
			t.Errorf("%s: the wrong-part item %+v (action %+v), want the changeover to B", mode, it, it.Action)
		}
		mu.Lock()
		if said != 1 {
			t.Errorf("%s: the monitor said %d times that nothing moves, want once", mode, said)
		}
		mu.Unlock()
	}
}

// TestWrongPartItem_OffersTheChangeover: the wrong-part item offers the
// changeover to the one style whose pin holds the PLC's part, with the
// sentence that says what to do and what waits; every other wrong part keeps
// the item as it was, with no action.
func TestWrongPartItem_OffersTheChangeover(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		v     string
		setup func(t *testing.T, eng *Engine, db *store.DB, pid, a, b int64, st *catidState)
		count bool
		offer bool
	}{
		{name: "one pinned target", v: "1002", count: true, offer: true},
		{name: "two styles run it", v: "1002", count: true, setup: func(t *testing.T, eng *Engine, _ *store.DB, pid, _, _ int64, _ *catidState) {
			pinnedStyle(t, eng, pid, "STYLE-C", "1002")
		}},
		{name: "no style runs it", v: "1007", count: true},
		{name: "a set the catalog derived", v: "1002", count: true, setup: func(t *testing.T, _ *Engine, db *store.DB, _, _, b int64, _ *catidState) {
			testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
			seedProduceClaim(t, db, b, "N-B", "PART-B2")
			putCatalog(t, db, 91, "PART-B2", "1002")
		}},
		{name: "a baseline", v: "1002", count: true, setup: func(_ *testing.T, _ *Engine, _ *store.DB, _, _, _ int64, st *catidState) {
			st.confirmedBaseline = true
		}},
		{name: "nothing counted on it", v: "1002"},
		{name: "confirmed before the latest landing", v: "1002", count: true, setup: func(_ *testing.T, eng *Engine, _ *store.DB, pid, a, _ int64, _ *catidState) {
			eng.noteLanding(pid, &a)
		}},
		{name: "a changeover open", v: "1002", count: true, setup: func(t *testing.T, eng *Engine, _ *store.DB, pid, a, _ int64, _ *catidState) {
			c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
			_, err := eng.changeoverService.Create(pid, &a, c, "test", "", nil, nil, nil, nil)
			testutil.MustNoErr(t, err, "open A -> C")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			eng, db, pid, a, b, cm := switchRig(t, "auto")
			mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-A1", "PART-B1", 9741)
			st := changedTo(cm, pid, row.v, time.Now().Add(-5*time.Minute))
			if row.setup != nil {
				row.setup(t, eng, db, pid, a, b, st)
			}
			if row.count {
				countedStroke(eng, pid, a)
				countedStroke(eng, pid, a)
			}
			it, ok := healthKinds(t, eng, pid)[catidKindWrongPart]
			if !ok {
				t.Fatal("no wrong-part item")
			}
			if !row.offer {
				if it.Action != nil || strings.Contains(it.Headline, "Start a changeover") {
					t.Errorf("the item offers %+v (%q), want no action", it.Action, it.Headline)
				}
				return
			}
			want := "The PLC is on 1002 (STYLE-B); PROD-STYLE is active. Start a changeover to STYLE-B — 2 strokes held at N-MIRROR."
			if it.Headline != want {
				t.Errorf("headline %q, want %q", it.Headline, want)
			}
			if it.Action == nil || *it.Action != (CatidHealthAction{Kind: catidActionStartChangeover, ToStyleID: b, ToStyleName: "STYLE-B"}) {
				t.Errorf("action %+v, want the changeover to STYLE-B", it.Action)
			}
		})
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

// sixStrokes counts six strokes the way the poll and the monitor take them,
// each stroke's pokes handed over and a tick after it, then one more poke (a
// landing or a terminal transition elsewhere on the process) and its tick.
func sixStrokes(eng *Engine, cm *catidMonitor, pid, pointStyle int64, st *catidState, v string, start time.Time) {
	for i := 0; i < 6; i++ {
		countedStroke(eng, pid, pointStyle)
		drainPokes(eng, cm)
		tickAutoCutover(cm, pid, st, v, true, start.Add(time.Duration(61+i)*time.Second))
	}
	cm.notePoke(pid)
	tickAutoCutover(cm, pid, st, v, true, start.Add(70*time.Second))
}

// TestAutoSwitch_SixHeldStrokesRaiseOnePrompt: a part two styles run, counted
// six times, raises the change prompt once, and another poke on the same
// value raises nothing more. Before, every held stroke poked the monitor and
// every decision raised the prompt again.
func TestAutoSwitch_SixHeldStrokesRaiseOnePrompt(t *testing.T) {
	t.Parallel()
	eng, _, pid, a, _, cm := switchRig(t, "auto")
	eng.cutoverPokes = make(chan int64, 64)
	pinnedStyle(t, eng, pid, "STYLE-C", "1002")
	prompts := 0
	eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(CATIDChangePromptEvent); ok {
			prompts++
		}
	}, EventCATIDChangePrompt)
	st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
	start := time.Now()
	settle(cm, pid, st, "1002", start)
	sixStrokes(eng, cm, pid, a, st, "1002", start)
	if prompts != 1 {
		t.Errorf("prompts after six held strokes and another poke = %d, want 1", prompts)
	}
}
