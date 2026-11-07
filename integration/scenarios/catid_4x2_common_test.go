// The common positions through a changeover (decision 41): the request guards
// refuse by position, so a position whose part both styles run is supplied
// while a changeover stands open.
//
//go:build docker

package scenarios

import (
	"errors"
	"strings"
	"testing"
	"time"

	edgeengine "shingoedge/engine"
)

// TestPinChangeover_ACommonPositionIsResuppliedWhileOpen: a changeover RUN ->
// ALT is open, its mirror legs landed, the PLC still on RUN's part. The
// common positions run to their level: the sweep asks a swap at each, under
// RUN's claim, and the request at a mirror position is still refused. The
// swaps land before the cutover, or after it (held at their waits while the
// program changes and the monitor forces the cutover onto the moved pointer);
// either way each binds, the counts continue on it, and every ledger is
// exact. Before, the request guard refused every RUN request while the
// changeover was open, and a common position that ran out got no cart.
func TestPinChangeover_ACommonPositionIsResuppliedWhileOpen(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before the cutover", "after the cutover"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()
			// Every claim's level at 30: ten counts bring a cart to it. The line
			// fills w6Depth loads deep before its first count.
			c := newW6Cell(t, w6Opts{reorder: 30})
			c.cycle(14)
			after := c.startChangeover(c.alt)
			c.carryAll("the changeover's legs", after, nil)
			for _, i := range []int{3, 4} {
				if tn := c.cart(i); tn == nil || tn.part != "MIRROR-R" {
					c.dump(when)
					t.Fatalf("%s holds %+v after the legs, want a MIRROR-R cart", w6Pos[i], tn)
				}
			}
			if c.open() == nil {
				t.Fatal("the changeover completed with the PLC still on RUN's part")
			}
			c.cycle(6) // the common carts reach their level, the changeover open; the MIRROR-R carts stay above it
			asked := c.sweep()
			at := map[string]string{}
			for _, o := range asked {
				if o.DeliveryNode != "" {
					at[o.DeliveryNode] = o.PayloadCode
				}
			}
			for i := 0; i < 3; i++ {
				if at[w6Pos[i]] != w6RunParts[i] {
					c.dump(when)
					t.Fatalf("the sweep with the changeover open asked %v, want %s's swap at %s", at, w6RunParts[i], w6Pos[i])
				}
			}
			for _, i := range []int{3, 4} {
				if _, ok := at[w6Pos[i]]; ok {
					t.Fatalf("the sweep asked at the mirror position %s: %v", w6Pos[i], at)
				}
			}
			var armed *edgeengine.ChangeoverArmedError
			if _, err := c.edge.Engine.RequestNodeMaterial(c.nodes[3], 1); !errors.As(err, &armed) {
				t.Fatalf("REQUEST at the mirror position %s with the changeover open: %v, want it refused", w6Pos[3], err)
			}
			first := asked[0].ID - 1
			commons := map[int]bool{0: true, 1: true, 2: true}
			if when == "before the cutover" {
				c.carryAll("the common swaps", first, nil)
				c.wantFreshCommons(when, 0)
				c.cycle(3)
				c.change(catALT)
				c.cycle(w6Depth + 1) // the run-out, then the program change
			} else {
				c.carryAll("the common swaps, held at their waits", first, commons)
				c.change(catALT)
				c.cycle(w6Depth + 1)
			}
			run := c.counted()
			c.cycle(w6Depth + 4) // ALT refills the line and its first parts are counted
			c.waitFor("the changeover completed by the monitor", declareWindow+10*time.Second, func() bool {
				return c.open() == nil
			})
			co := c.changeovers()[0]
			if co.ToStyleID != c.alt || c.active() != c.alt || !strings.HasPrefix(co.TriggeredBy, "auto-catid") {
				c.dump(when)
				t.Fatalf("changeover #%d %s by %q to %d, want completed by the monitor on ALT", co.ID, co.State, co.TriggeredBy,
					co.ToStyleID)
			}
			t.Logf("%s: changeover #%d completed by %s", when, co.ID, co.TriggeredBy)
			if when == "after the cutover" {
				counted := c.counted()
				c.carryAll("the common swaps, after the cutover", first, nil)
				c.wantFreshCommons(when, c.counted()-counted)
			}
			c.cycle(5)
			l := c.ledger("common "+when, true)
			c.wantRecord("common "+when, l, run, c.counted()-run)
			if l.holdsOpen != 0 {
				t.Fatalf("%s: %d held, want none", when, l.holdsOpen)
			}
			c.confirmedAll("common " + when)
			c.line("common "+when, l)
		})
	}
}

// wantFreshCommons fails unless each common position holds a cart that landed
// by its swap, bound at the Edge and at Core, drawn by drawn counts since.
func (c *w6Cell) wantFreshCommons(label string, drawn int64) {
	c.t.Helper()
	c.flush()
	for i := 0; i < 3; i++ {
		ts := c.tenures[i]
		tn := c.cart(i)
		if len(ts) < 2 || tn == nil || tn != ts[len(ts)-1] || tn.part != w6RunParts[i] {
			c.dump(label)
			c.t.Fatalf("%s: %s's carts %+v, want the swap's %s cart standing", label, w6Pos[i], ts, w6RunParts[i])
		}
		rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodes[i])
		mustNil(c.t, err, "runtime")
		if rt.ActiveBinID == nil || *rt.ActiveBinID != tn.bin || w6Full-c.count(tn) != drawn {
			c.dump(label)
			c.t.Fatalf("%s: %s binds %v and its cart %s (bin %d) drew %d, want it bound and drawn %d", label, w6Pos[i],
				rt.ActiveBinID, tn.label, tn.bin, w6Full-c.count(tn), drawn)
		}
		c.t.Logf("BOUND %s: %s took cart %s (bin %d), drawn %d", label, w6Pos[i], tn.label, tn.bin, drawn)
	}
}
