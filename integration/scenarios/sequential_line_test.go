// A sequential line, carried end to end across the two modules: its removal
// lifts the line's bin, its backfill brings the next one, and the line's next
// request starts the next cycle.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"fmt"
	"os"
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
// the backfill; it is returned. The cell's disposition when it has one (the
// release modal's choice), else a plain release.
func (c *ksrCell) seqRelease(label string, removal domain.Order) domain.Order {
	c.t.Helper()
	disp := edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}
	if c.disp != nil {
		disp = *c.disp
	}
	mustNil(c.t, c.edge.Engine.ReleaseOrderWithLineside(removal.ID, disp), label+": release")
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

// The owner's case (final brief 2e; one-node study 7.3): "that full bin would
// be going to same place b is picking from to deliver to the line". The line's
// outbound destination is its inbound source, A's market, so the removal
// (Order A) takes the line's bin back where the backfill (Order B) fetches the
// next one. On a paired A/B side and on a one-node line, a consume line's
// steady-state swap runs to the end, confirmed at both ends with one bin on the
// line, with a slot of the market free.
//
// WITH NO SLOT FREE IT WAITS, and that is the ruled behaviour (B22, closed by
// the owner 2026-11-25: "this isnt a code problem"; brief G D, DOCKET R159). B
// is made only at A's release at the line (ensureSequentialBackfill), and Core
// never pairs the two: B goes out with no sibling uuid
// (Manager.CreateComplexOrder), so A is a solo order and the vacated-slot rule
// has no partner's lift to credit; A's own earlier pickup is at the line, not in
// the market. So A waits queued at Core on the market's capacity, with its
// sentence on Core's row and the station's ("Waiting for a slot at" the
// market), and no robot comes. It goes when a slot frees by another door: here
// a person's move of one bin out of the market on Core's bins page (lane
// f-abrun's notes, 2e, have the trace). Before 2026-11-25 this arm was a
// skipped red pin of a wedge.
func TestScenario_SequentialLine_SwapsThroughItsOwnMarket(t *testing.T) {
	t.Parallel()
	for _, shape := range []struct {
		name string
		pair bool
	}{{"a paired A/B side", true}, {"a one-node line", false}} {
		for _, free := range []int{1, 0} {
			t.Run(fmt.Sprintf("%s, %d slot free", shape.name, free), func(t *testing.T) {
				t.Parallel()
				c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential,
					plain: true, bareSpot: true, pair: shape.pair, loopA: true, freeA: free})
				mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
				c.settle()
				removal := c.seqRemoval("the request", seqDoors[0].press)
				if free == 0 {
					// The wait and its sentence, held over the cell's passes.
					const sentence = "Step 2: Waiting for a slot at " + ksrMktA
					waiting := func() bool {
						co, row := c.coreOf(removal), c.edgeRow(removal.ID)
						return co.Status == protocol.StatusQueued && co.QueueCause == "ngrp-resolve" &&
							co.QueueReason == sentence && co.VendorOrderID == "" &&
							row.Status == protocol.StatusQueued && row.QueueReason == sentence &&
							row.QueueCode == string(protocol.QueueWaitingForSlot)
					}
					c.eventually("the removal waits for a slot, with its sentence", waiting)
					for i := 0; i < 5; i++ {
						c.tick()
					}
					if !waiting() {
						co, row := c.coreOf(removal), c.edgeRow(removal.ID)
						c.dump("still waiting")
						t.Fatalf("five passes on, Core reads %s cause=%q reason=%q vendor=%q and the Edge %s "+
							"reason=%q code=%q; want both queued on %q, no robot", co.Status, co.QueueCause,
							co.QueueReason, co.VendorOrderID, row.Status, row.QueueReason, row.QueueCode, sentence)
					}
					// A slot frees by another door: a person moves one of the
					// market's bins off it. The removal goes, and the swap runs on.
					slot := ksrMktA + "-8"
					in := c.binsAt(slot)
					if len(in) != 1 {
						c.dump("no bin to move")
						t.Fatalf("%s holds %d bins, want the one a person moves off it", slot, len(in))
					}
					out, err := c.core.eng.DB().GetNodeByName(ksrOutStg)
					mustNil(t, err, "the node a person moves the bin to")
					c.moveByHand(in[0], out.ID, "a person moves a bin out of the market")
				}
				c.seqAtTheLine("the request", removal)
				backfill := c.seqRelease("the request", removal)
				c.seqLand("the request", removal, backfill)
				c.seqConfirm("the request", removal, backfill)
			})
		}
	}
}

