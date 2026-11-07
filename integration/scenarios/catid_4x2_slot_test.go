// S8, the counted slot stops and a sibling moves: the slot watch's item, the
// click that re-points the counter, and the record's gap.
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
	// the scenario's read of the strip); clickSlack is the click, the hold
	// before WarLink publishes the new slot, and the scan that publishes it.
	slotSlack  = 2 * time.Second
	clickSlack = 5 * time.Second
)

// TestCatid4x2_S8_CountedSlotStopsASiblingMoves: WELD-6 counts on
// Prod_Counter_01, the five positions at the load station, the line running a
// cycle every 10 s. Right after a pass of the Edge's slot watch the PLC's
// program moves the count to Prod_Counter_02: it ticks from then and CATID_02
// reads the part; Prod_Counter_01 stops, CATID_01 stays. Within one watch
// interval the strip carries the red counter_slot_idle item naming
// Prod_Counter_02, and its click (the counter edit POST
// /api/processes/{id}/counter makes) re-points the counter: the item goes,
// the identity follows to CATID_02 (nothing is confirmed while WarLink does
// not publish it, though CATID_01 names the same part), the point's first
// read of Prod_Counter_02 seeds, and the first tick on it is recorded under
// CATID_02's part and RUN. The record lacks exactly the parts counted on _02
// before that first read, a gap of one watch interval and the click; the
// carts, counted at the station, lack nothing, and every order is confirmed.
func TestCatid4x2_S8_CountedSlotStopsASiblingMoves(t *testing.T) {
	t.Parallel()
	c := newW6Cell(t, w6Opts{station: []int{0, 1, 2, 3, 4}})
	c.unrecorded = map[int64]int64{}
	c.cycle(w6Depth + 2) // the line full: the counter counts
	c.ledger("S8 steady", true)
	c.noSlotItem("S8 steady")

	// The line runs until a pass of the watch has read the struct after the
	// line's last count reached the cache: Prod_Counter_01 stands from that
	// read to the move. (Before the watch existed nothing reads the struct:
	// the move comes after the budget instead.)
	if !c.runUntil(slotWatch+slotSlack, func() bool {
		return !c.plc.structReadAfter(c.lastPublish.Add(50 * time.Millisecond)).IsZero()
	}) {
		t.Logf("S8: no read of %s in %v; the count moves anyway", w6Struct, slotWatch+slotSlack)
	}
	c.noSlotItem("S8 the watch's pass before the move")
	recorded, _ := c.countedRows()
	c.plc.do(func(l *simwarlink.Line) { mustNil(t, l.CountOn(2), "the count to slot 2") })
	moved := time.Now()
	stood := c.plc.state().Slots[0]
	c.cycle(1) // the first part counted on Prod_Counter_02, which no point reads

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
		c.dump("S8: no slot item")
		t.Fatalf("S8: no counter_slot_idle item within one watch interval (%v) of the move to Prod_Counter_02; slot items %+v",
			slotWatch+slotSlack, c.slotItems())
	}
	shown := time.Since(moved)
	if item.Severity != edgeengine.CatidHealthRed || item.Data["tag"] != w6Counter2 || item.Data["catid"] != fmt.Sprint(catRUN) ||
		item.Data["counted_tag"] != w6Counter {
		t.Fatalf("S8: the item %+v; want red, naming %s (CATID %d) against the counted %s", item, w6Counter2, catRUN, w6Counter)
	}
	t.Logf("S8: %.1fs after the move: %q", shown.Seconds(), item.Headline)

	// The click: the counter edit to the item's tag, everything else as the
	// process has it, as the route makes it.
	seen := c.observations(catRUN)
	p, err := c.edge.DB.GetProcess(c.processID)
	mustNil(t, err, "the process")
	tag, _ := item.Data["tag"].(string)
	mustNil(t, c.edge.Engine.EditProcessCounter(c.processID, domain.ProcessCounterEdit{PLCName: p.CounterPLCName, TagName: tag,
		Enabled: p.CounterEnabled, IdentityTagName: p.IdentityTagName, IdentityNone: p.IdentityNone}), "the click")
	if p, err = c.edge.DB.GetProcess(c.processID); err != nil || p.CounterTagName != w6Counter2 {
		t.Fatalf("S8: after the click the process counts %v (%v), want %s", p, err, w6Counter2)
	}
	if rp, err := c.edge.DB.GetReportingPointByTag(w6PLC, w6Counter2); err != nil || rp == nil || !rp.Enabled || rp.LastPollAt != nil {
		t.Fatalf("S8: the reporting point on %s after the click: %+v (%v); want it enabled and unread", w6Counter2, rp, err)
	}
	c.noSlotItem("S8 after the click")
	for _, tag := range []string{w6Counter2, w6Identity2} {
		if !c.plc.isPublished(tag) {
			t.Fatalf("S8: the click did not ask WarLink to publish %s", tag)
		}
	}
	// WarLink publishes the two members at its next scan. Until then the
	// monitor, re-primed on CATID_02, has nothing to confirm: a confirm now
	// could only come from CATID_01, which is in the cache and names the
	// same part.
	time.Sleep(1500 * time.Millisecond)
	if n := c.observations(catRUN); n != seen {
		t.Fatalf("S8: the monitor confirmed %d before WarLink published %s (observations %d -> %d): it reads %s",
			catRUN, w6Identity2, seen, n, w6Identity)
	}

	// WarLink's scan, and the point's first read of Prod_Counter_02: it seeds.
	c.publish()
	c.tickers.poll.pass(t, "the poll")
	seeded := time.Now()
	gapParts := c.plc.state().Slots[1].Counter
	rp, err := c.edge.DB.GetReportingPointByTag(w6PLC, w6Counter2)
	if err != nil || rp == nil || rp.LastPollAt == nil || rp.LastCount != gapParts {
		t.Fatalf("S8: the point's first read of %s: %+v (%v); want it seeded at %d", w6Counter2, rp, err, gapParts)
	}
	c.unrecorded[catRUN] += gapParts
	if n, _ := c.countedRows(); n != recorded {
		t.Fatalf("S8: %d counted rows recorded between the move and the point's first read of %s, want none", n-recorded, w6Counter2)
	}
	gap := seeded.Sub(moved)
	if gap > slotWatch+clickSlack {
		t.Fatalf("S8: the record's gap is %.1fs (%d parts counted on %s before the point read it); want at most the watch interval %v and the click %v",
			gap.Seconds(), gapParts, w6Counter2, slotWatch, clickSlack)
	}
	c.awaitConfirm("CATID_02 after the re-point", catRUN, seen)

	// The first tick on Prod_Counter_02.
	_, last := c.countedRows()
	c.cycle(1)
	tick := c.rowAfter(last)
	want := c.plc.state().Slots[1]
	if tick.rp != rp.ID || tick.count != want.Counter || tick.delta != 1 || tick.catid != fmt.Sprint(want.CATID) || tick.style != c.run {
		t.Fatalf("S8: the first tick on %s: %+v; want point %d reading %d, one part, CATID_02's %d, RUN (%d)",
			w6Counter2, tick, rp.ID, want.Counter, want.CATID, c.run)
	}

	c.cycle(4)
	l := c.ledger("S8", true)
	c.wantRecord("S8", l, c.counted()-gapParts, 0)
	if l.holdsOpen != 0 {
		t.Fatalf("S8: %d held, want none", l.holdsOpen)
	}
	if s := c.plc.state().Slots[0]; s != stood {
		t.Fatalf("S8: Prod_Counter_01 / CATID_01 moved after the count left them: %+v -> %+v", stood, s)
	}
	c.confirmedAll("S8")
	c.line("S8", l)
	t.Logf("GAP S8: the item %.1fs after the move (watch interval %v); the record lacks the %d parts counted on %s in the %.1fs to the point's first read of it",
		shown.Seconds(), slotWatch, gapParts, w6Counter2, gap.Seconds())
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

// isPublished reports whether the Edge asked WarLink to publish tag.
func (f *w6WarLink) isPublished(tag string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[tag]
}
