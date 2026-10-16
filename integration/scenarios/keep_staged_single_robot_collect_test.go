//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"
)

// THE SINGLE-ROBOT COLLECT TAKES WHAT ITS OWN ORDER PARKED, NEVER A STRAY. A
// single-robot changeover onto a keep-staged spot that is the inbound staging,
// with a wrong spare standing there, is the market swap: one order fetches B,
// parks it on the spot, swaps the line and collects B again by relay. Until
// 2026-10-16 the park and the collect were two orders, and the collect, another
// order's pickup, named B's part: Core reserved for it a stray unstamped empty
// of a carrier B may ride, standing on the spot, the moment the swap was
// dispatched. That is the 08:31 shape (SPR ALN_011, 2026-10-15), where the leg
// lifted the very bin the start had judged wrong and sent back.
func TestScenario_KeepStagedSingleRobot_TheCollectNeverTakesAStray(t *testing.T) {
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot, strayEmptyB: true})
	coreDB := c.core.eng.DB()
	bins, err := coreDB.ListBins()
	mustNil(t, err, "bins")
	var stray int64
	for _, b := range bins {
		if b.Label == "KSR-STRAY" {
			stray = b.ID
		}
	}
	co := c.startChangeover()
	c.tick()
	c.dump("after the start")
	if len(co.legs) != 1 {
		t.Fatalf("legs=%d, want the single robot's one order", len(co.legs))
	}
	var parked bool
	for _, st := range c.legSteps(co.legs[0]) {
		if st.Node != ksrSpot {
			continue
		}
		if st.Action == protocol.ActionDropoff {
			parked = true
		}
		if st.Action == protocol.ActionPickup && (!parked || st.PayloadCode != "") {
			t.Fatalf("the collect at the spot is %+v: it must be the relay of what the order parked", st)
		}
	}
	for _, l := range co.legs {
		o := c.coreOf(l)
		if o.BinID != nil && *o.BinID == stray {
			t.Fatalf("leg %d (%s) holds the stray empty %d: it would carry it to the line", l.ID, o.Status, stray)
		}
		res, err := coreDB.ListReservationsByOrder(o.ID)
		mustNil(t, err, "reservations")
		for _, r := range res {
			if r.BinID == stray {
				t.Fatalf("leg %d reserves the stray empty %d (%v): it would carry it to the line", l.ID, stray, r.Kind)
			}
		}
	}
	// The stray goes back on the start's return, and the order parks B.
	c.spareLeaves(co)
	c.landOnSpot(c.bFlying(co))
	if !c.bOnSpot() {
		t.Fatal("the order set B down but no B bin is on the spot")
	}
}
