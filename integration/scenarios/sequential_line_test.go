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
		_, err := c.edge.Engine.RequestProduceSwap(c.nodeID)
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
// operator. On its way the removal mints the backfill, which is returned.
func (c *ksrCell) seqAtTheLine(label string, removal domain.Order) domain.Order {
	c.t.Helper()
	c.eventually(label+": the removal with the fleet", func() bool {
		return c.coreOf(removal).VendorOrderID != ""
	})
	c.drive(c.coreOf(removal), "RUNNING", "WAITING")
	var backfill domain.Order
	c.eventually(label+": the removal waits at the line and its backfill is made", func() bool {
		for _, o := range c.newRows(removal.ID) {
			if o.OrderType == protocol.OrderTypeComplex && o.DeliveryNode == ksrLine {
				backfill = o
			}
		}
		return backfill.ID != 0 && c.edgeRow(removal.ID).Status == protocol.StatusStaged
	})
	return backfill
}

// seqLand releases the removal and lets the fleet finish it and the backfill:
// the backfill is delivered and not yet confirmed.
func (c *ksrCell) seqLand(label string, removal, backfill domain.Order) {
	c.t.Helper()
	mustNil(c.t, c.edge.Engine.ReleaseOrderWithLineside(removal.ID,
		edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}), label+": release")
	c.settle()
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

// Two cycles back to back on every button: each removal makes one backfill,
// each cycle ends with one bin on the line, and the request after a cycle has
// ended starts the next.
func TestScenario_SequentialLine_CyclesBackToBack(t *testing.T) {
	for _, d := range seqDoors {
		t.Run(d.name, func(t *testing.T) {
			c := newKsrCell(t, ksrOpts{role: d.role, mode: protocol.SwapModeSequential, plain: true})
			for _, cycle := range []string{"first cycle", "second cycle"} {
				// Parts counted, as a line asking for its swap has.
				mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
				c.settle()
				removal := c.seqRemoval(cycle, d.press)
				backfill := c.seqAtTheLine(cycle, removal)
				c.seqLand(cycle, removal, backfill)
				c.seqConfirm(cycle, removal, backfill)
			}
		})
	}
}
