// A changeover started while an order is on its way to lift a changing node's
// bin, and one started with a changing node already empty, carried end to end
// across the two modules.
//
//go:build docker

package scenarios

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgeuop "shingoedge/uop"

	corebins "shingocore/store/bins"
)

// pairRemoval presses the standby position's material request and returns the
// removal it made: wait(KSR-PAIR) pickup(KSR-PAIR) dropoff(KSR-DEST), with the
// fleet.
func (c *ksrCell) pairRemoval(label string) domain.Order {
	c.t.Helper()
	mustNil(c.t, c.edge.DB.SetProcessNodeRuntime(c.pairID, &c.pairClaimA, 5), label+": count at the level")
	before := c.lastID()
	if _, err := c.edge.Engine.RequestNodeMaterial(c.pairID, 1); err != nil {
		c.dump(label)
		c.t.Fatalf("%s: the standby position's request was refused: %v", label, err)
	}
	c.tick()
	var removal domain.Order
	for _, o := range c.newRows(before) {
		if o.OrderType == protocol.OrderTypeComplex && o.ProcessNodeID != nil && *o.ProcessNodeID == c.pairID {
			removal = o
		}
	}
	if removal.ID == 0 {
		c.dump(label)
		c.t.Fatalf("NOT REACHED: %s: no removal at KSR-PAIR", label)
	}
	c.eventually(label+": the removal with the fleet", func() bool {
		return c.coreOf(removal).VendorOrderID != ""
	})
	return removal
}

// emptyByRelease takes the bin off a position the way the floor does: the
// position's seat holds the bin standing there, as a landing leaves it; the
// Material page's RELEASE sends it to the outbound destination; the fleet lifts
// it (the seat records the carrier's departure) and sets it down; the operator
// confirms. It ends with nothing at the position at Core and the seat empty.
func (c *ksrCell) emptyByRelease(label string, nodeID int64, coreNode string) {
	c.t.Helper()
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	bound := false
	for _, b := range bins {
		if c.nodeName(b.NodeID) == coreNode {
			id := b.ID
			mustNil(c.t, c.edge.DB.SetProcessNodeActiveBinID(nodeID, &id), label+": the seat holds "+b.Label)
			bound = true
		}
	}
	if !bound {
		c.notReached(label + ": no bin at " + coreNode + " to release")
	}
	// The Edge's inventory sink, as a plant runs it: the pickup clears the seat
	// through it (the test Edge leaves it unwired).
	c.edge.Engine.SetInventoryDeltaSink(edgeuop.New(c.edge.DB, "edge.test", c.edge.DB, c.edge.DB))
	o, err := c.edge.Engine.ReleaseNodeEmpty(nodeID)
	if err != nil || o == nil {
		c.dump(label)
		c.t.Fatalf("%s: the Material page's RELEASE at %s was refused: %v", label, coreNode, err)
	}
	rel := *o
	c.eventually(label+": the release with the fleet", func() bool {
		return c.coreOf(rel).VendorOrderID != ""
	})
	c.drive(c.coreOf(rel), "RUNNING")
	if !c.liftAt(c.coreOf(rel), coreNode, true) {
		c.notReached(label + ": the release has no pickup block at " + coreNode)
	}
	c.drive(c.coreOf(rel), "FINISHED")
	c.eventually(label+": the release done at both ends", func() bool {
		if c.edgeRow(rel.ID).Status == protocol.StatusDelivered {
			mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(rel.ID, 1), "confirm the release")
		}
		return protocol.IsTerminal(c.edgeRow(rel.ID).Status) && protocol.IsTerminal(c.coreOf(rel).Status)
	})
	rt, err := c.edge.DB.GetProcessNodeRuntime(nodeID)
	mustNil(c.t, err, "runtime")
	if rt.ActiveBinID != nil {
		c.notReached(label + ": the seat at " + coreNode + " still holds a bin after its lift")
	}
}

