//go:build docker

package scenarios

import (
	"strings"
	"testing"

	"shingo/protocol"
	corebins "shingocore/store/bins"
)

// A CHANGEOVER CANCELLED WHILE ITS OWN FETCH DIGS. In the two staging modes the
// changeover's supply fetches B itself, and with B buried in its lane that leg
// is its own dig (Core re-parents the demand and runs the excavation as its
// children). Cancelled with the first dig leg holding the blocker at the lane,
// the ruled behaviour (cancel-return, 2026-10-02/03) is: the carrying leg keeps
// going and stores the blocker in the group, the rest of the dig stands down,
// nothing is left on a robot's forks and nothing is stranded in the lane.
// Consume only: a produce leg's Empty pickup does not dig (it skips the finder).
//
// The lane reads unheld during this dig, unlike a plain refill's: the demand's
// own dig takes the soft hold on the bin it digs for one pass early, so its
// lane row need not outlive the dig (digLaneTake.inTx, handOffDugLane). That is
// the design, so the moment does not require the lane held.
func TestScenario_KeepStagedChangeover_CancelledWhileItsOwnFetchDigs(t *testing.T) {
	for _, mode := range []protocol.SwapMode{protocol.SwapModeTwoRobot, protocol.SwapModeSingleRobot} {
		for _, claimC := range []bool{true, false} {
			name := string(mode) + "/blocker-part-claimed"
			if !claimC {
				name = string(mode) + "/blocker-part-unclaimed"
			}
			t.Run(name, func(t *testing.T) {
				c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: mode, buriedB: true, claimC: claimC})
				co := c.startChangeover()
				ksrDigMomentOf(c, co, false)
				c.dump("at the cancel: the changeover's own fetch digging")
				mustNil(t, c.edge.Engine.CancelProcessChangeover(c.processID), "cancel")
				c.caseEStore()
				c.quiet()
				c.dump("after the cancel")

				db := c.core.eng.DB()
				bins, err := db.ListBins()
				mustNil(t, err, "bins")
				for _, b := range bins {
					at := c.nodeName(b.NodeID)
					if at == "_TRANSIT" || strings.HasPrefix(at, corebins.CarrierNodePrefix) {
						t.Errorf("bin %s is left at %s after the cancel: on a robot's forks", b.Label, at)
					}
					if b.ClaimedBy != nil {
						t.Errorf("bin %s at %s is still claimed by order %d", b.Label, at, *b.ClaimedBy)
					}
				}
				if c.core.eng.Dispatcher().LaneLock().IsLocked(c.laneB.ID) {
					t.Errorf("B's lane is still held after the dig stood down")
				}
				for _, o := range c.rows() {
					co, err := db.GetOrderByUUID(o.UUID)
					if err != nil || !protocol.IsTerminal(co.Status) {
						continue
					}
					res, err := db.ListReservationsByOrder(co.ID)
					mustNil(t, err, "reservations")
					for _, r := range res {
						t.Errorf("%s order %d still holds a %v reservation on bin %d / node %q",
							co.Status, co.ID, r.Kind, r.BinID, c.nodeName(&r.NodeID))
					}
				}
			})
		}
	}
}
