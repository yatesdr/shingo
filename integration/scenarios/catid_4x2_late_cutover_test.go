// S2, the late cutover (the pattern the plant's record shows): the program
// changes before anyone cuts over.
//
//go:build docker

package scenarios

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"shingoedge/store/processes"
)

// TestCatid4x2_S2_LateCutover: a changeover RUN -> ALT starts, the mirror
// carts land as the station stops loading, the line runs out and the program
// changes to ALT's part, and nobody presses CUTOVER. With every leg done the
// declaration completes the changeover once the part has stood the 60 s
// window; with a position's legs held back (the gate red: its new cart has
// not landed) the first counted ALT part forces it, and the held legs land
// after as ordinary orders. Either way the counts after the change are ALT's on both sides of
// the cutover, the mirror carts are right, and every order lands confirmed.
func TestCatid4x2_S2_LateCutover(t *testing.T) {
	t.Parallel()
	for _, v := range []struct {
		name string
		hold map[int]bool
		by   string
	}{
		{"green gate", nil, "auto-catid"},
		{"red gate", map[int]bool{4: true}, "auto-catid-forced"},
	} {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			c := newW6Cell(t, w6Opts{})
			c.cycle(14)
			after := c.startChangeover(c.alt)
			c.cycle(3) // the legs travel; the cell still runs RUN
			c.change(catALT)
			c.carryAll("the changeover's legs", after, v.hold)
			c.cycle(w6Depth + 1) // the run-out, then the program change
			changed := time.Now()
			run := c.counted()
			if c.open() == nil {
				t.Fatalf("the changeover completed before its window: %s", describeChangeovers(c.changeovers()))
			}
			c.cycle(w6Depth + 4) // ALT refills the line and its first parts are counted
			c.waitFor("the changeover completed by the monitor", autoSwitchWindow+10*time.Second, func() bool {
				return c.open() == nil
			})
			took := time.Since(changed)
			co := c.changeovers()[0]
			if co.TriggeredBy != v.by || co.ToStyleID != c.alt || c.active() != c.alt {
				c.dump(v.name)
				t.Fatalf("%s: changeover #%d %s by %q to %d, the cell on %d; want completed by %q on ALT", v.name, co.ID,
					co.State, co.TriggeredBy, co.ToStyleID, c.active(), v.by)
			}
			t.Logf("%s: completed by %s %s after the program change", v.name, co.TriggeredBy, took.Round(time.Second))
			c.cycle(5) // past the cutover
			if v.hold != nil {
				// The force left the held legs waiting; released now, they land
				// as ordinary orders.
				c.carryAll("the held legs", after, nil)
				c.cycle(3)
			}
			for _, i := range []int{3, 4} {
				if tn := c.cart(i); tn == nil || !strings.HasPrefix(tn.label, "MIRROR-R") {
					c.dump(v.name)
					t.Fatalf("%s holds %+v after the changeover, want a MIRROR-R cart", w6Pos[i], tn)
				}
			}
			l := c.ledger("S2 "+v.name, true)
			c.wantRecord("S2 "+v.name, l, run, c.counted()-run)
			if l.holdsOpen != 0 {
				t.Fatalf("%s: %d held, want none", v.name, l.holdsOpen)
			}
			c.confirmedAll("S2 " + v.name)
			c.line("S2 "+v.name, l)
		})
	}
}

// autoSwitchWindow is the CATID monitor's 60 s stability window (the process
// clock, the wall's here), and a read past it.
const autoSwitchWindow = 62 * time.Second

// startChangeover is the operator's START to style to.
func (c *w6Cell) startChangeover(to int64) int64 {
	c.t.Helper()
	before := c.lastID()
	if _, err := c.edge.Engine.StartProcessChangeover(c.processID, to, "w6-operator", ""); err != nil {
		c.dump("START")
		c.t.Fatalf("START: %v", err)
	}
	c.tick()
	return before
}

// active is the process's running style.
func (c *w6Cell) active() int64 {
	p, err := c.edge.DB.GetProcess(c.processID)
	mustNil(c.t, err, "process")
	if p.ActiveStyleID == nil {
		return 0
	}
	return *p.ActiveStyleID
}

// open is the open changeover, nil when none.
func (c *w6Cell) open() *processes.Changeover {
	co, err := c.edge.DB.GetActiveProcessChangeover(c.processID)
	if err != nil {
		return nil
	}
	return co
}

// changeovers is the process's changeover rows, newest first, as
// "id:state:triggered_by" for a log line.
func (c *w6Cell) changeovers() []processes.Changeover {
	c.t.Helper()
	list, err := c.edge.DB.ListRecentProcessChangeovers(c.processID, 20)
	mustNil(c.t, err, "changeovers")
	return list
}

func describeChangeovers(list []processes.Changeover) string {
	var parts []string
	for _, co := range list {
		parts = append(parts, fmt.Sprintf("#%d %s by %q", co.ID, co.State, co.TriggeredBy))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
