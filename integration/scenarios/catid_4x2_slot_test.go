// S8, the counted slot stops and a sibling moves: the slot watch's item, the
// click that re-points the counter, and the strokes since the move counted.
//
//go:build docker

package scenarios

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	"shingoedge/plc/simwarlink"
)

const (
	w6Counter2  = "MES_WELD_6.Prod_Counter_02"
	w6Identity2 = "MES_WELD_6.CATID_02"
	// slotWatch is the Edge's slot watch interval (engine.slotWatchInterval):
	// a pass a minute of the process clock (the wall's here), with no knob.
	slotWatch = time.Minute
	// w6Cadence is the line's machine cycle while S8 waits on the watch.
	w6Cadence = 10 * time.Second
	// slotSlack is the scheduling slack on the watch's minute (its ticker and
	// the scenario's read of the strip).
	slotSlack = 2 * time.Second
)

// TestCatid4x2_S8_CountedSlotStopsASiblingMoves: WELD-6 counts on
// Prod_Counter_01, the line running a cycle every 10 s, its positions counted
// at the load station in one half and at the production counter in the other.
// Once the Edge's slot watch has read the struct twice (the counter rising
// between the reads, standing from the second), the PLC's program moves the
// count to Prod_Counter_02: it ticks from then and CATID_02 reads the part;
// Prod_Counter_01 stops, CATID_01 stays. Within one watch interval the strip
// carries the red counter_slot_idle item naming Prod_Counter_02, and its click
// (the counter edit POST /api/processes/{id}/counter makes) re-points the
// counter: the item goes, the identity follows to CATID_02 (nothing is
// confirmed while WarLink does not publish it, though CATID_01 names the same
// part), and the point is seeded at _02's count when the PLC moved. Its first
// counted read waits for the monitor's read of CATID_02 and counts every part
// made on _02 since the move, under CATID_02's part and RUN: the record has
// every part, the carts equal the loads inside the window, and every order is
// confirmed.
func TestCatid4x2_S8_CountedSlotStopsASiblingMoves(t *testing.T) {
	t.Parallel()
	t.Run("station-counted", func(t *testing.T) {
		t.Parallel()
		s8(t, "S8 station-counted", w6Opts{station: []int{0, 1, 2, 3, 4}})
	})
	t.Run("cell-counted", func(t *testing.T) {
		t.Parallel()
		s8(t, "S8 cell-counted", w6Opts{})
	})
}

// s8 is one half of S8, on a cell counted as o says.
func s8(t *testing.T, label string, o w6Opts) {
	c := newW6Cell(t, o)
	c.cycle(w6Depth + 2) // the line full: the counter counts
	c.ledger(label+" steady", true)
	c.noSlotItem(label + " steady")
	c.twoPasses(label)
	c.noSlotItem(label + " the watch's pass before the move")
	_, before := c.countedRows()
	base := c.plc.state().Slots[1].Counter
	c.plc.do(func(l *simwarlink.Line) { mustNil(t, l.CountOn(2), "the count to slot 2") })
	moved := time.Now()
	stood := c.plc.state().Slots[0]
	c.cycle(1) // the first part counted on Prod_Counter_02, which no point reads
	item := c.awaitSlotIdle(label, moved)
	shown := time.Since(moved)
	if item.Severity != edgeengine.CatidHealthRed || item.Data["tag"] != w6Counter2 || item.Data["catid"] != fmt.Sprint(catRUN) ||
		item.Data["counted_tag"] != w6Counter {
		t.Fatalf("%s: the item %+v; want red, naming %s (CATID %d) against the counted %s", label, item, w6Counter2, catRUN, w6Counter)
	}
	t.Logf("%s: %.1fs after the move: %q (%s)", label, shown.Seconds(), item.Headline, item.Detail)

	seen := c.click(label, item)
	// WarLink's scan publishes the two members, the poll reads _02 (its strokes
	// wait for the part identity), the monitor reads CATID_02, the poll again.
	c.publish()
	c.tickers.poll.pass(t, "the poll")
	c.awaitConfirm("CATID_02 after the re-point", catRUN, seen)
	c.cycle(0)
	caught := time.Since(moved)
	since := c.plc.state().Slots[1].Counter - base
	l := c.ledger(label+", inside the window", true)
	c.wantRecord(label+", inside the window", l, c.counted(), 0)
	c.line(label+", inside the window", l)
	n, rows := c.rowsAfter(before)
	if n != since {
		t.Fatalf("%s: the record has %d parts counted since the move in %d rows, want the %d made on %s", label, n, len(rows),
			since, w6Counter2)
	}
	for _, r := range rows {
		if r.catid != fmt.Sprint(catRUN) || r.style != c.run {
			t.Fatalf("%s: a row since the move %+v; want CATID_02's %d under RUN (%d)", label, r, catRUN, c.run)
		}
	}

	// The next tick on Prod_Counter_02.
	_, last := c.countedRows()
	c.cycle(1)
	want := c.plc.state().Slots[1]
	if tick := c.rowAfter(last); tick.count != want.Counter || tick.delta != 1 || tick.catid != fmt.Sprint(want.CATID) ||
		tick.style != c.run {
		t.Fatalf("%s: the next tick on %s: %+v; want it reading %d, one part, CATID_02's %d, RUN (%d)",
			label, w6Counter2, tick, want.Counter, want.CATID, c.run)
	}
	c.cycle(4)
	l = c.ledger(label, true)
	c.wantRecord(label, l, c.counted(), 0)
	if l.holdsOpen != 0 {
		t.Fatalf("%s: %d held, want none", label, l.holdsOpen)
	}
	if s := c.plc.state().Slots[0]; s != stood {
		t.Fatalf("%s: Prod_Counter_01 / CATID_01 moved after the count left them: %+v -> %+v", label, stood, s)
	}
	c.confirmedAll(label)
	c.line(label, l)
	t.Logf("CATCH-UP %s: the item %.1fs after the move (watch interval %v); the %d parts counted on %s in the %.1fs to the "+
		"monitor's read of CATID_02 are in the record, in %d row(s) under CATID %d and RUN", label, shown.Seconds(), slotWatch,
		since, w6Counter2, caught.Seconds(), len(rows), catRUN)
}

