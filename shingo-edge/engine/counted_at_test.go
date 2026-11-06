// counted_at_test.go — binding positions to a station counter from its card:
// the dry run binds nothing, a binding is a person's save, and a binding made
// mid-run nets within the line under the borrowing rule.
package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
)

// TestCountedAt_NothingBindsWithoutTheSave: the card's dry run reads every
// position and binds none; a position made after the counter exists is not
// bound either (there is no walk-time default). Station clicks charge nothing
// until a position is saved to the counter; the production counter keeps
// charging every unbound position. A guard.
func TestCountedAt_NothingBindsWithoutTheSave(t *testing.T) {
	t.Parallel()
	r := newPairRig(t)
	a := r.consume("NB-1", "", "simple", true)
	b := r.consume("NB-2", "", "simple", true)
	lc := stationCounter(t, r.db, r.pid)
	for _, n := range []int64{a, b} {
		testutil.MustNoErr(t, r.eng.StationCountable(n), "the dry run")
	}
	later := r.consume("NB-3", "", "simple", true)
	r.stationClick(lc, 3)
	for _, n := range []int64{a, b, later} {
		node, err := r.db.GetProcessNode(n)
		testutil.MustNoErr(t, err, "node")
		if node.LineCounterID != nil {
			t.Errorf("%s is counted at station counter %d with no save", node.Name, *node.LineCounterID)
		}
		if got := r.left(n); got != 50 {
			t.Errorf("%s after three station clicks = %d, want 50 (nothing is bound)", node.Name, got)
		}
	}
	r.prodStroke(2)
	for _, n := range []int64{a, b, later} {
		if got := r.left(n); got != 48 {
			t.Errorf("node %d after two production strokes = %d, want 48", n, got)
		}
	}
}

// TestCountedAt_BindMidRunNetsWithinTheLine: a position counted at the end of
// a line `depth` parts deep is bound to the load station while its cart runs.
// Before the bind each stroke at the end charged the cart, the first `depth`
// of them its predecessor's parts; at the bind its own last `depth` parts are
// in the line, already clicked at the station, and never charged. The two
// net: each cart, the one running at the bind and the next, is charged exactly
// what was loaded from it, and no stroke is charged twice, the line's drain
// included.
func TestCountedAt_BindMidRunNetsWithinTheLine(t *testing.T) {
	t.Parallel()
	const depth = 4
	r := newPairRig(t)
	n := r.consume("ML-1", "", "simple", true) // cart A, 50 parts
	lc := stationCounter(t, r.db, r.pid)
	line := []string{"Z", "Z", "Z", "Z"} // the predecessor's parts, in the line as A lands
	loads := map[string]int{}
	cycle := func(cart string) {
		r.stationClick(lc, 1) // a part leaves the cart at the load station
		loads[cart]++
		line = append(line[1:], cart)
		r.prodStroke(1) // the part `depth` loads earlier is counted at the end
	}

	for i := 0; i < 20; i++ {
		cycle("A")
	}
	testutil.MustNoErr(t, r.eng.SetCountedAt(n, &lc), "bind mid-run")
	for _, c := range line {
		if c != "A" {
			t.Fatalf("fixture: the line at the bind holds %v, want A's own %d parts", line, depth)
		}
	}
	for i := 0; i < 30; i++ {
		cycle("A")
	}
	if got := 50 - r.left(n); got != loads["A"] {
		t.Errorf("cart A charged %d, loaded %d", got, loads["A"])
	}

	claim := runtimeOf(t, r.eng, n).ActiveClaimID
	testutil.MustNoErr(t, r.db.SetProcessNodeRuntimeWithBin(n, claim, binPtr(9901), 50), "cart B lands")
	for i := 0; i < 10; i++ {
		cycle("B")
	}
	r.prodStroke(depth) // the line drains: B's last parts are counted at the end, nothing loads
	if got := 50 - r.left(n); got != loads["B"] {
		t.Errorf("cart B charged %d, loaded %d", got, loads["B"])
	}
}

// inLineRig is a pair rig whose process counts on a production point at 6 since
// its last reset, with one consume position and a station counter that has
// clicked 30 and was anchored at 16 at that reset: 8 parts in the line.
func inLineRig(t *testing.T) (*pairRig, int64, int64) {
	t.Helper()
	r := newPairRig(t)
	n := r.consume("IL-1", "", "simple", true)
	const plcName, point, station = "plc", "MES_Cell.Prod_Counter_01", "MES_Cell.Station_Counter_01"
	_, err := r.db.Exec(`UPDATE processes SET counter_plc_name = ?, counter_tag_name = ? WHERE id = ?`, plcName, point, r.pid)
	testutil.MustNoErr(t, err, "the process's counter")
	rp, err := r.db.CreateReportingPoint(plcName, point, r.style)
	testutil.MustNoErr(t, err, "the production point")
	_, err = r.db.UpdateReportingPointCounter(rp, plcName, point, 6)
	testutil.MustNoErr(t, err, "the point's count since its reset")
	lc := stationCounter(t, r.db, r.pid)
	_, err = r.db.UpdateLineCounterRead(lc, plcName, station, 30, time.Now())
	testutil.MustNoErr(t, err, "the station's count")
	testutil.MustNoErr(t, r.db.AnchorLineCounter(lc, plcName, station, 16, time.Now().Add(-time.Hour)), "the anchor")
	return r, n, lc
}

// TestCountedAt_TransitionLogsTheInLineNumber: binding a position to a station
// counter, and leaving it, logs the parts in the line at that counter (its
// clicks since the anchor less the production counts since the reset), the
// number the card shows; an unanchored counter says it has none yet.
func TestCountedAt_TransitionLogsTheInLineNumber(t *testing.T) {
	t.Parallel()
	r, n, lc := inLineRig(t)
	var lines []string
	r.eng.logFn = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	const want = "≈8 in the line; they net against this cart's run-out under the borrowing rule"
	testutil.MustNoErr(t, r.eng.SetCountedAt(n, &lc), "bind")
	testutil.MustNoErr(t, r.eng.SetCountedAt(n, nil), "unbind")
	if len(lines) != 2 || !strings.Contains(lines[0], want) || !strings.Contains(lines[1], want) {
		t.Fatalf("the binding's log = %q, want both to say %q", lines, want)
	}
	cards, err := r.eng.ListStationCounters(r.pid)
	testutil.MustNoErr(t, err, "the cards")
	if len(cards) != 1 || cards[0].InLine == nil || *cards[0].InLine != 8 {
		t.Fatalf("the card reads %+v, want 8 in the line", cards)
	}

	testutil.MustNoErr(t, r.db.ClearLineCounterAnchors(r.pid), "unanchor")
	lines = nil
	testutil.MustNoErr(t, r.eng.SetCountedAt(n, &lc), "bind unanchored")
	if len(lines) != 1 || !strings.Contains(lines[0], "not anchored yet") {
		t.Fatalf("an unanchored counter's binding logs %q, want it to say it has no number yet", lines)
	}
}
