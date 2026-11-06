// station_in_line_test.go — the parts in the line charge nothing, and the
// deepest run-out outlives a restart.
package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
)

// TestInLine_NeverCharges: the anchor moves no cart. Two cells take the same
// clicks and strokes, one anchored at a count that would read 900 in the line,
// one unanchored: every position is charged the same, a position counted at
// the station by its clicks and one counted at the cell by its strokes. A
// guard: no walk path reads the anchor.
func TestInLine_NeverCharges(t *testing.T) {
	t.Parallel()
	run := func(anchored bool) [2]int {
		r := newPairRig(t)
		a := r.consume("NC-1", "", "simple", true)
		b := r.consume("NC-2", "", "simple", true)
		lc := stationCounter(t, r.db, r.pid, a)
		if anchored {
			testutil.MustNoErr(t, r.db.AnchorLineCounter(lc, "plc", "MES_Cell.Station_Counter_01", -900, time.Now()), "anchor")
		}
		r.stationClick(lc, 7)
		r.prodStroke(3)
		r.stationClick(lc, 2)
		r.prodStroke(4)
		return [2]int{r.left(a), r.left(b)}
	}
	with, without := run(true), run(false)
	if with != without || with != [2]int{41, 43} {
		t.Fatalf("left at the station-counted and cell-counted positions: anchored %v, unanchored %v, want both [41 43]",
			with, without)
	}
}

// TestDeepest_SurvivesRestart: the deepest run-out a station counter has shown
// is stored when it grows, so after a restart the card still reads it and a
// stop is still judged against it: 25 strokes with no click inside a run-out
// once seen 30 deep is not "stopped". Before, it lived in memory and a restart
// started it again from 0.
func TestDeepest_SurvivesRestart(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	r.stationClick(lc, 1)
	r.prodStroke(30) // a cart's run-out
	r.stationClick(lc, 1)

	eng := testEngine(t, r.db) // the Edge restarts: nothing in memory
	eng.wireEventHandlers()
	cards, err := eng.ListStationCounters(r.pid)
	testutil.MustNoErr(t, err, "the cards")
	if len(cards) != 1 || cards[0].DeepestRunout == nil {
		t.Fatalf("after the restart the cards are %+v, want one with its deepest run-out", cards)
	}
	if d := *cards[0].DeepestRunout; d != 30 {
		t.Fatalf("after the restart the card's deepest run-out = %d, want 30", d)
	}
	for i := 0; i < 25; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{ProcessID: r.pid, StyleID: r.style, Delta: 1}})
	}
	if it, ok := healthKinds(t, eng, r.pid)[stationKindStopped]; ok {
		t.Fatalf("after the restart, 25 strokes inside a 30-deep run-out read %q", it.Detail)
	}
}

// TestStationCard_SaysStoppedPastTheDeepestRunOut: the card carries the
// production strokes since the station's last click and, past the deepest
// run-out seen (at least the floor), stopped: the strip's own measure, so the
// card and the strip agree. A click clears it.
func TestStationCard_SaysStoppedPastTheDeepestRunOut(t *testing.T) {
	t.Parallel()
	r, _, lc := healthStation(t)
	card := func() StationCounterCard {
		t.Helper()
		cards, err := r.eng.ListStationCounters(r.pid)
		testutil.MustNoErr(t, err, "the cards")
		if len(cards) != 1 {
			t.Fatalf("cards = %+v, want one", cards)
		}
		return cards[0]
	}
	r.stationClick(lc, 1)
	r.prodStroke(stationStopFloor + 5)
	c := card()
	_, strip := healthKinds(t, r.eng, r.pid)[stationKindStopped]
	if c.StrokesSinceClick != stationStopFloor+5 || !c.Stopped || !strip {
		t.Fatalf("after %d strokes with no click: card strokes %d, stopped %v; strip stopped %v; want stopped on both",
			stationStopFloor+5, c.StrokesSinceClick, c.Stopped, strip)
	}
	r.stationClick(lc, 1)
	r.prodStroke(3)
	if c := card(); c.StrokesSinceClick != 3 || c.Stopped {
		t.Errorf("3 strokes after a click: card strokes %d, stopped %v; want 3, not stopped", c.StrokesSinceClick, c.Stopped)
	}
}
