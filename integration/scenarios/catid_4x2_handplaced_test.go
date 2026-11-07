// S9, the hand-placed cart: the mirror positions hold carts whose seats could
// not say their part, and the PLC jumps to ALT's part with no changeover.
//
//go:build docker

package scenarios

import (
	"fmt"
	"testing"
	"time"

	"shingoedge/domain"
)

// TestCatid4x2_S9_HandPlacedCart: RUN runs on carts whose seats at the two
// mirror positions could not say their part (a cart placed by hand, a seat
// from before the known bit: the Edge has the cart bound and Core knows it as
// MIRROR-L, the Edge does not), and draws them as RUN's part. The PLC is
// forced onto ALT's part: the off-plan ALT counts are held at the mirror
// positions (the holds ledger) and charged at the common ones, and the
// automatic switch fires. The switch records what it leaves: the mirror seats
// read MIRROR-L, source flip, so its release lands nothing there (the holds
// stay open), every ALT stroke at a mirror position is held at the seat under
// MIRROR-R with the MIRROR-L cart's count standing from the switch on, the
// common positions charge, and the stranded notice names each mirror
// position. Core refuses nothing (no payload_mismatch_dropped row). Then a
// MIRROR-R cart lands at each mirror position: the next stroke replays the
// seat's hold onto it, a person's Charge releases the holds ledger's share onto
// it, and the ledger is exact. Before, the switch read the unsaid carts as
// ALT's part: its release charged the MIRROR-L carts as MIRROR-R, and every
// later stroke there went out as MIRROR-R and Core refused it.
func TestCatid4x2_S9_HandPlacedCart(t *testing.T) {
	t.Parallel()
	c := newW6Cell(t, w6Opts{})
	mirror := []int{3, 4}
	for _, i := range mirror {
		mustNil(t, c.edge.DB.SetProcessNodeRuntimeLinesidePayload(c.nodes[i], "", false, ""),
			"the seat at "+w6Pos[i]+" cannot say its cart's part")
	}
	c.cycle(14)
	l := c.ledger("S9 RUN on the unsaid carts", true)
	c.line("S9 RUN on the unsaid carts", l)
	run := c.counted()
	c.force(catALT)
	c.cycle(6) // ALT's first counts, off-plan: the mirror shares held, the common positions charged
	offPlan := c.counted() - run
	if h := c.holds(); len(h.rows) != 2 || h.open != 2*offPlan {
		t.Fatalf("after the jump: holds %+v, want one open MIRROR-R share of %d at each mirror position", h.rows, offPlan)
	}

	c.waitFor("the automatic switch to ALT", autoSwitchWindow+10*time.Second, func() bool { return c.active() == c.alt })
	c.afterTheDoor() // the switch flips first; its row and its release follow under the door
	if co := c.changeovers()[0]; co.TriggeredBy != "auto-catid-flip" || co.ToStyleID != c.alt {
		t.Fatalf("the switch's row: #%d by %q to %d, want auto-catid-flip to ALT", co.ID, co.TriggeredBy, co.ToStyleID)
	}
	c.flush()
	for _, i := range mirror {
		rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
		mustNil(t, err, "runtime")
		if !rt.LinesidePayloadKnown || rt.LinesidePayloadCode != "MIRROR-L" || rt.LinesideSource != string(domain.CarrierFromFlip) {
			t.Errorf("after the switch the seat at %s reads %q known %t from %q, want MIRROR-L from the flip", w6Pos[i],
				rt.LinesidePayloadCode, rt.LinesidePayloadKnown, rt.LinesideSource)
		}
	}
	h := c.holds()
	if h.open != 2*offPlan {
		t.Errorf("after the switch: %d held open, want the mirror shares (%d) still open: %+v", h.open, 2*offPlan, h.rows)
	}
	switched := map[int]int64{}
	for _, i := range mirror {
		switched[i] = c.count(c.cart(i))
	}
	common := c.drawnNow()

	// ALT runs on the MIRROR-L carts; the parked-ticks monitor scans once a
	// cycle, so the seat's hold rises at each scan.
	alt := c.counted()
	for k := 0; k < 6; k++ {
		c.cycle(1)
		c.tickers.stranded.pass(t, "the parked-ticks monitor")
	}
	since := c.counted() - alt
	c.flush()
	for _, i := range mirror {
		tn := c.cart(i)
		rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
		mustNil(t, err, "runtime")
		if got := c.count(tn); got != switched[i] || rt.PendingUOPDelta != since || rt.PendingUOPPayload != "MIRROR-R" {
			t.Errorf("%s after %d ALT counts: cart %s reads %d (at the switch %d), the seat holds %d of %q; want the cart "+
				"standing and the seat holding %d of MIRROR-R", w6Pos[i], since, tn.label, got, switched[i],
				rt.PendingUOPDelta, rt.PendingUOPPayload, since)
		}
		c.t.Logf("HELD S9 after the switch: %s holds %d of MIRROR-R beside cart %s, which stands at %d from the switch",
			w6Pos[i], rt.PendingUOPDelta, tn.label, c.count(tn))
		want := fmt.Sprintf("Strokes held at %s — the cart there is recorded as MIRROR-L (from the switch off RUN); ALT draws "+
			"MIRROR-R here. %d held for the next MIRROR-R cart: land one, or name this cart with Load Payload.", w6Pos[i], since)
		if got := c.edge.Engine.StrandedAlarmDetail(w6Pos[i]); got != want {
			t.Errorf("the stranded notice at %s = %q, want %q", w6Pos[i], got, want)
		} else {
			c.t.Logf("NOTICE S9 %s: %s", w6Pos[i], got)
		}
	}
	now := c.drawnNow()
	for i := 0; i < 3; i++ {
		if now[i]-common[i] != since {
			t.Errorf("the common position %s drew %d over %d ALT counts, want them all", w6Pos[i], now[i]-common[i], since)
		}
	}
	for _, r := range h.rows {
		if r.ResolvedAt != nil {
			continue
		}
		pv, err := c.edge.Engine.PreviewCatidHold(r.ID)
		mustNil(t, err, "the hold's preview")
		want := fmt.Sprintf("the cart at %s is recorded as MIRROR-L (from the switch off RUN), not ALT's part", w6Pos[c.posOf(r.ProcessNodeID)])
		if pv.Refusal != want {
			t.Errorf("hold %d's preview refuses %q, want %q", r.ID, pv.Refusal, want)
		}
	}
	if n := c.refusedAtCore(); n != 0 {
		t.Errorf("Core's ledger has %d payload_mismatch_dropped row(s) for the run, want none", n)
	}
	l = c.ledger("S9 after the switch", true)
	c.line("S9 after the switch", l)

	// A MIRROR-R cart at each mirror position: the operator's REQUEST there.
	before := c.lastID()
	for _, i := range mirror {
		if _, err := c.edge.Engine.RequestNodeMaterial(c.nodes[i], 1); err != nil {
			c.dump("REQUEST")
			t.Fatalf("REQUEST at %s: %v", w6Pos[i], err)
		}
	}
	c.tick()
	c.carryAll("the MIRROR-R carts", before, nil)
	for _, i := range mirror {
		if tn := c.cart(i); tn == nil || tn.part != "MIRROR-R" {
			c.dump("the MIRROR-R carts")
			t.Fatalf("%s holds %+v after its REQUEST, want a MIRROR-R cart", w6Pos[i], tn)
		}
	}
	landed := c.counted()
	c.cycle(3) // the first count replays the seat's hold onto the MIRROR-R cart
	c.flush()
	own := c.counted() - landed
	for _, i := range mirror {
		tn := c.cart(i)
		rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
		mustNil(t, err, "runtime")
		if drew := w6Full - c.count(tn); drew != since+own || rt.PendingUOPDelta != 0 {
			t.Errorf("%s: cart %s drew %d, the seat holds %d; want the %d held and its own %d, the seat empty", w6Pos[i],
				tn.label, drew, rt.PendingUOPDelta, since, own)
		}
		c.t.Logf("HELD S9 after the landing: %s's cart %s drew %d (the %d held at the seat and %d of its own)", w6Pos[i],
			tn.label, w6Full-c.count(tn), since, own)
	}
	for _, r := range c.holds().rows {
		if r.ResolvedAt == nil {
			c.chargeHold(r.ID)
		}
	}
	if n := c.refusedAtCore(); n != 0 {
		t.Errorf("Core's ledger has %d payload_mismatch_dropped row(s) for the run, want none", n)
	}
	l = c.ledger("S9", true)
	c.wantRecord("S9", l, run, c.counted()-run)
	if l.holdsOpen != 0 {
		t.Errorf("S9: %d held open at the end, want none", l.holdsOpen)
	}
	c.confirmedAll("S9")
	c.line("S9", l)
}

// refusedAtCore is how many deltas Core refused as another part's: its
// ledger's payload_mismatch_dropped rows (the database is the run's own).
func (c *w6Cell) refusedAtCore() int64 {
	c.t.Helper()
	c.flush()
	var n int64
	mustNil(c.t, c.core.eng.DB().QueryRow(`SELECT COUNT(*) FROM bin_uop_ledger WHERE op = $1`,
		"payload_mismatch_dropped").Scan(&n), "Core's refused deltas")
	return n
}
