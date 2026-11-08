// S6, the mixed cell: three positions on the load station's counter, two on
// the cell counter.
//
//go:build docker

package scenarios

import "testing"

// TestCatid4x2_S6_MixedCell: a changeover RUN -> ALT runs through both
// counting paths (a common and a mirror position on each), and then the
// program jumps to a part no style pins. Every position's draws equal the
// parts counted at its own counter; the unmapped part's draw is held per
// position at the station's positions and whole on the node-0 row for the
// cell counter's, which the station strokes never write.
func TestCatid4x2_S6_MixedCell(t *testing.T) {
	t.Parallel()
	c := newW6Cell(t, w6Opts{station: []int{0, 1, 3}})
	c.cycle(12)
	after := c.startChangeover(c.alt)
	c.change(catALT)
	c.carryAll("the changeover's legs", after, nil)
	c.cycle(w6Depth + 1) // the run-out, then the program change
	mustNil(t, c.edge.Engine.CompleteProcessProductionCutover(c.processID), "CUTOVER")
	run := c.counted()
	c.cycle(w6Depth + 5)
	c.ledger("S6 the changeover", true)

	c.force(1003)
	loads, counts := c.plc.state().Loads, c.counted()
	c.cycle(4)
	loads, counts = c.plc.state().Loads-loads, c.counted()-counts
	c.force(catALT)
	c.cycle(3)
	h := c.shares()
	for _, r := range h.rows {
		i, want := c.posOf(r.ProcessNodeID), counts
		switch {
		case r.ProcessNodeID == 0:
		case i >= 0 && c.station[i]:
			want = loads
		default:
			t.Fatalf("hold %+v: a row at a position the cell counter counts, want its share on the node-0 row", r)
		}
		if r.CATID != "1003" || r.Qty != want || r.ResolvedAt != nil {
			t.Fatalf("hold %+v: want 1003's open draw of %d (station loads %d, cell counts %d)", r, want, loads, counts)
		}
	}
	if len(h.rows) != 4 {
		t.Fatalf("1003's holds: %+v, want one row per station position and the node-0 row", h.rows)
	}
	l := c.ledger("S6", true)
	c.wantRecord("S6", l, run, c.counted()-run)
	c.confirmedAll("S6")
	c.line("S6", l)
}
