// S4, the cell jumps parts with no changeover (decision 39): Shingo asks, and
// moves nothing until a person taps.
//
//go:build docker

package scenarios

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	"shingoedge/plc/simwarlink"
)

// TestCatid4x2_S4_JumpWithNoChangeover: RUN is active and both styles pinned
// when the PLC is forced onto ALT's part with no changeover open. The ALT
// counts are off-plan: the common positions' carts, which are ALT's parts
// too, are charged; the mirror positions' shares are held, their carts being
// RUN's part. Nothing moves: past the window the old automatic switch acted
// after, the process runs RUN, no changeover row exists and no order was made,
// and the wrong-part item, red, offers the changeover to ALT. Untapped, the
// mirror holds grow with every stroke, the common positions charge, the record
// is ALT's by CATID and nothing is ordered. Tapped, the item's action starts
// the changeover; its legs land, the monitor completes it, and the completion
// lands the held shares on the MIRROR-R carts: no hold is left, every ledger
// is exact.
func TestCatid4x2_S4_JumpWithNoChangeover(t *testing.T) {
	t.Parallel()
	for _, half := range []string{"untapped", "tapped"} {
		t.Run(half, func(t *testing.T) {
			t.Parallel()
			c := newW6Cell(t, w6Opts{})
			c.cycle(14)
			run := c.counted()
			before := c.lastID()
			c.force(catALT)
			forced := time.Now()
			c.cycle(6) // ALT's first counts: the parts that were in the line
			offPlan := c.counted() - run
			c.wantMirrorShares("S4 the jump", offPlan)
			c.ledger("S4 the jump", true)
			item := c.asksAndMovesNothing("S4 "+half, forced, before)
			if half == "untapped" {
				c.untapped("S4", run, before)
				return
			}
			c.tapped("S4", item, run, false)
		})
	}
}

// wantMirrorShares fails unless the holds ledger keeps one open off-plan
// MIRROR-R share of n at each mirror position, and nothing else.
func (c *w6Cell) wantMirrorShares(label string, n int64) {
	c.t.Helper()
	h := c.holds()
	if len(h.rows) != 2 || h.open != 2*n {
		c.dump(label)
		c.t.Fatalf("%s: holds %+v, want one open off-plan MIRROR-R share of %d at each mirror position", label, h.rows, n)
	}
	for _, r := range h.rows {
		if i := c.posOf(r.ProcessNodeID); (i != 3 && i != 4) || r.PayloadCode != "MIRROR-R" || r.Qty != n || r.ResolvedAt != nil {
			c.t.Fatalf("%s: hold %+v, want MIRROR-R's open share of %d at a mirror position", label, r, n)
		}
	}
}

// asksAndMovesNothing waits out the stability window the automatic switch
// once acted after (from forced, the confirm) and counts one more part, then
// fails unless nothing moved — the process on RUN, no changeover row, no order
// made since before — and returns the wrong-part item, which must be red and
// offer the changeover to ALT, its sentence naming the mirror positions' held
// strokes.
func (c *w6Cell) asksAndMovesNothing(label string, forced time.Time, before int64) edgeengine.CatidHealthItem {
	c.t.Helper()
	time.Sleep(time.Until(forced.Add(declareWindow)))
	c.cycle(1)
	c.afterTheDoor()
	if a := c.active(); a != c.run {
		c.dump(label)
		c.t.Fatalf("%s: the process runs style %d past the window, want RUN (%d): nothing switches without a changeover",
			label, a, c.run)
	}
	if rows := c.changeovers(); len(rows) != 0 {
		c.t.Fatalf("%s: changeover rows %s, want none", label, describeChangeovers(rows))
	}
	if orders := c.newRows(before); len(orders) != 0 {
		c.dump(label)
		c.t.Fatalf("%s: %d orders since the jump, want none: the part change orders nothing", label, len(orders))
	}
	item := c.wrongPart(label)
	if item.Severity != edgeengine.CatidHealthRed || item.Action == nil || item.Action.Kind != "start_changeover" ||
		item.Action.ToStyleID != c.alt || !strings.Contains(item.Headline, "Start a changeover to ALT") ||
		!strings.Contains(item.Headline, "held at W6-P4 and W6-P5") {
		c.t.Fatalf("%s: the wrong-part item %+v (action %+v), want it red, offering the changeover to ALT with the mirror "+
			"positions' held strokes named", label, item, item.Action)
	}
	c.t.Logf("ITEM %s: [%s] %s (%s); action %s to %s (style %d)", label, item.Severity, item.Headline, item.Detail,
		item.Action.Kind, item.Action.ToStyleName, item.Action.ToStyleID)
	return item
}

// wrongPart is the strip's wrong-part item for the cell.
func (c *w6Cell) wrongPart(label string) edgeengine.CatidHealthItem {
	c.t.Helper()
	items, err := c.edge.Engine.CatidHealth(c.processID)
	mustNil(c.t, err, "the strip")
	for _, it := range items {
		if it.Kind == "wrong_part" {
			return it
		}
	}
	c.t.Fatalf("%s: no wrong-part item in %+v", label, items)
	return edgeengine.CatidHealthItem{}
}