// twoPasses runs the line until the slot watch has read the struct twice,
// the second time after the line's last count reached the cache: the counted
// slot rose between the two reads, so the watch saw it counting, and it
// stands from the second to the move.
func (c *w6Cell) twoPasses(label string) {
	c.t.Helper()
	var first time.Time
	if !c.runUntil(slotWatch+slotSlack, func() bool {
		first = c.plc.structReadAfter(time.Time{})
		return !first.IsZero()
	}) {
		c.t.Fatalf("%s: no read of %s in %v", label, w6Struct, slotWatch+slotSlack)
	}
	if !c.runUntil(slotWatch+slotSlack, func() bool {
		after := c.lastPublish.Add(50 * time.Millisecond)
		if first.After(after) {
			after = first
		}
		return !c.plc.structReadAfter(after).IsZero()
	}) {
		c.t.Fatalf("%s: no second read of %s in %v", label, w6Struct, slotWatch+slotSlack)
	}
}

// awaitSlotIdle runs the line until the strip carries the red
// counter_slot_idle item, within one watch interval of the move.
func (c *w6Cell) awaitSlotIdle(label string, moved time.Time) edgeengine.CatidHealthItem {
	c.t.Helper()
	var item edgeengine.CatidHealthItem
	if !c.runUntil(slotWatch+slotSlack-time.Since(moved), func() bool {
		for _, it := range c.slotItems() {
			if it.Kind == "counter_slot_idle" {
				item = it
				return true
			}
		}
		return false
	}) {
		c.dump(label + ": no slot item")
		c.t.Fatalf("%s: no counter_slot_idle item within one watch interval (%v) of the move to Prod_Counter_02; slot items %+v",
			label, slotWatch+slotSlack, c.slotItems())
	}
	return item
}

