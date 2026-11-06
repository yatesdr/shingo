// counted_at_test.go — binding positions to a station counter from its card:
// the dry run binds nothing, a binding is a person's save, and a binding made
// mid-run nets within the line under the borrowing rule.
package engine

import (
	"testing"

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
