// A bin already on its way to a line, sent by another station that shares the
// line's position, and every request button of both roles pressed at the line
// meanwhile. Carried end to end across the two modules.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// sibling is a process node of another process on the cell's own core node,
// with the same claim on the line: two stations that share one physical
// position.
func (c *ksrCell) sibling() int64 {
	c.t.Helper()
	db := c.edge.DB
	proc, err := db.CreateProcess("KSR-PROC-2", "a second station on the line", "active_production", "", "", false)
	mustNil(c.t, err, "sibling process")
	id, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: proc, CoreNodeName: ksrLine, Code: "KSR2", Name: ksrLine + "-2", Sequence: 1, Enabled: true,
	})
	mustNil(c.t, err, "sibling process node")
	style, err := db.CreateStyle("KSR-A2", "", proc)
	mustNil(c.t, err, "sibling style")
	mustNil(c.t, db.SetActiveStyle(proc, &style), "sibling active style")
	claim, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: style, CoreNodeName: ksrLine, Role: c.role, SwapMode: c.mode,
		PayloadCode: ksrPartA, UOPCapacity: 40, ReorderPoint: 10,
		InboundSource: ksrMktA, InboundStaging: ksrSpot, OutboundStaging: ksrOutStg,
		OutboundDestination: ksrDest, KeepStaged: domain.Ptr(false),
	})
	mustNil(c.t, err, "sibling claim")
	_, err = db.EnsureProcessNodeRuntime(id)
	mustNil(c.t, err, "sibling runtime")
	mustNil(c.t, db.SetProcessNodeRuntime(id, &claim, 30), "sibling runtime claim")
	return id
}

// everyRowAfter is every Edge order of every process created after id.
func (c *ksrCell) everyRowAfter(id int64) []domain.Order {
	c.t.Helper()
	all, err := c.edge.DB.ListOrders()
	mustNil(c.t, err, "every edge row")
	var out []domain.Order
	for _, o := range all {
		if o.ID > id {
			out = append(out, o)
		}
	}
	return out
}

// pressAt presses the door's button at the given process node.
func pressAt(c *ksrCell, door string, nodeID int64) error {
	var err error
	switch door {
	case "material request":
		_, err = c.edge.Engine.RequestNodeMaterial(nodeID, 1)
	case "produce request":
		_, err = c.edge.Engine.RequestProduceSwap(nodeID)
	case "empty-bin request":
		_, err = c.edge.Engine.RequestEmptyBin(nodeID, ksrPartA)
	}
	return err
}

// The line is bare and a sibling station has already sent a bin to it. While
// that bin is on its way, every button at the line is refused and makes
// nothing: one line, one bin coming. Once the sibling's bin is delivered (and
// not yet confirmed) the line holds a bin, and every button makes the swap,
// which runs to the end with one bin on the line.
func TestScenario_LineInbound_FromASiblingStation(t *testing.T) {
	for _, d := range seqDoors {
		t.Run(d.name, func(t *testing.T) {
			c := newKsrCell(t, ksrOpts{role: d.role, mode: protocol.SwapModeTwoRobot, plain: true})
			mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
			sib := c.sibling()
			c.takeLineBinOff()
			c.settle()

			// The sibling station asks first: a bin for the shared position.
			before := c.lastID()
			if err := pressAt(c, d.name, sib); err != nil {
				c.dump("sibling refused")
				t.Fatalf("the sibling's %s on a bare line refused: %v", d.name, err)
			}
			c.tick()
			first := c.everyRowAfter(before)
			if len(first) != 1 || first[0].DeliveryNode != ksrLine {
				c.dump("sibling")
				t.Fatalf("the sibling made %d orders, want one delivery to the line", len(first))
			}
			f := first[0]

			// On its way: the line's button is refused and makes nothing.
			err := pressAt(c, d.name, c.nodeID)
			c.tick()
			if made := c.everyRowAfter(f.ID); len(made) != 0 || err == nil {
				c.dump(d.name + " with the sibling's bin on its way")
				t.Fatalf("%s at the line while the sibling's bin is on its way: made %d orders (err=%v), "+
					"want a refusal and none", d.name, len(made), err)
			}
			t.Logf("%s with the sibling's bin on its way: refused: %v", d.name, err)

			// Delivered, not confirmed: the line holds a bin, and the button makes
			// the swap. A plain two-robot swap stages its supply on the spot: clear
			// the spare the cell keeps there.
			c.eventually("the sibling's delivery landed", func() bool {
				c.fleetStep()
				return c.edgeRow(f.ID).Status == protocol.StatusDelivered
			})
			if sp := c.spareOnSpot(); sp != nil {
				slot, err := c.core.eng.DB().GetNodeByName(ksrMktA + "-5")
				mustNil(t, err, "a free slot of A's market")
				mustNil(t, c.core.eng.DB().MoveBinClearingStaging(sp.ID, slot.ID, true), "the spare off the spot")
			}
			c.swap(d.name+" with the sibling's bin delivered", func() {
				if err := pressAt(c, d.name, c.nodeID); err != nil {
					c.dump(d.name + " refused")
					t.Fatalf("%s at the line holding the sibling's delivered bin refused: %v", d.name, err)
				}
			})
			bins, err := c.core.eng.DB().ListBins()
			mustNil(t, err, "bins")
			onLine := 0
			for _, b := range bins {
				if c.nodeName(b.NodeID) == ksrLine {
					onLine++
				}
			}
			if onLine != 1 {
				c.dump("after the swap")
				t.Errorf("%d bins on the line after the swap, want 1", onLine)
			}
		})
	}
}