// untapped runs the cell on, nobody tapping: the mirror positions' shares grow
// with every stroke, the common positions charge every count, the record is
// ALT's by CATID, nothing is ordered (a pass of the sweep included) and the
// item stays red.
func (c *w6Cell) untapped(label string, run, before int64) {
	c.t.Helper()
	c.flush()
	common := c.drawnNow()
	from := c.counted()
	c.cycle(6)
	n := c.counted() - from
	c.wantMirrorShares(label+" untapped", c.counted()-run)
	c.flush()
	now := c.drawnNow()
	for i := 0; i < 3; i++ {
		if now[i]-common[i] != n {
			c.t.Errorf("%s untapped: the common position %s drew %d over %d ALT counts, want them all", label, w6Pos[i],
				now[i]-common[i], n)
		}
	}
	if orders := c.sweep(); len(orders) != 0 {
		c.dump(label)
		c.t.Fatalf("%s untapped: the sweep made %d orders, want none", label, len(orders))
	}
	if orders := c.newRows(before); len(orders) != 0 {
		c.t.Fatalf("%s untapped: %d orders since the jump, want none", label, len(orders))
	}
	if it := c.wrongPart(label); it.Severity != edgeengine.CatidHealthRed || it.Action == nil {
		c.t.Fatalf("%s untapped: the wrong-part item %+v, want it still red with its action", label, it)
	}
	l := c.ledger(label+" untapped", true)
	c.wantRecord(label+" untapped", l, run, c.counted()-run)
	if l.holdsOpen != 2*(c.counted()-run) {
		c.t.Fatalf("%s untapped: %d held open, want every ALT count at both mirror positions (%d)", label, l.holdsOpen,
			2*(c.counted()-run))
	}
	c.line(label+" untapped", l)
}

// tapped is the person's tap on the item: the start verb, as the strip calls
// it; the changeover's legs land, the monitor completes it, and the
// completion lands the held shares on the MIRROR-R carts. Every ledger is
// exact, no hold is left and every order lands confirmed.
//
// overCartsMayStay: S9's shares grew at the hand-placed carts for the whole
// untapped stretch, and a share bigger than the MIRROR-R cart that lands for
// it did not come from that cart, so the completion leaves it open for a
// person (decision 46). The ledger is still exact — held is a term — and the
// assertion is that no cart went below 0, not that no hold is open.
func (c *w6Cell) tapped(label string, item edgeengine.CatidHealthItem, run int64, overCartsMayStay bool) {
	c.t.Helper()
	before := c.lastID()
	if _, err := c.edge.Engine.StartProcessChangeover(c.processID, item.Action.ToStyleID, "engineer",
		"from the health strip: the PLC is on its part"); err != nil {
		c.dump(label + " tap")
		c.t.Fatalf("%s: the tap's start: %v", label, err)
	}
	c.tick()
	c.carryAll(label+" the changeover's legs", before, nil)
	c.waitFor(label+" the changeover completed by the monitor", declareWindow+10*time.Second, func() bool {
		return c.open() == nil
	})
	co := c.changeovers()[0]
	if co.ToStyleID != c.alt || c.active() != c.alt || !strings.HasPrefix(co.TriggeredBy, "auto-catid") {
		c.dump(label)
		c.t.Fatalf("%s: changeover #%d %s by %q to %d, the cell on %d; want completed by the monitor on ALT", label, co.ID,
			co.State, co.TriggeredBy, co.ToStyleID, c.active())
	}
	c.t.Logf("%s tapped: changeover #%d called by %q, completed by %s", label, co.ID, co.CalledBy, co.TriggeredBy)
	for _, i := range []int{3, 4} {
		if tn := c.cart(i); tn == nil || tn.part != "MIRROR-R" {
			c.dump(label)
			c.t.Fatalf("%s: %s holds %+v after the changeover, want a MIRROR-R cart", label, w6Pos[i], tn)
		}
	}
	c.cycle(5)
	l := c.ledger(label+" tapped", true)
	c.wantRecord(label+" tapped", l, run, c.counted()-run)
	switch {
	case overCartsMayStay:
		for i := range w6Pos {
			for _, tn := range c.tenures[i] {
				if n := c.count(tn); n < 0 {
					c.dump(label)
					c.t.Fatalf("%s tapped: a cart at %s reads %d: a release landed more than it held", label, w6Pos[i], n)
				}
			}
		}
		c.t.Logf("%s tapped: %d hold(s) open, each over its cart", label, l.holdsOpen)
	case l.holdsOpen != 0:
		c.t.Fatalf("%s tapped: %d held open, want none: the completion lands the held shares", label, l.holdsOpen)
	}
	c.confirmedAll(label + " tapped")
	c.line(label+" tapped", l)
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
