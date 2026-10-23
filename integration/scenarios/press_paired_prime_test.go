// A press whose head holds a bin and whose paired position is bare, asked for a
// bin by every button of both roles, carried end to end across the two modules.
//
// Every press swap indexes the paired position's bin onto the head, so with no
// bin there the index leg has nothing to lift yet. The answer is the same for
// both roles and every button, in one tap: a plain order bringing a bin to the
// bare paired position (a full of the part on a consume press, an empty on a
// produce press), created first, and the swap queued behind it. Core holds the
// swap's legs for the carrier coming (waiting_for_outside) and sends them when
// it lands; the swap then runs as any other.
//
//go:build docker

package scenarios

import (
	"strings"
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
)

func TestScenario_PressBarePairedPosition_EveryButtonPrimesItThenSwaps(t *testing.T) {
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
			_, err := c.edge.Engine.RequestProduceSwap(c.nodeID, 0)
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
			// position. Its claims reach Core's mirror as in the plant: the
			// hold reads the fed shape there (dispatch dependentSource).
			c := newKsrCell(t, ksrOpts{role: d.role, mode: protocol.SwapModeTwoRobotPressIndex, plain: true, mirror: true})
			// A count that a produce request will finalize.
			mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
			c.settle()

			var prime domain.Order
			c.swap(d.name, func() {
				before := c.lastID()
				if err := d.press(c); err != nil {
					c.dump(d.name + " refused")
					t.Fatalf("%s with the paired position bare refused: %v", d.name, err)
				}
				c.tick()
				prime = c.primeThenQueuedSwap(d.name, before)
			})

			c.eventually(d.name+": the prime confirmed", func() bool {
				if c.edgeRow(prime.ID).Status == protocol.StatusDelivered {
					mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(prime.ID, 1), "confirm")
				}
				return protocol.IsTerminal(c.edgeRow(prime.ID).Status) && protocol.IsTerminal(c.coreOf(prime).Status)
			})
			if s := c.coreOf(prime).Status; s != protocol.StatusConfirmed {
				t.Errorf("the prime %d ended %s at Core, want confirmed", prime.ID, s)
			}

			// The head's old bin went out, the primed bin was indexed onto the
			// head, and the swap refilled the paired position behind it.
			bins, err := c.core.eng.DB().ListBins()
			mustNil(t, err, "bins")
			filled := map[string]int{}
			for _, b := range bins {
				at := c.nodeName(b.NodeID)
				filled[at]++
				if b.Label == "KSR-LINE-BIN" && !strings.HasPrefix(at, ksrDest) {
					t.Errorf("the head's old bin is at %q, want the outbound %s", at, ksrDest)
				}
			}
			if filled[ksrLine] != 1 || filled[ksrDeck] != 1 {
				t.Errorf("bins on the line %d and on the paired position %d, want 1 and 1", filled[ksrLine], filled[ksrDeck])
			}
		})
	}
}

// primeThenQueuedSwap reads one tap's rows on a press with a bare paired
// position: one plain order to the paired position, nothing plain to the head,
// the two swap legs created after it (the outbox drains by id, so Core is asked
// for the prime first), and both legs held at Core for the carrier coming,
// neither with the fleet. Returns the prime.
func (c *ksrCell) primeThenQueuedSwap(label string, before int64) domain.Order {
	c.t.Helper()
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
	if len(toLine) != 0 || len(toDeck) != 1 || len(legs) != 2 {
		c.dump(label)
		c.t.Fatalf("%s: deliveries to the line %d, to the paired position %d, swap legs %d; want 0, 1 and 2",
			label, len(toLine), len(toDeck), len(legs))
	}
	prime := toDeck[0]
	for _, l := range legs {
		if l.ID < prime.ID {
			c.t.Errorf("%s: swap leg %d was created before the prime %d", label, l.ID, prime.ID)
		}
		co := c.coreOf(l)
		if co.VendorOrderID != "" {
			c.dump(label)
			c.t.Fatalf("%s: swap leg %d went to the fleet with the paired position bare", label, l.ID)
		}
		if co.QueueCode != string(protocol.QueueWaitingForOutside) {
			c.t.Errorf("%s: swap leg %d is held as %q (%s: %q) at Core, want %q", label, l.ID, co.QueueCode,
				co.QueueCause, co.QueueReason, protocol.QueueWaitingForOutside)
		}
	}
	return prime
}
