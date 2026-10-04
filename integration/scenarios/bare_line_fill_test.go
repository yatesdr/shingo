// A line with no bin on it, filled end to end across the two modules by every
// button, in every swap mode.
//
// Every swap opens by lifting the line's bin, so on a bare line the answer is
// one plain order bringing a bin to it: a full of the part to a consume line,
// an empty to a produce line, and one more to each bare paired position of a
// press. This drives each button against the real Core engine and fleet
// simulator (the keep-staged recovery cell, without a staged spare) and carries
// what it made to the end: delivered, confirmed at Core and a bin on every
// position.
//
//go:build docker

package scenarios

import (
	"fmt"
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
)

func TestScenario_BareLine_EveryButtonFillsIt(t *testing.T) {
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
	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, d := range doors {
			t.Run(fmt.Sprintf("%s/%s", mode, d.name), func(t *testing.T) {
				c := newKsrCell(t, ksrOpts{role: d.role, mode: mode, plain: true})
				c.takeLineBinOff()
				c.settle()

				before := c.lastID()
				if err := d.press(c); err != nil {
					c.dump(d.name + " refused")
					t.Fatalf("%s on a bare line refused: %v", d.name, err)
				}
				c.tick()
				var toLine, toDeck []domain.Order
				for _, o := range c.newRows(before) {
					switch {
					case o.OrderType == protocol.OrderTypeComplex:
						c.dump(d.name)
						t.Fatalf("a swap leg %d on a bare line, want a plain delivery", o.ID)
					case o.DeliveryNode == ksrLine:
						toLine = append(toLine, o)
					case o.DeliveryNode == ksrDeck:
						toDeck = append(toDeck, o)
					}
				}
				wantDeck := 0
				if mode == protocol.SwapModeTwoRobotPressIndex {
					wantDeck = 1
				}
				if len(toLine) != 1 || len(toDeck) != wantDeck {
					c.dump(d.name)
					t.Fatalf("deliveries to the line %d and to the paired position %d, want 1 and %d",
						len(toLine), len(toDeck), wantDeck)
				}
				made := append(toLine, toDeck...)
				c.eventually(d.name+": every delivery landed and confirmed", func() bool {
					c.fleetStep()
					done := true
					for _, o := range made {
						if c.edgeRow(o.ID).Status == protocol.StatusDelivered {
							mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(o.ID, 1), "confirm")
						}
						if !protocol.IsTerminal(c.edgeRow(o.ID).Status) || !protocol.IsTerminal(c.coreOf(o).Status) {
							done = false
						}
					}
					return done
				})
				for _, o := range made {
					if s := c.coreOf(o).Status; s != protocol.StatusConfirmed {
						t.Errorf("delivery %d to %s ended %s at Core, want confirmed", o.ID, o.DeliveryNode, s)
					}
				}
				bins, err := c.core.eng.DB().ListBins()
				mustNil(t, err, "bins")
				filled := map[string]int{}
				for _, b := range bins {
					filled[c.nodeName(b.NodeID)]++
				}
				if filled[ksrLine] != 1 || filled[ksrDeck] != wantDeck {
					t.Errorf("bins on the line %d and on the paired position %d, want 1 and %d",
						filled[ksrLine], filled[ksrDeck], wantDeck)
				}
			})
		}
	}
}
