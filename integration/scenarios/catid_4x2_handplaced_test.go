// S9, the hand-placed cart: the mirror positions hold carts whose seats could
// not say their part, and the PLC jumps to ALT's part with no changeover.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"
)

// TestCatid4x2_S9_HandPlacedCart: RUN runs on carts whose seats at the two
// mirror positions could not say their part (a cart placed by hand, a seat
// from before the known bit: the Edge has the cart bound and Core knows it as
// MIRROR-L, the Edge does not), and draws them as RUN's part. The PLC is
// forced onto ALT's part with no changeover open: the off-plan ALT counts are
// held at the mirror positions (the holds ledger) and charged at the common
// ones, and nothing moves — no switch, no changeover row, no order — while the
// wrong-part item, red, offers the changeover to ALT. Untapped, the seats stay
// as they were, the holds grow and nothing is ordered. Tapped, the changeover
// swaps the hand-placed carts for MIRROR-R carts, the monitor completes it, the
// completion lands the held shares on them, and the ledger is exact with no
// Charge. Core refuses nothing in either half (no payload_mismatch_dropped row).
func TestCatid4x2_S9_HandPlacedCart(t *testing.T) {
	t.Parallel()
	for _, half := range []string{"untapped", "tapped"} {
		t.Run(half, func(t *testing.T) {
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
			before := c.lastID()
			c.force(catALT)
			forced := time.Now()
			c.cycle(6) // ALT's first counts, off-plan: the mirror shares held, the common positions charged
			c.wantMirrorShares("S9 the jump", c.counted()-run)
			item := c.asksAndMovesNothing("S9 "+half, forced, before)
			if half == "untapped" {
				c.untapped("S9", run, before)
				for _, i := range mirror {
					rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
					mustNil(t, err, "runtime")
					if rt.LinesidePayloadKnown {
						t.Errorf("untapped, the seat at %s reads %q from %q, want it as it was: nothing flipped",
							w6Pos[i], rt.LinesidePayloadCode, rt.LinesideSource)
					}
				}
			} else {
				c.tapped("S9", item, run)
			}
			if n := c.refusedAtCore(); n != 0 {
				t.Errorf("Core's ledger has %d payload_mismatch_dropped row(s) for the run, want none", n)
			}
		})
	}
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
