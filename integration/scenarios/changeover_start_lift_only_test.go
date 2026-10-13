// A changeover started while an order is on its way to lift a changing node's
// bin, carried end to end across the two modules on a sequential pair.
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