// click is the item's click: the counter edit to the item's tag, everything
// else as the process has it, as the route makes it. It checks the re-point
// (the process on _02, the item gone, both _02 members asked of WarLink, no
// confirm while WarLink does not publish CATID_02) and returns the part's
// observations before it.
func (c *w6Cell) click(label string, item edgeengine.CatidHealthItem) int64 {
	c.t.Helper()
	seen := c.observations(catRUN)
	p, err := c.edge.DB.GetProcess(c.processID)
	mustNil(c.t, err, "the process")
	tag, _ := item.Data["tag"].(string)
	mustNil(c.t, c.edge.Engine.EditProcessCounter(c.processID, domain.ProcessCounterEdit{PLCName: p.CounterPLCName, TagName: tag,
		Enabled: p.CounterEnabled, IdentityTagName: p.IdentityTagName, IdentityNone: p.IdentityNone}), "the click")
	if p, err = c.edge.DB.GetProcess(c.processID); err != nil || p.CounterTagName != w6Counter2 {
		c.t.Fatalf("%s: after the click the process counts %v (%v), want %s", label, p, err, w6Counter2)
	}
	c.noSlotItem(label + " after the click")
	for _, tag := range []string{w6Counter2, w6Identity2} {
		if !c.plc.isPublished(tag) {
			c.t.Fatalf("%s: the click did not ask WarLink to publish %s", label, tag)
		}
	}
	// WarLink publishes the two members at its next scan. Until then the
	// monitor, re-primed on CATID_02, has nothing to confirm: a confirm now
	// could only come from CATID_01, which is in the cache and names the
	// same part.
	time.Sleep(1500 * time.Millisecond)
	if n := c.observations(catRUN); n != seen {
		c.t.Fatalf("%s: the monitor confirmed %d before WarLink published %s (observations %d -> %d): it reads %s",
			label, catRUN, w6Identity2, seen, n, w6Identity)
	}
	return seen
}

// runUntil runs the line a machine cycle every w6Cadence until ok holds,
// asked every 50 ms between cycles, or the budget is spent; false then.
func (c *w6Cell) runUntil(budget time.Duration, ok func() bool) bool {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	next := time.Now().Add(w6Cadence)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		if !time.Now().Before(next) {
			c.cycle(1)
			next = next.Add(w6Cadence)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ok()
}

// slotItems are the strip's items on the MES struct's slots.
func (c *w6Cell) slotItems() []edgeengine.CatidHealthItem {
	c.t.Helper()
	items, err := c.edge.Engine.CatidHealth(c.processID)
	mustNil(c.t, err, "the strip")
	var out []edgeengine.CatidHealthItem
	for _, it := range items {
		if strings.HasPrefix(it.Kind, "counter_slot_") {
			out = append(out, it)
		}
	}
	return out
}

// noSlotItem fails if the strip carries an item on the struct's slots.
func (c *w6Cell) noSlotItem(label string) {
	c.t.Helper()
	if items := c.slotItems(); len(items) != 0 {
		c.t.Fatalf("%s: slot items %+v, want none", label, items)
	}
}

// countedRows is how many counted rows (delta > 0) the record has for the
// process, and the last one's id.
func (c *w6Cell) countedRows() (n, last int64) {
	c.t.Helper()
	mustNil(c.t, c.edge.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM counter_snapshots WHERE process_id = ? AND delta > 0`,
		c.processID).Scan(&n, &last), "counted rows")
	return n, last
}

// w6Row is one counted row of the record.
type w6Row struct {
	rp, count, delta, style int64
	catid                   string
}

// rowAfter is the process's first counted row after id.
func (c *w6Cell) rowAfter(id int64) w6Row {
	c.t.Helper()
	var r w6Row
	mustNil(c.t, c.edge.DB.QueryRow(`SELECT reporting_point_id, count_value, delta, COALESCE(style_id, 0), COALESCE(catid, '')
		FROM counter_snapshots WHERE process_id = ? AND delta > 0 AND id > ? ORDER BY id LIMIT 1`, c.processID, id).
		Scan(&r.rp, &r.count, &r.delta, &r.style, &r.catid), "the first counted row after "+fmt.Sprint(id))
	return r
}

// rowsAfter is the process's counted rows after id, and their parts.
func (c *w6Cell) rowsAfter(id int64) (parts int64, rows []w6Row) {
	c.t.Helper()
	q, err := c.edge.DB.Query(`SELECT reporting_point_id, count_value, delta, COALESCE(style_id, 0), COALESCE(catid, '')
		FROM counter_snapshots WHERE process_id = ? AND delta > 0 AND id > ? ORDER BY id`, c.processID, id)
	mustNil(c.t, err, "the counted rows after "+fmt.Sprint(id))
	defer q.Close()
	for q.Next() {
		var r w6Row
		mustNil(c.t, q.Scan(&r.rp, &r.count, &r.delta, &r.style, &r.catid), "a counted row")
		parts += r.delta
		rows = append(rows, r)
	}
	mustNil(c.t, q.Err(), "the counted rows")
	return parts, rows
}

// isPublished reports whether the Edge asked WarLink to publish tag.
func (f *w6WarLink) isPublished(tag string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[tag]
}
