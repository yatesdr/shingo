// S3, the early cutover (decisions 23-24): "Mark as running" at the landing
// while the PLC still runs RUN's part.
//
//go:build docker

package scenarios

import (
	"fmt"
	"testing"
	"time"
)

// TestCatid4x2_S3_EarlyCutover: the cell has run RUN past the switch's
// window; a changeover to ALT starts; the mirror carts are swapped as the
// station stops loading, and the person marks ALT running at the landing while
// RUN's last L parts are still in the line. Those parts are the run-out:
// attributed today (the point's style, ALT), they prepay the new carts on the
// cell counter and leave them untouched on the station's; no hold opens, and
// the process never switches back to RUN. The landing preceded the counter's
// reset, so the changeover's history row says it ran out L after the cutover.
func TestCatid4x2_S3_EarlyCutover(t *testing.T) {
	t.Parallel()
	for _, v := range []struct {
		name    string
		station []int
	}{{"cell-counted", nil}, {"station-counted", []int{0, 1, 2, 3, 4}}} {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			c := newW6Cell(t, w6Opts{station: v.station})
			booted := time.Now()
			c.cycle(14)
			// RUN's part has stood past the switch's window: a stroke the
			// monitor took for another style's would switch the cell now.
			time.Sleep(time.Until(booted.Add(autoSwitchWindow)))
			c.cycle(2)

			after := c.startChangeover(c.alt)
			c.change(catALT) // the station stops loading RUN; the line runs out
			c.carryAll("the changeover's legs", after, nil)
			runBefore := c.counted()
			if err := c.edge.Engine.SetProcessActiveStyle(c.processID, &c.alt); err != nil {
				t.Fatalf("Mark as running: %v", err)
			}
			c.cycle(w6Depth) // the run-out: RUN's part, ALT running
			time.Sleep(3 * time.Second)
			c.cycle(w6Depth + 7) // the change lands; ALT refills the line and runs

			if a := c.active(); a != c.alt {
				t.Errorf("the process runs style %d after the run-out, want ALT (%d): it switched back; changeovers %s",
					a, c.alt, describeChangeovers(c.changeovers()))
			}
			for _, co := range c.changeovers() {
				if co.TriggeredBy == "auto-catid-flip" {
					t.Errorf("changeover row #%d: an automatic switch (%s -> %d) after an early cutover", co.ID,
						co.TriggeredBy, co.ToStyleID)
				}
			}
			// The program change anchored the load station: what it has
			// loaded since, less what the counter has counted, is the line.
			if v.station != nil {
				if n := c.inLine("S3 station-counted, after the program change"); n == nil || *n != w6Depth ||
					*n != int64(c.plc.state().InFlight) {
					got := "unanchored"
					if n != nil {
						got = fmt.Sprint(*n)
					}
					t.Errorf("the parts in the line read %s after the program change, want L = %d (the fake has %d in flight)",
						got, w6Depth, c.plc.state().InFlight)
				}
			}
			var landed int64
			for _, co := range c.changeovers() {
				if co.ToStyleID == c.alt && co.CompletedAt != nil {
					landed = co.ID
				}
			}
			if n := c.runOut("S3 "+v.name, landed); n == nil || *n != w6Depth {
				got := "none"
				if n != nil {
					got = fmt.Sprint(*n)
				}
				t.Errorf("changeover #%d's history row: run-out %s, want L = %d", landed, got, w6Depth)
			}
			if h := c.holds(); len(h.rows) != 0 {
				t.Errorf("the run-out opened %d hold(s), %d held, want none: %+v", len(h.rows), h.open, h.rows)
			}
			l := c.ledger("S3 "+v.name, true)
			c.wantRecord("S3 "+v.name, l, runBefore, c.counted()-runBefore)
			if v.station == nil {
				for _, i := range []int{3, 4} {
					if tn := c.cart(i); c.counts(tn) != c.truth(tn) {
						t.Fatalf("%s: the new cart %s drew %d counts against %d taken: the run-out did not prepay it",
							w6Pos[i], tn.label, c.counts(tn), c.truth(tn))
					}
				}
			}
			c.confirmedAll("S3 " + v.name)
			c.line("S3 "+v.name, l)
		})
	}
}