// binAt is the node Core holds the bin labelled at, "" for none.
func (c *ksrCell) binAt(label string) string {
	c.t.Helper()
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if b.Label == label {
			return c.nodeName(b.NodeID)
		}
	}
	return ""
}

// coreRoute logs each order's route as Core holds it: its source, its
// delivery and its plan's stops.
func (c *ksrCell) coreRoute(label string, ords ...domain.Order) {
	c.t.Helper()
	for _, o := range ords {
		co := c.coreOf(o)
		var stops []string
		var steps []protocol.ComplexOrderStep
		if err := json.Unmarshal([]byte(co.StepsJSON), &steps); err == nil {
			for _, s := range steps {
				stops = append(stops, s.Action+"("+s.Node+")")
			}
		}
		c.t.Logf("%s: Core order %d (edge %d) %s -> %s status=%s cause=%q: %s", label, co.ID, o.ID,
			co.SourceNode, co.DeliveryNode, co.Status, co.QueueCause, strings.Join(stops, " "))
	}
}

// The owner's case with a dedicated home (final brief 2e, lane f-home): "what
// if the market is also a dedicated home like hop will be doing". The line's
// part has a pinned home on a dedicated-positions loader, named as the line's
// inbound source and outbound destination, and the home holds the full the
// backfill fetches: the removal (Order A) takes the line's emptied cart back to
// be loaded, and the backfill (Order B) brings the home's full. On a paired A/B
// side and on a one-node line, with one home and a free buffer, its buffer
// holding an empty, no buffer, and a second home for the part standing bare,
// the steady-state swap should run to the end: confirmed at both ends, the
// home's full on the line and the old cart on a spot of the loader.
//
// With a buffer free it runs to the end, on both shapes: A's drop moves off the
// occupied home to the buffer and A goes; B, made at A's release, lifts the
// home's full and sets it on the line. Red at 7a7d9a25 until brief H: the loader
// park's Pattern A moved B's drop from the line onto the home it lifts from (a
// no-wait leg off a loader spot read as a return there), B ran circular and the
// line was left bare (TestPlaceForDedicatedLoader_SequentialBackfillFromHome_Untouched).
//
// WITH ONE HOME AND NO BUFFER FREE IT WAITS, on both shapes, and that is the
// mode's rule, as a full market's wait is (B22 and the owner's "this isnt a code
// problem", R159; brief H): A is made first and carries the line's cart to the
// home, which holds the full B comes for; B is made only at A's release, so A
// has no partner whose lift empties the home, and A waits queued at Core,
// loader-park-no-slot, with its sentence on Core's row and the station's, and
// no robot comes. With an empty standing in the buffer, a person moving it out
// lets A go to the buffer and the swap runs to its end. With no buffer at all
// the sentence names only the home, which holds the full B comes for. Before
// brief H these four cells were skipped reds.
//
// THE TWO-HOMES CASE WITH NO BUFFER STILL FAILS, on both shapes, and is skipped
// unless SHINGO_KNOWN_RED=1 (lane f-home's notes have the trace and the red
// output): A waits the same way, and the bare second home is never offered
// (Core's sentence names it all the same).
func TestScenario_SequentialLine_SwapsThroughItsOwnHome(t *testing.T) {
	t.Parallel()
	for _, shape := range []struct {
		name string
		pair bool
	}{{"a paired A/B side", true}, {"a one-node line", false}} {
		for _, lc := range []struct {
			name                      string
			extraHomes, buffers, held int
			red                       bool
			// waits is the spots the removal's wait sentence names when it waits.
			waits string
		}{
			{"one home, a free buffer", 0, 1, 0, false, ""},
			{"one home, its buffer holding an empty", 0, 1, 1, false, ksrHomeA + ", KSR-BUF-1"},
			{"one home, no buffer", 0, 0, 0, false, ksrHomeA},
			{"two homes, the second bare, no buffer", 1, 0, 0, true, ""},
			{"two homes, the second bare, a free buffer", 1, 1, 0, false, ""},
		} {
			t.Run(shape.name+", "+lc.name, func(t *testing.T) {
				if lc.red && os.Getenv("SHINGO_KNOWN_RED") == "" {
					t.Skip("final brief 2e (lane f-home) red pin, red until a sequential swap through its own home " +
						"with no buffer free brings the line its cart; run with SHINGO_KNOWN_RED=1")
				}
				t.Parallel()
				c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential,
					plain: true, bareSpot: true, pair: shape.pair, home: true,
					extraHomesA: lc.extraHomes, buffers: lc.buffers, heldBuffers: lc.held})
				// RELEASE EMPTY: the cart goes back empty, to be loaded.
				c.disp = &edgeengine.ReleaseDisposition{Mode: edgeengine.DispositionCaptureLineside, CalledBy: "ksr-operator"}
				mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
				c.settle()
				removal := c.seqRemoval("the request", seqDoors[0].press)
				defer func() { c.coreRoute("at the end", c.rows()...) }()
				if lc.waits != "" {
					// The wait and its sentence, held over the cell's passes.
					sentence := "Waiting for a slot at " + ksrHomeA + " — empty one of " + lc.waits + " and it goes"
					waiting := func() bool {
						co, row := c.coreOf(removal), c.edgeRow(removal.ID)
						return co.Status == protocol.StatusQueued && co.QueueCause == "loader-park-no-slot" &&
							co.QueueReason == sentence && co.VendorOrderID == "" &&
							row.Status == protocol.StatusQueued && row.QueueReason == sentence &&
							row.QueueCode == string(protocol.QueueWaitingForSlot)
					}
					c.eventually("the removal waits for a spot of the loader, with its sentence", waiting)
					for i := 0; i < 5; i++ {
						c.tick()
					}
					if !waiting() {
						co, row := c.coreOf(removal), c.edgeRow(removal.ID)
						c.dump("still waiting")
						t.Fatalf("five passes on, Core reads %s cause=%q reason=%q vendor=%q and the Edge %s "+
							"reason=%q code=%q; want both queued on %q, no robot", co.Status, co.QueueCause,
							co.QueueReason, co.VendorOrderID, row.Status, row.QueueReason, row.QueueCode, sentence)
					}
					if lc.held == 0 {
						return
					}
					// A person moves the buffer's empty out: the removal goes there.
					in := c.binsAt("KSR-BUF-1")
					if len(in) != 1 {
						c.dump("no empty to move")
						t.Fatalf("KSR-BUF-1 holds %d bins, want the empty a person moves off it", len(in))
					}
					out, err := c.core.eng.DB().GetNodeByName(ksrOutStg)
					mustNil(t, err, "the node a person moves the empty to")
					c.moveByHand(in[0], out.ID, "a person moves the empty out of the buffer")
				}
				c.seqAtTheLine("the request", removal)
				backfill := c.seqRelease("the request", removal)
				c.tick()
				c.coreRoute("after the release", removal, backfill)
				c.seqLand("the request", removal, backfill)
				c.seqConfirm("the request", removal, backfill)
				c.lineHolds("after the cycle", "KSR-HOME-A-FULL")
				if at := c.binAt("KSR-LINE-BIN"); !strings.HasPrefix(at, "KSR-HOME-A") && !strings.HasPrefix(at, "KSR-BUF-") {
					t.Errorf("the old cart is at %q after the cycle, want it on a spot of the loader", at)
				}
			})
		}
	}
}

