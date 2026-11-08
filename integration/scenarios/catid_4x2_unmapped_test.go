// S5, an unmapped part: a CATID no style pins.
//
//go:build docker

package scenarios

import (
	"fmt"
	"testing"
	"time"

	edgeengine "shingoedge/engine"
)

// TestCatid4x2_S5_UnmappedPart: during a changeover RUN -> ALT the program
// changes to 1003, which no style pins. Every count's draw is held, whole, on
// the cell counter's node-0 row; no cart is drawn. Once the changeover has
// completed, the strip offers to pin 1003 to ALT on that changeover's
// evidence; pinned, the hold's Charge (its preview first) lands on ALT's carts.
// A later unmapped 1004 is written off: resolved, nothing drawn.
func TestCatid4x2_S5_UnmappedPart(t *testing.T) {
	t.Parallel()
	c := newW6Cell(t, w6Opts{})
	c.cycle(12)
	after := c.startChangeover(c.alt)
	c.change(1003)
	c.carryAll("the changeover's legs", after, nil)
	c.cycle(w6Depth + 1) // the run-out, then the change to 1003
	before := c.counted()
	c.flush()
	drawn := c.drawnNow()
	c.cycle(w6Depth + 4) // 1003 refills the line, then four counts
	held := c.counted() - before
	h := c.shares()
	if len(h.rows) != 1 || h.rows[0].ProcessNodeID != 0 || h.rows[0].CATID != "1003" || h.open != held {
		t.Fatalf("1003's counts: holds %+v, want one open node-0 row of %d", h.rows, held)
	}
	c.flush()
	if now := c.drawnNow(); now != drawn {
		t.Fatalf("1003's counts drew the carts: %v -> %v", drawn, now)
	}

	mustNil(t, c.edge.Engine.CompleteProcessProductionCutover(c.processID), "CUTOVER")
	co := c.changeovers()[0]
	run := c.counted()
	var suggestion map[string]any
	c.waitFor("the strip's suggestion for 1003", 2*time.Minute+20*time.Second, func() bool {
		items, err := c.edge.Engine.CatidHealth(c.processID)
		mustNil(t, err, "the strip")
		for _, it := range items {
			if s, ok := it.Data["suggestion"].(map[string]any); ok && it.Kind == "unmapped_catid" && it.Data["catid"] == "1003" {
				suggestion = s
				return true
			}
		}
		return false
	})
	if fmt.Sprint(suggestion["style_id"]) != fmt.Sprint(c.alt) || fmt.Sprint(suggestion["changeover_id"]) != fmt.Sprint(co.ID) {
		t.Fatalf("the suggestion %v, want ALT (%d) on changeover #%d", suggestion, c.alt, co.ID)
	}
	c.pin(c.alt, "1003", fmt.Sprintf("changeover #%d to ALT ended on 1003", co.ID))
	pv := c.chargeHold(h.rows[0].ID)
	c.wantCharged(pv, drawn, held)
	l := c.ledger("S5 charged", true)
	c.wantRecord("S5 charged", l, run, 0)

	c.force(1004)
	before = c.counted()
	c.cycle(3)
	held = c.counted() - before
	h = c.shares()
	if open := h.open; open != held {
		t.Fatalf("1004's counts: %d held, want %d: %+v", open, held, h.rows)
	}
	c.writeOffHold(h.rows[len(h.rows)-1].ID)
	if h := c.shares(); h.open != 0 || h.writ != held {
		t.Fatalf("after Write off: %d open, %d written off; want 0 and %d: %+v", h.open, h.writ, held, h.rows)
	}
	l = c.ledger("S5", false)
	c.wantRecord("S5", l, run, c.counted()-run)
	c.confirmedAll("S5")
	c.line("S5", l)
}

// drawnNow is each position's standing cart's draw.
func (c *w6Cell) drawnNow() [5]int64 {
	var d [5]int64
	for i := range w6Pos {
		d[i] = w6Full - c.count(c.cart(i))
	}
	return d
}

// wantCharged fails unless the Charge drew the held count from every
// position's standing cart, as its preview said.
func (c *w6Cell) wantCharged(pv *edgeengine.CatidHoldPreview, before [5]int64, qty int64) {
	c.t.Helper()
	if len(pv.Rows) != len(w6Pos) {
		c.t.Fatalf("the preview names %d rows, want the five positions: %+v", len(pv.Rows), pv.Rows)
	}
	now := c.drawnNow()
	for i := range w6Pos {
		if now[i]-before[i] != qty {
			c.t.Fatalf("the Charge drew %d from %s's cart %s, want %d (preview %+v)", now[i]-before[i], w6Pos[i],
				c.cart(i).label, qty, pv.Rows)
		}
	}
}

// The hold verbs and the pin:
//
// The strip's buttons, each behind one helper: the trim folds the preview into
// the Charge and the pin's one-click into the pin check, and a rename is a
// change here.

// chargeHold is the strip's Charge, its preview first: it returns the preview
// the person saw.
func (c *w6Cell) chargeHold(id int64) *edgeengine.CatidHoldPreview {
	c.t.Helper()
	pv, err := c.edge.Engine.PreviewCatidHold(id)
	mustNil(c.t, err, "the hold's preview")
	if pv.Refusal != "" {
		c.t.Fatalf("hold %d: the preview refuses the Charge: %s", id, pv.Refusal)
	}
	mustNil(c.t, c.edge.Engine.ChargeCatidHold(id, "w6-engineer"), "Charge")
	c.flush()
	return pv
}

// writeOffHold is the strip's Write off.
func (c *w6Cell) writeOffHold(id int64) {
	c.t.Helper()
	mustNil(c.t, c.edge.Engine.WriteOffCatidHold(id, "w6-engineer"), "Write off")
	c.flush()
}

// pin is the strip's one click on a suggestion: v joins the style's pin.
func (c *w6Cell) pin(style int64, v string, evidence string) {
	c.t.Helper()
	mustNil(c.t, c.edge.Engine.AddStyleCATID(c.processID, style, v, "w6-engineer", evidence), "pin "+v)
}
