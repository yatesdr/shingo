// S1, the steady run: one style, the 4x2-shaped cell counted two ways.
//
//go:build docker

package scenarios

import "testing"

// TestCatid4x2_S1_SteadyRun runs RUN with no changeover. Counted at the load
// station, every cart is exact at every read, nothing is held, and the record
// is RUN's. Counted at the cell counter, the carts follow the prepay profile:
// a run's first carts read L high (the parts in the line are counted later);
// at a swap the line runs out onto the landed cart, which reads L low until the
// line refills and exact after; the departing cart's last L went to its
// successor; and per position the draws equal the counts.
func TestCatid4x2_S1_SteadyRun(t *testing.T) {
	t.Parallel()
	t.Run("station-counted", func(t *testing.T) {
		t.Parallel()
		c := newW6Cell(t, w6Opts{station: []int{0, 1, 2, 3, 4}})
		for _, n := range []int{5, 9, 13} {
			c.cycle(n)
			l := c.ledger("station-counted", true)
			c.wantRecord("station-counted", l, c.counted(), 0)
			if l.holdsOpen != 0 {
				t.Fatalf("station-counted: %d held, want none", l.holdsOpen)
			}
		}
		l := c.ledger("station-counted", true)
		c.confirmedAll("station-counted")
		c.line("S1 station-counted", l)
	})
	t.Run("cell-counted", func(t *testing.T) {
		t.Parallel()
		c := newW6Cell(t, w6Opts{})
		c.cycle(20)
		c.offsets("the line filled", w6Depth)
		c.ledger("cell-counted, the line filled", true)

		// A swap at every position: the station stops loading while the carts
		// are swapped and the line runs out. The fake's one verb that stops
		// loading and lets the line count out is the program change, so the
		// swap rides a change to the program already running (the counter
		// resets, as a program change resets it; the identity does not move).
		first := [5]*w6Tenure{}
		for i := range w6Pos {
			first[i] = c.cart(i)
		}
		c.change(catRUN)
		c.swapAll("the swap")
		c.cycle(w6Depth + 1)
		c.offsets("the line ran out onto the landed carts", -w6Depth)
		for i, tn := range first {
			if off := (w6Full - c.count(tn)) - c.truth(tn); off != -w6Depth {
				t.Fatalf("%s: the departed cart %s drew %d against %d taken, want its last %d on its successor",
					w6Pos[i], tn.label, w6Full-c.count(tn), c.truth(tn), w6Depth)
			}
		}
		c.ledger("cell-counted, run out", true)
		c.cycle(w6Depth)
		c.offsets("the line refilled", 0)
		c.cycle(6)
		c.offsets("running on", 0)
		l := c.ledger("cell-counted", true)
		c.wantRecord("cell-counted", l, c.counted(), 0)
		if l.holdsOpen != 0 {
			t.Fatalf("cell-counted: %d held, want none", l.holdsOpen)
		}
		c.confirmedAll("cell-counted")
		c.line("S1 cell-counted", l)
	})
}
