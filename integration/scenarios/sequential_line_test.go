// A sequential line, carried end to end across the two modules: its removal
// lifts the line's bin, its backfill brings the next one, and the line's next
// request starts the next cycle.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
)

// seqDoors are the request buttons of a role: the material request on a
// consume line, the produce request and the empty-bin request on a produce
// line.
var seqDoors = []struct {
	name  string
	role  protocol.ClaimRole
	press func(c *ksrCell) error
}{
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

// seqRemoval presses the button and returns the one removal it made.
func (c *ksrCell) seqRemoval(label string, press func(c *ksrCell) error) domain.Order {
	c.t.Helper()
	before := c.lastID()
	if err := press(c); err != nil {
		c.dump(label + " refused")
		c.t.Fatalf("%s refused: %v", label, err)
	}
	c.tick()
	made := c.newRows(before)
	if len(made) != 1 || made[0].OrderType != protocol.OrderTypeComplex || made[0].DeliveryNode == ksrLine {
		c.dump(label)
		c.t.Fatalf("%s made %d orders, want one removal", label, len(made))
	}
	return made[0]
}

// seqAtTheLine sends the removal's robot to the line, where it waits for the
// operator. Nothing has called the backfill yet: the release at the line does
// (seqRelease).
func (c *ksrCell) seqAtTheLine(label string, removal domain.Order) {
	c.t.Helper()
	c.eventually(label+": the removal with the fleet", func() bool {
		return c.coreOf(removal).VendorOrderID != ""
	})
	c.drive(c.coreOf(removal), "RUNNING", "WAITING")
	c.eventually(label+": the removal waits at the line", func() bool {
		return c.edgeRow(removal.ID).Status == protocol.StatusStaged
	})
	if b := c.backfillOf(removal); b.ID != 0 {
		c.dump(label)
		c.t.Fatalf("%s: backfill %d made while the removal waits at the line unreleased, want none before the release", label, b.ID)
	}
}

// backfillOf is the backfill made after a removal (an order bringing the line a
// bin), or a zero order.
func (c *ksrCell) backfillOf(removal domain.Order) domain.Order {
	var backfill domain.Order
	for _, o := range c.newRows(removal.ID) {
		if o.OrderType == protocol.OrderTypeComplex && o.DeliveryNode == ksrLine {
			backfill = o
		}
	}
	return backfill
}

// seqRelease is the operator's RELEASE of the removal at the line, which calls
// the backfill; it is returned.
func (c *ksrCell) seqRelease(label string, removal domain.Order) domain.Order {
	c.t.Helper()
	mustNil(c.t, c.edge.Engine.ReleaseOrderWithLineside(removal.ID,
		edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}), label+": release")
	c.settle()
	var backfill domain.Order
	c.eventually(label+": the release at the line made the backfill", func() bool {
		backfill = c.backfillOf(removal)
		return backfill.ID != 0
	})
	return backfill
}

// seqLand lets the fleet finish the released removal and the backfill: the
// backfill is delivered and not yet confirmed.
func (c *ksrCell) seqLand(label string, removal, backfill domain.Order) {
	c.t.Helper()
	c.drive(c.coreOf(removal), "RUNNING", "FINISHED")
	c.eventually(label+": the backfill delivered", func() bool {
		if co := c.coreOf(backfill); co.VendorOrderID != "" {
			switch c.vendorState(co.VendorOrderID) {
			case "CREATED", "RUNNING":
				c.drive(co, "RUNNING", "FINISHED")
			}
		}
		if c.edgeRow(removal.ID).Status == protocol.StatusDelivered {
			mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(removal.ID, 1), "confirm the removal")
		}
		return c.edgeRow(backfill.ID).Status == protocol.StatusDelivered
	})
}

// seqConfirm is the operator confirming the backfill: the cycle is over, and
// every order of it ended confirmed at Core with one bin on the line.
func (c *ksrCell) seqConfirm(label string, removal, backfill domain.Order) {
	c.t.Helper()
	mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(backfill.ID, 1), label+": confirm the backfill")
	c.eventually(label+": the cycle over at both ends", func() bool {
		for _, o := range []domain.Order{removal, backfill} {
			if !protocol.IsTerminal(c.edgeRow(o.ID).Status) || !protocol.IsTerminal(c.coreOf(o).Status) {
				return false
			}
		}
		return true
	})
	for _, o := range []domain.Order{removal, backfill} {
		if s := c.coreOf(o).Status; s != protocol.StatusConfirmed {
			c.t.Errorf("%s: order %d ended %s at Core, want confirmed", label, o.ID, s)
		}
	}
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	onLine := 0
	for _, b := range bins {
		if c.nodeName(b.NodeID) == ksrLine {
			onLine++
		}
	}
	if onLine != 1 {
		c.dump(label)
		c.t.Errorf("%s: %d bins on the line, want 1", label, onLine)
	}
}

// seqRefused presses the button while the line's swap is in flight: it is
// refused, and nothing is made.
func (c *ksrCell) seqRefused(label string, press func(c *ksrCell) error) {
	c.t.Helper()
	before := c.lastID()
	err := press(c)
	c.tick()
	if made := c.newRows(before); len(made) != 0 || err == nil {
		c.dump(label)
		c.t.Fatalf("%s made %d orders while the line's swap is in flight (err=%v), want a refusal and none",
			label, len(made), err)
	}
	c.t.Logf("%s: refused: %v", label, err)
}

// One line, one live swap, as in the other swap modes: a second request while
// the removal waits at the line, or while the backfill is delivered and not yet
// confirmed, is refused and makes nothing. The backfill is called by the
// removal's release at the line, not before. Once the operator confirms the
// backfill, the next request starts the next cycle: two cycles back to back on
// every button, each release making one backfill and each cycle ending with one
// bin on the line.
func TestScenario_SequentialLine_SecondRequestWaitsForTheSwap(t *testing.T) {
	t.Parallel()
	for _, d := range seqDoors {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			c := newKsrCell(t, ksrOpts{role: d.role, mode: protocol.SwapModeSequential, plain: true})
			mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
			c.settle()
			removal := c.seqRemoval("the request", d.press)
			c.seqAtTheLine("the request", removal)
			c.seqRefused("a second request, the removal at the line", d.press)
			backfill := c.seqRelease("the request", removal)
			c.seqLand("the request", removal, backfill)
			c.seqRefused("a second request, the backfill delivered", d.press)
			c.seqConfirm("the request", removal, backfill)

			mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
			c.settle()
			next := c.seqRemoval("the next request", d.press)
			c.seqAtTheLine("the next request", next)
			backfill = c.seqRelease("the next request", next)
			c.seqLand("the next request", next, backfill)
			c.seqConfirm("the next request", next, backfill)
		})
	}
}
