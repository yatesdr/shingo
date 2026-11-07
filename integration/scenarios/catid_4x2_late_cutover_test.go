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
// With the legs held, the held-back position's MIRROR-L cart stays bound past
// the force and no ALT stroke draws it: its count stands from the program
// change while the seat holds every ALT stroke there, and the MIRROR-R cart
// takes them all when it lands.
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
			var atChange int64 // the held-back position's MIRROR-L cart at the program change
			if v.hold != nil {
				c.flush()
				atChange = c.count(c.cart(4))
			}
			if c.open() == nil {
				t.Fatalf("the changeover completed before its window: %s", describeChangeovers(c.changeovers()))
			}
			c.cycle(w6Depth + 4) // ALT refills the line and its first parts are counted
			c.waitFor("the changeover completed by the monitor", declareWindow+10*time.Second, func() bool {
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
				held := c.heldBeside("S2 "+v.name+" mid-run", 4, atChange, c.counted()-run)
				// The force left the held legs waiting; released now, they land
				// as ordinary orders.
				c.carryAll("the held legs", after, nil)
				c.cycle(3)
				c.tookTheHold("S2 "+v.name, 4, atChange, held)
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

// heldBeside fails unless the cart of RUN's part standing at position i reads
// at, its count at the program change, and the seat holds n of ALT's part
// there: every ALT stroke since, none drawn from that cart. It logs the read
// and returns n.
func (c *w6Cell) heldBeside(label string, i int, at, n int64) int64 {
	c.t.Helper()
	c.flush()
	tn := c.cart(i)
	rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
	mustNil(c.t, err, "runtime")
	if tn == nil || tn.part != w6RunParts[i] {
		c.t.Fatalf("%s: %s holds %+v, want the %s cart its legs kept", label, w6Pos[i], tn, w6RunParts[i])
	}
	if got := c.count(tn); got != at || rt.PendingUOPDelta != n || rt.PendingUOPPayload != w6AltParts[i] {
		c.dump(label)
		c.t.Fatalf("%s: %s: cart %s reads %d (at the program change %d: %d of ALT's strokes drawn from it), the seat holds "+
			"%d of %q; want it standing at %d and the seat holding %d of %s", label, w6Pos[i], tn.label, got, at, at-got,
			rt.PendingUOPDelta, rt.PendingUOPPayload, at, n, w6AltParts[i])
	}
	c.t.Logf("HELD %s: %s holds %d of %s beside cart %s, which stands at %d from the program change", label, w6Pos[i], n,
		w6AltParts[i], tn.label, at)
	return n
}

// tookTheHold fails unless position i's RUN-part cart left at at (no ALT
// stroke drawn from it) and the ALT-part cart standing there now drew the n
// held strokes and its own, the seat's hold replayed.
func (c *w6Cell) tookTheHold(label string, i int, at, n int64) {
	c.t.Helper()
	c.flush()
	ts := c.tenures[i]
	if len(ts) < 2 || ts[len(ts)-2].part != w6RunParts[i] || ts[len(ts)-1].part != w6AltParts[i] {
		c.t.Fatalf("%s: %s's carts %+v, want the %s cart then the %s cart", label, w6Pos[i], ts, w6RunParts[i], w6AltParts[i])
	}
	old, cur := ts[len(ts)-2], ts[len(ts)-1]
	rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
	mustNil(c.t, err, "runtime")
	if got := c.count(old); got != at || rt.PendingUOPDelta != 0 {
		c.dump(label)
		c.t.Fatalf("%s: cart %s left at %d, want %d; the seat holds %d, want its hold replayed", label, old.label, got, at,
			rt.PendingUOPDelta)
	}
	drew := w6Full - c.count(cur)
	if own := c.counted() - cur.countsIn; drew != n+own {
		c.dump(label)
		c.t.Fatalf("%s: cart %s drew %d, want the %d held and its own %d", label, cur.label, drew, n, own)
	}
	c.t.Logf("HELD %s: cart %s left at %d; cart %s drew %d (the %d held and %d of its own)", label, old.label, at, cur.label,
		drew, n, drew-n)
}

// declareWindow is the CATID monitor's 60 s stability window (the process
// clock, the wall's here), and a read past it.
const declareWindow = 62 * time.Second

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
