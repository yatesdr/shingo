// S7, the restart: the Edge restarts mid-run and mid-changeover.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"
)

// TestCatid4x2_S7_Restart: a mixed cell (both counting paths) holding an
// unmapped part's draws restarts mid-run, then mid-changeover after the
// program has changed to ALT's part with a position's legs held back. Nothing
// is counted twice at either counter; the holds come back as they were; the
// monitor's first read after the restart is the part already standing, so
// the counted-part force does not fire on it (it waits for the press to
// change again) while the gate is red; once the held legs land, the gated
// declaration completes the changeover.
func TestCatid4x2_S7_Restart(t *testing.T) {
	t.Parallel()
	c := newW6Cell(t, w6Opts{station: []int{0, 1, 3}})
	c.cycle(10)
	c.force(1003) // a spell on a part no style pins: its draws are held
	c.cycle(3)
	c.force(catRUN)
	c.cycle(4)
	held := c.holds()
	if held.open == 0 {
		t.Fatalf("the unmapped spell held nothing: %+v", held.rows)
	}

	c.restart("mid-run")
	c.cycle(6)
	c.ledger("S7 after the restart mid-run", false)
	c.sameHolds("S7 after the restart mid-run", held)

	after := c.startChangeover(c.alt)
	c.change(catALT)
	c.carryAll("the changeover's legs", after, map[int]bool{4: true})
	c.cycle(w6Depth + 1) // the run-out, then the program change to ALT
	run := c.counted()
	c.cycle(w6Depth + 2) // ALT's first counts, before the restart
	c.restart("mid-changeover")
	c.cycle(3) // and after it
	c.ledger("S7 after the restart mid-changeover", false)
	time.Sleep(autoSwitchWindow)
	if co := c.open(); co == nil {
		t.Fatalf("the changeover completed on the part standing at the restart, with its gate red: %s",
			describeChangeovers(c.changeovers()))
	}

	c.carryAll("the held legs", after, nil)
	c.waitFor("the gated declaration completes the changeover", autoSwitchWindow+10*time.Second, func() bool {
		return c.open() == nil
	})
	if co := c.changeovers()[0]; co.TriggeredBy != "auto-catid" || c.active() != c.alt {
		t.Fatalf("changeover #%d completed by %q, the cell on %d; want the gated declaration onto ALT", co.ID,
			co.TriggeredBy, c.active())
	}
	c.cycle(4)
	l := c.ledger("S7", false)
	c.sameHolds("S7", held)
	c.wantRecord("S7", l, run, c.counted()-run)
	c.confirmedAll("S7")
	c.line("S7", l)
}

// sameHolds fails unless the holds ledger is as it was.
func (c *w6Cell) sameHolds(label string, was w6Holds) {
	c.t.Helper()
	now := c.holds()
	if len(now.rows) != len(was.rows) || now.open != was.open {
		c.t.Fatalf("%s: holds %+v, want them as they were: %+v", label, now.rows, was.rows)
	}
	for i := range now.rows {
		a, b := now.rows[i], was.rows[i]
		if a.ID != b.ID || a.Qty != b.Qty || (a.ResolvedAt == nil) != (b.ResolvedAt == nil) {
			c.t.Fatalf("%s: hold %+v, was %+v", label, a, b)
		}
	}
}
