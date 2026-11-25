// A sequential line, carried end to end across the two modules: its removal
// lifts the line's bin, its backfill brings the next one, and the line's next
// request starts the next cycle.
//
//go:build docker

package scenarios

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"

	corebins "shingocore/store/bins"
	coreorders "shingocore/store/orders"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
)

// seqDoors are the request buttons this test and
// TestScenario_LineInbound_FromASiblingStation carry end to end: the material
// request on a consume line and the produce request on a produce line.
//
// THE EMPTY-BIN REQUEST IS NOT AMONG THEM. On a swap-mode line it asks what the
// produce request asks (RequestEmptyBin goes through produceRequest, the same
// line guards and plan), and in both tests its cell's delivered-order
// transcript was the produce cell's in each of the recorder's 11 runs
// (REMOVAL-CANDIDATES B3, B4, 2026-11-23). Which door was pressed is Edge-local;
// that all three doors give one answer in every line state these two tests
// reach is pinned by TestRequestButtons_OneAnswerPerLineState.
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
	c.driveUntil(label+": the backfill delivered", func() bool {
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
// each of seqDoors' buttons, each release making one backfill and each cycle
// ending with one bin on the line.
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

// TestScenario_SequentialLine_OneNodeHoldsOneBinAtEveryStep: a Sequential A/B
// position with no partner, a one-node line (final brief 2a, point 1), consume
// as HOP runs it. At every step of a swap the line holds at most one bin, and
// the new one is set down on an empty line: in steady state (Order A lifts the
// old bin, Order B made at A's release brings the next) and at a changeover
// that changes the part (its one leg lifts, drops at the outbound destination,
// fetches the new part and sets it down). After each, the line holds exactly
// one bin, the new one, and the old one stands in the outbound destination.
// The steady state's two orders ride one robot, back to back, as a plant with
// one robot runs them (point 5).
//
// The changeover half was refused until final brief 2b: the planner asked for
// the partner and left the line's task in error.
func TestScenario_SequentialLine_OneNodeHoldsOneBinAtEveryStep(t *testing.T) {
	t.Parallel()
	t.Run("steady state", func(t *testing.T) {
		t.Parallel()
		c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential, plain: true})
		mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
		c.settle()
		c.lineHolds("before the request", "KSR-LINE-BIN")
		removal := c.seqRemoval("the request", seqDoors[0].press)
		c.seqAtTheLine("the request", removal)
		c.lineHolds("the removal waits at the line", "KSR-LINE-BIN")
		backfill := c.seqRelease("the request", removal)
		c.lineHolds("the removal released, not yet lifted", "KSR-LINE-BIN")
		a := c.pastItsWait("the removal", removal)
		c.drive(a, "RUNNING")
		if !c.pickupAt(a, ksrLine) {
			c.notReached("the removal has no pickup block at " + ksrLine)
		}
		c.lineHolds("the removal lifted the old bin")
		c.drive(c.coreOf(removal), "FINISHED")
		c.eventually("the removal set the old bin down", func() bool {
			return c.binAtDest("KSR-LINE-BIN")
		})
		// ONE ROBOT: the backfill rides the removal's robot (point 5).
		var b *coreorders.Order
		c.eventually("the backfill with the fleet", func() bool {
			b = c.coreOf(backfill)
			return b.VendorOrderID != ""
		})
		c.robots[b.VendorOrderID] = c.robotFor(a.VendorOrderID)
		c.lineHolds("the backfill on its way")
		c.drive(b, "RUNNING", "FINISHED")
		c.driveUntil("the cycle delivered", func() bool {
			if c.edgeRow(removal.ID).Status == protocol.StatusDelivered {
				mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(removal.ID, 1), "confirm the removal")
			}
			return c.edgeRow(backfill.ID).Status == protocol.StatusDelivered
		})
		c.lineHoldsOneNew("the backfill set the next bin down", c.typeA)
		c.seqConfirm("the cycle", removal, backfill)
		if ra, rb := c.robotFor(a.VendorOrderID), c.robotFor(b.VendorOrderID); ra != rb {
			t.Errorf("the removal rode %s and the backfill %s, want one robot for both", ra, rb)
		}
	})
	t.Run("changeover", func(t *testing.T) {
		t.Parallel()
		c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential, plain: true})
		c.lineHolds("before the changeover", "KSR-LINE-BIN")
		before := c.lastID()
		co, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", "")
		if err != nil {
			c.dump("START")
			t.Fatalf("START refused: %v", err)
		}
		c.tick()
		var leg domain.Order
		for _, o := range c.newRows(before) {
			if o.OrderType == protocol.OrderTypeComplex && o.ProcessNodeID != nil && *o.ProcessNodeID == c.nodeID {
				leg = o
			}
		}
		if leg.ID == 0 {
			task, err := c.edge.DB.GetChangeoverNodeTaskByNode(co.ID, c.nodeID)
			mustNil(t, err, "the line's task")
			c.dump("no leg")
			t.Fatalf("the changeover made no leg for the one-node line: its task is a %s at %s",
				task.Situation, task.State)
		}
		c.eventually("the leg with the fleet", func() bool { return c.coreOf(leg).VendorOrderID != "" })
		c.drive(c.coreOf(leg), "RUNNING", "WAITING")
		c.eventually("the leg waits at the line", func() bool {
			return c.edgeRow(leg.ID).Status == protocol.StatusStaged
		})
		c.lineHolds("the leg waits at the line", "KSR-LINE-BIN")
		if _, err := c.edge.Engine.ReleaseChangeoverWaitForNode(c.processID, c.nodeID,
			edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}); err != nil {
			c.dump("release")
			t.Fatalf("the release at the line was refused: %v", err)
		}
		l := c.pastItsWait("the leg", leg)
		c.drive(l, "RUNNING")
		if !c.pickupAt(l, ksrLine) {
			c.notReached("the leg has no pickup block at " + ksrLine)
		}
		c.lineHolds("the leg lifted the old bin")
		c.drive(c.coreOf(leg), "FINISHED")
		c.driveUntil("the leg done", func() bool {
			if c.edgeRow(leg.ID).Status == protocol.StatusDelivered {
				mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(leg.ID, 1), "confirm the leg")
			}
			return protocol.IsTerminal(c.edgeRow(leg.ID).Status) && protocol.IsTerminal(c.coreOf(leg).Status)
		})
		if !c.binAtDest("KSR-LINE-BIN") {
			c.dump("old bin")
			t.Errorf("the old bin is not in the outbound destination")
		}
		c.lineHoldsOneNew("the leg set B's bin down", c.typeB)
		c.driveUntil("the cutover gate open", func() bool {
			ok, _, err := c.edge.Engine.ChangeoverGateStatus(c.processID)
			return err == nil && ok
		})
		mustNil(t, c.edge.Engine.CompleteProcessProductionCutover(c.processID), "CUTOVER")
		c.settle()
		proc, err := c.edge.DB.GetProcess(c.processID)
		mustNil(t, err, "process")
		if proc.ActiveStyleID == nil || *proc.ActiveStyleID != c.styleB {
			t.Errorf("the process is on style %v after CUTOVER, want %d", proc.ActiveStyleID, c.styleB)
		}
		c.lineHoldsOneNew("after CUTOVER", c.typeB)
	})
}

