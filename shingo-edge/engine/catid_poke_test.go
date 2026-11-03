// catid_poke_test.go — when the monitor decides again: a refused fire on a
// poke and at most once a minute, a predicate-false decision only on a poke;
// the change prompt only while no changeover is open; the A5 notice for a part
// the open changeover's target runs.
package engine

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// TestPoke_RefusedFireRetriesOnPokeAndAtMostOncePerMinute: the door refuses
// (the gate is red). With no poke the monitor asks again a minute later, not
// sooner; a poke — a landing on the process — asks on the next tick.
func TestPoke_RefusedFireRetriesOnPokeAndAtMostOncePerMinute(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, calls := setupAutoCutover(t, "auto", "1001", "1002", true)
	mon.completeCutover = func(pid, _ int64, by string, _ CutoverMode) ([]domain.Blocker, error) {
		*calls = append(*calls, autoCutoverSpyCall{pid, by})
		return nil, errors.New("cannot cutover: task at node N1 in staging_requested")
	}
	t0 := time.Unix(0, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	for _, s := range []int{0, 30, 60} {
		tickAutoCutover(mon, processID, st, "1002", true, at(s))
	}
	if len(*calls) != 1 {
		t.Fatalf("first decision: %d door calls, want 1", len(*calls))
	}
	for _, s := range []int{90, 110} {
		tickAutoCutover(mon, processID, st, "1002", true, at(s))
	}
	if len(*calls) != 1 {
		t.Fatalf("within a minute of the refusal: %d door calls, want 1", len(*calls))
	}
	tickAutoCutover(mon, processID, st, "1002", true, at(120))
	if len(*calls) != 2 {
		t.Fatalf("a minute after the refusal: %d door calls, want 2", len(*calls))
	}
	mon.notePoke(processID)
	tickAutoCutover(mon, processID, st, "1002", true, at(121))
	if len(*calls) != 3 {
		t.Fatalf("on the tick after a poke: %d door calls, want 3", len(*calls))
	}
}

// TestPoke_PredicateFalseHasNoClock: a decision the predicate refused (here,
// no changeover open yet) is not taken again on a clock. The changeover's
// start pokes, and the next tick cuts over: a program that settled before the
// changeover opened is not lost.
func TestPoke_PredicateFalseHasNoClock(t *testing.T) {
	t.Parallel()
	mon, st, processID, targetStyleID, calls := setupAutoCutover(t, "auto", "1001", "1002", false)
	t0 := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		tickAutoCutover(mon, processID, st, "1002", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	_, err := mon.eng.db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state) VALUES (?, ?, 'in_progress')`,
		processID, targetStyleID)
	testutil.MustNoErr(t, err, "open a changeover")
	for i := 3; i < 20; i++ {
		tickAutoCutover(mon, processID, st, "1002", true, t0.Add(time.Duration(i)*30*time.Second))
	}
	if len(*calls) != 0 {
		t.Fatalf("a predicate-false decision was taken again on a clock: %d door calls", len(*calls))
	}
	mon.notePoke(processID)
	tickAutoCutover(mon, processID, st, "1002", true, t0.Add(20*30*time.Second))
	if len(*calls) != 1 {
		t.Fatalf("the start's poke: %d door calls, want 1", len(*calls))
	}
}

// TestPrompt_OnlyWhenNoChangeoverOpen: the change prompt offers to start a
// changeover, so it is not raised while one is open (the start would refuse).
func TestPrompt_OnlyWhenNoChangeoverOpen(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, _ := setupAutoCutover(t, "prompt", "1001", "1002", true)
	var prompts int
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if _, ok := evt.Payload.(CATIDChangePromptEvent); ok {
			prompts++
		}
	}, EventCATIDChangePrompt)
	st.lastConfirmed = "1002"
	mon.onConfirmedCATID(processID, st, true)
	if prompts != 0 {
		t.Fatalf("prompted %d time(s) with a changeover open, want 0", prompts)
	}
}

// TestA5_WaitingNoticeDuringAPlannedChangeover: the program changes ahead of
// the cutover at every planned changeover. A live part the open changeover's
// target runs is reported as the changeover's progress, not a wrong part.
func TestA5_WaitingNoticeDuringAPlannedChangeover(t *testing.T) {
	t.Parallel()
	mon, st, processID, _, _ := setupAutoCutover(t, "auto", "1001", "1002", true)
	var got []CATIDMismatchEvent
	mon.eng.Events.SubscribeTypes(func(evt Event) {
		if e, ok := evt.Payload.(CATIDMismatchEvent); ok {
			got = append(got, e)
		}
	}, EventCATIDMismatch)

	st.lastConfirmed = "1002"
	mon.onConfirmedCATID(processID, st, true)
	if len(got) != 1 || got[0].ChangeoverToStyle != "TARGET-STYLE" {
		t.Fatalf("events = %+v, want one naming the changeover's target TARGET-STYLE", got)
	}

	st.lastConfirmed = "1003" // a part neither style runs: still the red alert
	mon.onConfirmedCATID(processID, st, true)
	if len(got) != 2 || got[1].ChangeoverToStyle != "" {
		t.Fatalf("an unrelated part = %+v, want the wrong-part alert", got[1:])
	}
}
