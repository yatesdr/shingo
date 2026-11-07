// S4, the cell jumps parts with no changeover (decision 20): the automatic
// switch.
//
//go:build docker

package scenarios

import (
	"cmp"
	"fmt"
	"slices"
	"testing"
	"time"

	"shingoedge/domain"
	"shingoedge/plc/simwarlink"
)

// TestCatid4x2_S4_JumpWithNoChangeover: RUN is active and both styles pinned
// when the PLC is forced onto ALT's part with no changeover open. The first
// ALT counts are off-plan: the common positions' carts, which are ALT's parts
// too, are charged; the mirror positions' shares are held, their carts being
// RUN's part. After the 60 s window the automatic switch fires on a counted
// part and writes its row; its release lands nothing on the MIRROR-L carts
// still standing there, so the mirror holds stay open. The next sweep
// reorders under ALT, and the reorders land.
func TestCatid4x2_S4_JumpWithNoChangeover(t *testing.T) {
	t.Parallel()
	// Every claim's level at 36, so the first sweep after the switch reorders
	// at every position (the sweep runs only when the scenario fires it).
	c := newW6Cell(t, w6Opts{reorder: 36})
	c.cycle(14)
	run := c.counted()
	c.force(catALT)
	c.cycle(6) // ALT's first counts: the parts that were in the line
	offPlan := c.counted() - run

	h := c.holds()
	if len(h.rows) != 2 || h.open != 2*offPlan {
		t.Fatalf("after the jump: holds %+v, want one open off-plan MIRROR-R row of %d at each mirror position",
			h.rows, offPlan)
	}
	for _, r := range h.rows {
		if i := c.posOf(r.ProcessNodeID); (i != 3 && i != 4) || r.PayloadCode != "MIRROR-R" || r.Qty != offPlan {
			t.Fatalf("after the jump: hold %+v, want MIRROR-R's share of %d at a mirror position", r, offPlan)
		}
	}
	c.ledger("S4 the jump", true)
	mirror := [2]int64{c.count(c.cart(3)), c.count(c.cart(4))}

	c.waitFor("the automatic switch to ALT", autoSwitchWindow+10*time.Second, func() bool { return c.active() == c.alt })
	c.afterTheDoor() // the switch flips first; its row and its release follow under the door
	if co := c.changeovers()[0]; co.TriggeredBy != "auto-catid-flip" || co.ToStyleID != c.alt {
		t.Fatalf("the switch's row: #%d by %q to %d, want auto-catid-flip to ALT", co.ID, co.TriggeredBy, co.ToStyleID)
	}
	c.flush()
	if h := c.holds(); h.open != 2*offPlan {
		t.Fatalf("after the switch: %d held open, want the mirror shares (%d) still open: %+v", h.open, 2*offPlan, h.rows)
	}
	for k, i := range []int{3, 4} {
		if got := c.count(c.cart(i)); got != mirror[k] {
			t.Fatalf("the switch's release drew the %s cart at %s: %d -> %d", c.cart(i).label, w6Pos[i], mirror[k], got)
		}
	}

	orders := c.sweep()
	got := map[string]string{}
	for _, o := range orders {
		if o.DeliveryNode != "" {
			got[o.DeliveryNode] = o.PayloadCode
		}
	}
	for i, part := range w6AltParts {
		if got[w6Pos[i]] != part {
			c.dump("the sweep")
			t.Fatalf("the sweep after the switch: %s reordered %q, want ALT's %s (orders %v)", w6Pos[i], got[w6Pos[i]], part, got)
		}
	}
	c.carryAll("the reorders", orders[0].ID-1, nil)
	c.cycle(5)

	l := c.ledger("S4", true)
	c.wantRecord("S4", l, run, c.counted()-run)
	if l.holdsOpen != 2*offPlan {
		t.Fatalf("S4: %d held open at the end, want the mirror shares %d", l.holdsOpen, 2*offPlan)
	}
	c.confirmedAll("S4")
	c.line("S4", l)
}

// force is a program change without the drain: the identity and the reset at
// once, the loads in flight counted out under v. It waits for the confirm.
func (c *w6Cell) force(v int64) {
	c.t.Helper()
	seen := c.observations(v)
	c.plc.do(func(l *simwarlink.Line) { l.Force(v) })
	c.publish()
	c.awaitConfirm(fmt.Sprintf("the forced change to %d", v), v, seen)
}

// sweep is one pass of the demand reconciler (its level sweep), as its
// minute ticker would run it; it returns the orders the pass made, oldest
// first (the rows come newest first by the second they were made in, so two
// made in different seconds put the later ones first).
func (c *w6Cell) sweep() []domain.Order {
	c.t.Helper()
	before := c.lastID()
	c.tickers.sweep.pass(c.t, "the demand sweep")
	c.tick()
	orders := c.newRows(before)
	slices.SortFunc(orders, func(a, b domain.Order) int { return cmp.Compare(a.ID, b.ID) })
	return orders
}

// afterTheDoor returns once the changeover door is free: a Settings save that
// changes nothing takes the door's mutex, so it waits out a switch or a
// cutover in flight on the monitor's goroutine.
func (c *w6Cell) afterTheDoor() {
	c.t.Helper()
	mustNil(c.t, c.edge.Engine.EditProcessCounter(c.processID,
		domain.ProcessCounterEdit{PLCName: w6PLC, TagName: w6Counter, Enabled: true}), "a Settings save")
}