// binsAt is the bins Core holds at node.
func (c *ksrCell) binsAt(node string) []*corebins.Bin {
	c.t.Helper()
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	var out []*corebins.Bin
	for _, b := range bins {
		if c.nodeName(b.NodeID) == node {
			out = append(out, b)
		}
	}
	return out
}

// lineHolds asserts the line holds exactly the bins labelled, and none when
// none is named.
func (c *ksrCell) lineHolds(label string, want ...string) {
	c.t.Helper()
	var got []string
	for _, b := range c.binsAt(ksrLine) {
		got = append(got, b.Label)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		c.dump(label)
		c.t.Errorf("%s: the line holds %v, want %v", label, got, want)
	}
}

// lineHoldsOneNew asserts the line holds one bin, of type bt, and not the one
// it started with.
func (c *ksrCell) lineHoldsOneNew(label string, bt *corebins.BinType) {
	c.t.Helper()
	on := c.binsAt(ksrLine)
	if len(on) != 1 || on[0].Label == "KSR-LINE-BIN" || on[0].BinTypeID != bt.ID {
		var got []string
		for _, b := range on {
			got = append(got, fmt.Sprintf("%s (type %d)", b.Label, b.BinTypeID))
		}
		c.dump(label)
		c.t.Errorf("%s: the line holds %v, want one new bin of %s", label, got, bt.Code)
	}
}

// binAtDest reports whether the bin labelled stands in the outbound
// destination's slots.
func (c *ksrCell) binAtDest(label string) bool {
	c.t.Helper()
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if b.Label == label {
			return strings.HasPrefix(c.nodeName(b.NodeID), ksrDest+"-")
		}
	}
	return false
}

// pastItsWait waits until Core has let the order's robot past its station wait
// (every block of the order is with the fleet) and returns Core's row.
func (c *ksrCell) pastItsWait(label string, o domain.Order) *coreorders.Order {
	c.t.Helper()
	c.eventually(label+": Core lets the robot past its wait", func() bool {
		v := c.core.sim.GetOrder(c.coreOf(o).VendorOrderID)
		return v != nil && v.Complete
	})
	return c.coreOf(o)
}

// pickupAt is the fleet reporting the order's pickup block at node done, as
// the real poller does mid-order, and the messages that follow pumped. liftAt
// takes the first block at the node, which on a sequential leg is its station
// wait there.
func (c *ksrCell) pickupAt(o *coreorders.Order, node string) bool {
	c.t.Helper()
	v := c.core.sim.GetOrder(o.VendorOrderID)
	if v == nil {
		return false
	}
	for _, b := range v.Blocks {
		if b.Location == node && b.Action == protocol.ActionPickup {
			ok := c.core.sim.CompleteBlock(o.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
			c.settle()
			return ok
		}
	}
	return false
}
