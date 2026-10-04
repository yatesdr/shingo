// A press whose head holds a bin and whose paired position is bare, asked for a
// bin by every button of both roles, carried end to end across the two modules.
//
// Every press swap indexes the paired position's bin onto the head, so with no
// bin there the index leg has nothing to lift. The answer is the same for both
// roles: one plain order bringing a bin to the bare paired position, and no
// swap until it is there. A full of the part on a consume press, an empty on a
// produce press.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
)

func TestScenario_PressBarePairedPosition_EveryButtonPrimesIt(t *testing.T) {
	type door struct {
		name  string
		role  protocol.ClaimRole
		press func(c *ksrCell) error
	}
	doors := []door{
		{"material request", protocol.ClaimRoleConsume, func(c *ksrCell) error {
			_, err := c.edge.Engine.RequestNodeMaterial(c.nodeID, 1)
			return err
		}},
		{"produce request", protocol.ClaimRoleProduce, func(c *ksrCell) error {
			_, err := c.edge.Engine.RequestProduceSwap(c.nodeID)
			return err
		}},
		{"empty-bin request", protocol.ClaimRoleProduce, func(c *ksrCell) error {
			_, err := c.edge.Engine.RequestEmptyBin(c.nodeID, ksrPartA)
			return err
		}},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			// The plain press cell: a bin on the head, none on the paired
			// position.
			c := newKsrCell(t, ksrOpts{role: d.role, mode: protocol.SwapModeTwoRobotPressIndex, plain: true})
			// A count that a produce request will finalize.
			mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
			c.settle()

			before := c.lastID()
			if err := d.press(c); err != nil {
				c.dump(d.name + " refused")
				t.Fatalf("%s with the paired position bare refused: %v", d.name, err)
			}
			c.tick()
			var legs, toLine, toDeck []domain.Order
			for _, o := range c.newRows(before) {
				switch {
				case o.OrderType == protocol.OrderTypeComplex:
					legs = append(legs, o)
				case o.DeliveryNode == ksrLine:
					toLine = append(toLine, o)
				case o.DeliveryNode == ksrDeck:
					toDeck = append(toDeck, o)
				}
			}
			if len(legs) > 0 {
				// What the swap does with nothing on the paired position: let the
				// world run and show where each leg stands at Core.
				c.quiet()
				c.dump(d.name + ": a swap with the paired position bare")
				t.Fatalf("%d swap legs with the paired position bare, want one delivery to it", len(legs))
			}
			if len(toLine) != 0 || len(toDeck) != 1 {
				c.dump(d.name)
				t.Fatalf("deliveries to the line %d and to the paired position %d, want 0 and 1", len(toLine), len(toDeck))
			}
			p := toDeck[0]
			c.eventually(d.name+": the delivery to the paired position landed and confirmed", func() bool {
				c.fleetStep()
				if c.edgeRow(p.ID).Status == protocol.StatusDelivered {
					mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(p.ID, 1), "confirm")
				}
				return protocol.IsTerminal(c.edgeRow(p.ID).Status) && protocol.IsTerminal(c.coreOf(p).Status)
			})
			if s := c.coreOf(p).Status; s != protocol.StatusConfirmed {
				t.Errorf("delivery %d ended %s at Core, want confirmed", p.ID, s)
			}
			bins, err := c.core.eng.DB().ListBins()
			mustNil(t, err, "bins")
			filled := map[string]int{}
			for _, b := range bins {
				filled[c.nodeName(b.NodeID)]++
			}
			if filled[ksrLine] != 1 || filled[ksrDeck] != 1 {
				t.Errorf("bins on the line %d and on the paired position %d, want 1 and 1", filled[ksrLine], filled[ksrDeck])
			}
		})
	}
}