// runningCell is a plain cell as a running line holds it: the inbound staging
// spot clear (the plain cell parks A's spare there), and a press's deck
// holding its next A bin.
func (c *ksrCell) runningCell(label string) {
	c.t.Helper()
	coreDB := c.core.eng.DB()
	free, err := coreDB.GetNodeByName(ksrMktA + "-8")
	mustNil(c.t, err, label+": a free slot in A's market")
	bins, err := coreDB.ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if c.nodeName(b.NodeID) == ksrSpot {
			mustNil(c.t, coreDB.MoveBinClearingStaging(b.ID, free.ID, true), label+": "+b.Label+" off the spot")
		}
	}
	if c.mode != protocol.SwapModeTwoRobotPressIndex {
		c.bus.PushBins() // the spot cleared, in the Edge's table
		return
	}
	deck, err := coreDB.GetNodeByName(ksrDeck)
	mustNil(c.t, err, "deck")
	b := &corebins.Bin{BinTypeID: c.typeA.ID, Label: "KSR-A-DECK", NodeID: &deck.ID, Status: "available"}
	mustNil(c.t, coreDB.CreateBin(b), label+": the deck's bin")
	mustNil(c.t, coreDB.SetBinManifest(b.ID, `{"items":[]}`, ksrPartA, 40), "deck manifest")
	mustNil(c.t, coreDB.ConfirmBinManifest(b.ID, ""), "deck confirm")
	c.bus.PushBins() // the deck's bin reaches the Edge's table, as Core's push would
}

// runChangeover is the operator running a started changeover to its end at
// the station: each order the changeover made is carried by the fleet; a leg
// staged at a wait is released at its node, as the tile's RELEASE does; a
// delivered leg is confirmed; once the gate opens CUTOVER is pressed. It ends
// with the changeover completed, the process on B, and a B bin on each
// position in positions.
func (c *ksrCell) runChangeover(label string, after int64, positions ...string) {
	c.t.Helper()
	disp := edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}
	c.eventually(label+": the changeover's legs done and its gate open", func() bool {
		c.fleetStep()
		done := true
		for _, o := range c.newRows(after) {
			edge := c.edgeRow(o.ID)
			if edge.Status == protocol.StatusDelivered {
				mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(o.ID, 1), "confirm")
			}
			if protocol.IsTerminal(edge.Status) && protocol.IsTerminal(c.coreOf(o).Status) {
				continue
			}
			done = false
			if o.OrderType != protocol.OrderTypeComplex {
				continue // the fleet carries a plain order (fleetStep)
			}
			core := c.coreOf(o)
			if core.VendorOrderID == "" {
				continue
			}
			view := c.core.sim.GetOrder(core.VendorOrderID)
			if view == nil {
				continue
			}
			switch view.State {
			case "CREATED":
				c.drive(core, "RUNNING")
			case "RUNNING":
				if view.Complete {
					c.drive(core, "FINISHED")
				} else {
					c.drive(core, "WAITING")
				}
			case "WAITING":
				if view.Complete {
					c.drive(core, "RUNNING")
				} else if edge.Status == protocol.StatusStaged && edge.ProcessNodeID != nil {
					if _, err := c.edge.Engine.ReleaseChangeoverWaitForNode(c.processID, *edge.ProcessNodeID, disp); err != nil {
						c.t.Logf("%s: release of order %d at node %d: %v", label, o.ID, *edge.ProcessNodeID, err)
					}
					c.settle()
				}
			}
		}
		if !done {
			return false
		}
		ok, _, err := c.edge.Engine.ChangeoverGateStatus(c.processID)
		return err == nil && ok
	})
	mustNil(c.t, c.edge.Engine.CompleteProcessProductionCutover(c.processID), label+": CUTOVER")
	c.settle()
	co, err := c.edge.DB.GetActiveProcessChangeover(c.processID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		c.t.Fatalf("%s: active changeover: %v", label, err)
	}
	if co != nil {
		c.dump(label)
		c.t.Fatalf("%s: changeover %d still %s after CUTOVER", label, co.ID, co.State)
	}
	proc, err := c.edge.DB.GetProcess(c.processID)
	mustNil(c.t, err, "process")
	if proc.ActiveStyleID == nil || *proc.ActiveStyleID != c.styleB {
		c.t.Errorf("%s: the process is on style %v after CUTOVER, want B (%d)", label, proc.ActiveStyleID, c.styleB)
	}
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	onB := map[string]int{}
	for _, b := range bins {
		if b.BinTypeID == c.typeB.ID {
			onB[c.nodeName(b.NodeID)]++
		}
	}
	for _, pos := range positions {
		if onB[pos] != 1 {
			c.dump(label)
			c.t.Errorf("%s: %d B bins on %s after CUTOVER, want 1 (B bins by node: %v)", label, onB[pos], pos, onB)
		}
	}
}

