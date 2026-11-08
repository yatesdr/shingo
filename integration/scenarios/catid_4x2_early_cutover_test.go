// S3, the early cutover (decisions 23-24): "Mark as running" at the landing
// while the PLC still runs RUN's part.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"
)

// TestCatid4x2_S3_EarlyCutover: a changeover to ALT starts; the mirror carts
// are swapped as the station stops loading, and the person marks ALT running
// at the landing while RUN's last L parts are still in the line. Those parts
// are the run-out: attributed today (the point's style, ALT), they prepay the
// new carts on the cell counter and leave them untouched on the station's; no
// hold opens, the process stays on ALT, and the wrong-part item offers no
// changeover back to RUN (the PLC's part stood from before the landing). The
// landing preceded the counter's reset, so the changeover's history row says
// it ran out L after the cutover.
func TestCatid4x2_S3_EarlyCutover(t *testing.T) {
	t.Parallel()
	for _, v := range []struct {
		name    string
		station []int
	}{{"cell-counted", nil}, {"station-counted", []int{0, 1, 2, 3, 4}}} {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			c := newW6Cell(t, w6Opts{station: v.station})
			c.cycle(16)

			after := c.startChangeover(c.alt)
			c.change(catALT) // the station stops loading RUN; the line runs out
			c.carryAll("the changeover's legs", after, nil)
			runBefore := c.counted()
			pre := map[int]int64{}
			for _, i := range []int{3, 4} {
				pre[i] = c.loggedAt(c.holds(), i)
			}
			if err := c.edge.Engine.SetProcessActiveStyle(c.processID, &c.alt); err != nil {
				t.Fatalf("Mark as running: %v", err)
			}
			c.cycle(w6Depth) // the run-out: RUN's part, ALT running
			if it := c.wrongPart("S3 " + v.name + " run-out"); it.Action != nil {
				t.Errorf("during the run-out the wrong-part item offers %+v (%q), want nothing: RUN's part stood from "+
					"before the landing", it.Action, it.Headline)
			}
			time.Sleep(3 * time.Second)
			c.cycle(w6Depth + 7) // the change lands; ALT refills the line and runs

			if a := c.active(); a != c.alt {
				t.Errorf("the process runs style %d after the run-out, want ALT (%d); changeovers %s",
					a, c.alt, describeChangeovers(c.changeovers()))
			}
			l := c.ledger("S3 "+v.name, true)
			c.wantRecordRunOut("S3 "+v.name, l, runBefore)
			if v.station == nil {
				// The run-out is RUN's part, and so are the counts the cell made
				// before the monitor confirmed 1002 (the tag still read 1001): at
				// the mirror positions, where ALT's carts stand, they are logged,
				// and ALT's carts draw ALT's counts alone (decision 55).
				h := c.holds()
				for _, i := range []int{3, 4} {
					tn := c.cart(i)
					drew, logged := w6Full-c.count(tn), c.loggedAt(h, i)-pre[i]
					if drew > c.counts(tn) || drew+logged != c.counts(tn)+w6Depth {
						t.Fatalf("%s: the new cart %s drew %d and %d were logged since the landing, against %d of its part "+
							"counted and the run-out of %d: it is never pre-paid, and every count is a cart's or the ledger's",
							w6Pos[i], tn.label, drew, logged, c.counts(tn), w6Depth)
					}
					c.t.Logf("S3: %s: the new cart drew %d of ALT's; %d logged (the run-out %d, and %d before the confirm)",
						w6Pos[i], drew, logged, w6Depth, logged-w6Depth)
				}
			}
			c.confirmedAll("S3 " + v.name)
			c.line("S3 "+v.name, l)
		})
	}
}
