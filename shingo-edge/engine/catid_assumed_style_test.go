// catid_assumed_style_test.go — decision 56: with exactly two live styles on
// a process and exactly one of them pinned, a confirmed CATID outside the
// pinned set is taken as the unpinned style's part, marked assumed: the
// attribution, the force and the one-tap read it so, and the strip asks for
// the pin, yellow, beside today's unmapped item and its pin verbs. With three
// or more live styles, or none pinned, nothing is assumed. Nothing is ever
// pinned by it. RED at the gate fold's commit: every two-style case (the
// unmapped item and its suggestion are today's, held); the others are today's
// behaviour, held.
package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/plc"
)

const assumedB = "The PLC is on 1002, taken as STYLE-B — STYLE-B's CATID is not pinned; pin it."

func TestPinAssumed_TwoStylesOnePinnedTakesTheOthersPart(t *testing.T) {
	t.Parallel()
	t.Run("the attribution: off-plan to the unpinned style", func(t *testing.T) {
		t.Parallel()
		eng, pid, a, b, _ := attributeRig(t, "1002", "1001")
		testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(b, ""), "unpin B")
		if id := eng.attributeTick(pid, a); id != (plc.TickIdentity{StyleID: b, CATID: "1002", Kind: plc.TickOffPlan}) {
			t.Errorf("attribution = %+v, want off-plan to STYLE-B on 1002 (taken as its part)", id)
		}
	})
	t.Run("the strip: the item yellow, the pin asked for, the one-tap offered", func(t *testing.T) {
		t.Parallel()
		eng, db, pid, a, b, cm := switchRig(t, "auto")
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
		mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-A1", "PART-B1", 9741)
		changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		countedStroke(eng, pid, a)
		countedStroke(eng, pid, a)
		it, ok := healthKinds(t, eng, pid)[catidKindWrongPart]
		if !ok {
			t.Fatal("no wrong-part item")
		}
		if it.Severity != CatidHealthAmber || it.Headline != assumedB {
			t.Errorf("item %s %q, want amber %q", it.Severity, it.Headline, assumedB)
		}
		if it.Action == nil || it.Action.Kind != catidActionStartChangeover || it.Action.ToStyleID != b {
			t.Errorf("action %+v, want the one-tap to STYLE-B", it.Action)
		}
	})
	t.Run("the force completes a changeover to it past a red gate", func(t *testing.T) {
		t.Parallel()
		eng, db, pid, from, coID, _, cm := forceRig(t, "auto")
		co := changeoverRow(t, db, coID)
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(co.ToStyleID, ""), "unpin the incoming style")
		st := changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		settle(cm, pid, st, "1002", time.Now())
		if id := countedStroke(eng, pid, from); id.Kind != plc.TickTarget || id.StyleID != co.ToStyleID {
			t.Fatalf("stroke = %+v, want the target's (its part taken as the incoming style's)", id)
		}
		drainPokes(eng, cm)
		tickAutoCutover(cm, pid, st, "1002", true, time.Now().Add(61*time.Second))
		if co := changeoverRow(t, db, coID); co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredAutoCATID+triggeredForced {
			t.Errorf("row = %s by %q, want completed by auto-catid-forced", co.State, co.TriggeredBy)
		}
	})
	t.Run("the style running it: the strip asks for the pin, nothing offered", func(t *testing.T) {
		t.Parallel()
		eng, db, pid, _, b, _ := healthRig(t, "1002")
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
		testutil.MustNoErr(t, db.SetActiveStyle(pid, &b), "B runs")
		kinds := healthKinds(t, eng, pid)
		it, ok := kinds[catidKindAssumed]
		if !ok || it.Severity != CatidHealthAmber || it.Headline != assumedB || it.Action != nil {
			t.Errorf("assumed item %+v (present %t), want amber %q with no action", it, ok, assumedB)
		}
		if _, ok := kinds[catidKindWrongPart]; ok {
			t.Error("a wrong-part item: the PLC's part is the running style's, taken as assumed")
		}
	})
	t.Run("the pin it asks for stays on the strip: unmapped, its verbs, and the suggestion", func(t *testing.T) {
		t.Parallel()
		eng, db, pid, a, b, _ := healthRig(t, "1002")
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
		testutil.MustNoErr(t, db.RecordPLCCATIDObservation(pid, "plc", "1002"), "observe 1002")
		co, err := eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
		testutil.MustNoErr(t, err, "changeover")
		testutil.MustNoErr(t, db.SetChangeoverStartCATID(co, "1001"), "start")
		testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(pid), "cut over")
		testutil.MustNoErr(t, db.SetChangeoverEndCATID(co, "1002"), "end")
		kinds := healthKinds(t, eng, pid)
		if _, ok := kinds[catidKindAssumed]; !ok {
			t.Error("no assumed item for the running style's part")
		}
		it, ok := kinds[catidKindUnmapped]
		if !ok {
			t.Fatal("no unmapped item: the assumption took the pin verbs off the strip")
		}
		sug, _ := it.Data["suggestion"].(map[string]any)
		if sug == nil || sug["style_id"] != b || !strings.Contains(it.Detail, "ended on 1002") {
			t.Errorf("unmapped item %+v, want STYLE-B suggested from the changeover that ended on 1002 (decision 17)", it)
		}
	})
}

func TestPinAssumed_ThreeStylesOrNonePinnedAssumeNothing(t *testing.T) {
	t.Parallel()
	t.Run("three styles, one pinned: held, no target, no assumption", func(t *testing.T) {
		t.Parallel()
		eng, db, pid, a, b, cm := switchRig(t, "auto")
		testutil.MustNoErr(t, db.SetStyleExpectedCATID(b, ""), "unpin B")
		_, err := db.CreateStyle("STYLE-C", "", pid)
		testutil.MustNoErr(t, err, "a third style")
		mirrorNode(t, eng, pid, a, b, "N-MIRROR", "PART-A1", "PART-B1", 9741)
		changedTo(cm, pid, "1002", time.Now().Add(-5*time.Minute))
		if id := countedStroke(eng, pid, a); id.Kind != plc.TickHeld || id.StyleID != a {
			t.Errorf("attribution = %+v, want held under PROD-STYLE (no style runs 1002)", id)
		}
		kinds := healthKinds(t, eng, pid)
		if it, ok := kinds[catidKindWrongPart]; !ok || it.Action != nil {
			t.Errorf("wrong-part item %+v (present %t), want it with no offer", it, ok)
		}
		if _, ok := kinds[catidKindAssumed]; ok {
			t.Error("an assumed item with three styles")
		}
	})
	t.Run("neither pinned: today's, the point's style", func(t *testing.T) {
		t.Parallel()
		eng, pid, a, b, _ := attributeRig(t, "1002", "1001")
		testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(a, ""), "unpin A")
		testutil.MustNoErr(t, eng.db.SetStyleExpectedCATID(b, ""), "unpin B")
		if id := eng.attributeTick(pid, a); id.Kind != plc.TickToday || id.StyleID != a {
			t.Errorf("attribution = %+v, want today under the point's style", id)
		}
	})
}