// The standby position's removal is with the fleet, on its way to wait at its
// position, or waiting there, when the operator presses START. Its lift is
// still ahead and not released: START cancels it at both ends, the bin stays
// for the changeover's own swap at KSR-PAIR, and the changeover runs to
// CUTOVER (edge2 run post-edge2c-c8a8cc21: PRS_010B's removal passed START
// and the changeover's swap waited for its bin for good).
func TestScenario_ChangeoverStart_CancelsALiftOnlyOrderAtAChangingNode(t *testing.T) {
	for _, m := range []struct {
		name   string
		states []string
	}{
		{"with the fleet", nil},
		{"in transit to its wait", []string{"RUNNING"}},
		{"staged at its wait", []string{"RUNNING", "WAITING"}},
	} {
		t.Run(m.name, func(t *testing.T) {
			c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential, plain: true, pair: true})
			removal := c.pairRemoval(m.name)
			if len(m.states) > 0 {
				c.drive(c.coreOf(removal), m.states...)
			}
			c.tick()

			before := c.lastID()
			if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", ""); err != nil {
				c.dump(m.name)
				t.Fatalf("START refused with the standby removal %s: %v — its lift is ahead and not "+
					"released, so it is cancelled, not waited for", m.name, err)
			}
			c.tick()
			c.eventually(m.name+": the removal cancelled at both ends", func() bool {
				return c.edgeRow(removal.ID).Status == protocol.StatusCancelled &&
					protocol.IsTerminal(c.coreOf(removal).Status)
			})
			c.runChangeover(m.name, before, ksrLine, ksrPair)
		})
	}
}

// A changeover started with a changing position already empty, one case per
// swap type: the bin was released off the position from the Material page and
// lifted before START. The position needs the new style's bin and has nothing
// to take away, and the changeover runs to CUTOVER with a B bin on it.
func TestScenario_ChangeoverStart_WithAChangingPositionAlreadyEmpty(t *testing.T) {
	for _, m := range []struct {
		name string
		opts ksrOpts
	}{
		{"sequential, the standby position", ksrOpts{mode: protocol.SwapModeSequential, pair: true}},
		{"single_robot", ksrOpts{mode: protocol.SwapModeSingleRobot}},
		{"two_robot", ksrOpts{mode: protocol.SwapModeTwoRobot}},
		{"two_robot_press_index", ksrOpts{mode: protocol.SwapModeTwoRobotPressIndex}},
	} {
		t.Run(m.name, func(t *testing.T) {
			opts := m.opts
			opts.role, opts.plain = protocol.ClaimRoleConsume, true
			c := newKsrCell(t, opts)
			nodeID, pos, positions := c.nodeID, ksrLine, []string{ksrLine}
			if opts.pair {
				nodeID, pos, positions = c.pairID, ksrPair, []string{ksrLine, ksrPair}
			}
			c.runningCell(m.name)
			c.emptyByRelease(m.name, nodeID, pos)

			before := c.lastID()
			if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", ""); err != nil {
				c.dump(m.name)
				t.Fatalf("%s: START refused with %s empty: %v", m.name, pos, err)
			}
			c.tick()
			c.runChangeover(m.name, before, positions...)
		})
	}
}