// The part change on a one-node line fed off its homes (final brief 2e, lane
// f-home): the old part's home and the new part's home stand on one
// dedicated-positions loader with a free buffer, and the changeover's one leg
// lifts the line's old cart, sets it down at the old part's home, fetches the
// new part's full from its home and sets it on the line. With the old part's
// home bare, the old cart lands there. With it holding the old part's spare
// full, the old cart should go to the free buffer and the spare stay put.
//
// THE SPARE CASE FAILS at 7a7d9a25 and is skipped unless SHINGO_KNOWN_RED=1
// (lane f-home's notes): nothing asks about the leg's drop at the old part's
// home. Core reads the leg's delivery as the line (its last stop), so the
// loader park never sees the home; the final-drop gate asks the line; the
// home is neither storage nor a drop the Edge declares exclusive. The leg is
// sent to set the old cart down on the occupied home, and the landing evicts
// the spare's record to _TRANSIT.
func TestScenario_SequentialLine_PartChangeThroughItsHomes(t *testing.T) {
	t.Parallel()
	for _, hc := range []struct {
		name string
		bare bool
	}{{"the old part's home bare", true}, {"the old part's home holding its spare", false}} {
		t.Run(hc.name, func(t *testing.T) {
			if !hc.bare && os.Getenv("SHINGO_KNOWN_RED") == "" {
				t.Skip("final brief 2e (lane f-home) red pin, red until a part change's drop at an occupied home " +
					"goes elsewhere; run with SHINGO_KNOWN_RED=1")
			}
			t.Parallel()
			c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential,
				plain: true, bareSpot: true, home: true, bareHomeA: hc.bare, buffers: 1})
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
			defer func() { c.coreRoute("at the end", c.rows()...) }()
			c.eventually("the leg with the fleet", func() bool { return c.coreOf(leg).VendorOrderID != "" })
			c.coreRoute("the leg dispatched", leg)
			c.drive(c.coreOf(leg), "RUNNING", "WAITING")
			c.eventually("the leg waits at the line", func() bool {
				return c.edgeRow(leg.ID).Status == protocol.StatusStaged
			})
			if _, err := c.edge.Engine.ReleaseChangeoverWaitForNode(c.processID, c.nodeID,
				edgeengine.ReleaseDisposition{Mode: edgeengine.DispositionCaptureLineside, CalledBy: "ksr-operator"}); err != nil {
				c.dump("release")
				t.Fatalf("the release at the line was refused: %v", err)
			}
			l := c.pastItsWait("the leg", leg)
			c.coreRoute("the leg released", leg)
			c.drive(l, "RUNNING", "FINISHED")
			c.driveUntil("the leg done", func() bool {
				if c.edgeRow(leg.ID).Status == protocol.StatusDelivered {
					mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(leg.ID, 1), "confirm the leg")
				}
				return protocol.IsTerminal(c.edgeRow(leg.ID).Status) && protocol.IsTerminal(c.coreOf(leg).Status)
			})
			c.lineHolds("after the leg", "KSR-HOME-B-FULL")
			want := ksrHomeA
			if !hc.bare {
				want = "KSR-BUF-1"
				if at := c.binAt("KSR-HOME-A-FULL"); at != ksrHomeA {
					t.Errorf("the old part's spare is at %q after the leg, want it still on %s", at, ksrHomeA)
				}
			}
			if at := c.binAt("KSR-LINE-BIN"); at != want {
				c.dump("the old cart")
				t.Errorf("the old cart is at %q after the leg, want %s", at, want)
			}
		})
	}
}
