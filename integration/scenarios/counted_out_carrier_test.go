// A carrier its line counted down to nothing, carried end to end across the
// two modules through two changeovers.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"

	"shingo/protocol"

	"shingocore/service"
	corebins "shingocore/store/bins"
	coreuop "shingocore/uop"

	"shingoedge/domain"
	"shingoedge/store/processes"
	edgeuop "shingoedge/uop"
)

// outboundIsMarketA points both styles' claims at A's market for their
// outbound, as a sequential consume line whose market takes its carts back is
// set up: a carrier a changeover takes off the line goes back where the line's
// next fetch looks.
func (c *ksrCell) outboundIsMarketA() {
	c.t.Helper()
	for _, s := range []struct {
		style     int64
		part, mkt string
	}{{c.styleA, ksrPartA, ksrMktA}, {c.styleB, ksrPartB, ksrMktB}} {
		for pos, partner := range map[string]string{ksrLine: ksrPair, ksrPair: ksrLine} {
			id, err := c.edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
				StyleID: s.style, CoreNodeName: pos, Role: c.role, SwapMode: c.mode, PayloadCode: s.part,
				UOPCapacity: 40, ReorderPoint: 10, InboundSource: s.mkt, InboundStaging: ksrSpot,
				OutboundStaging: ksrOutStg, OutboundDestination: ksrMktA, KeepStagedNode: domain.Ptr(""),
				AutoReorder: domain.Ptr(true), PairedCoreNode: partner,
			})
			mustNil(c.t, err, "claim "+s.part+" at "+pos)
			if s.style == c.styleA && id != map[string]int64{ksrLine: c.claimA, ksrPair: c.pairClaimA}[pos] {
				c.t.Fatalf("re-saving A's claim at %s made claim %d, not the cell's", pos, id)
			}
		}
	}
	c.settle()
}

// countOut draws the bin on node down to nothing through Core's door for a
// line's consume ticks, as the line's counter reports them, and the Edge's own
// count with it. The bin keeps its part.
func (c *ksrCell) countOut(node string) *corebins.Bin {
	c.t.Helper()
	coreDB := c.core.eng.DB()
	var on *corebins.Bin
	bins, err := coreDB.ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if c.nodeName(b.NodeID) == node {
			on = b
		}
	}
	if on == nil {
		c.notReached("no bin on " + node + " to count out")
	}
	announce := service.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "core"}
	inv := coreuop.NewInventoryDeltaService(coreDB, service.NewBinManifestService(coreDB, announce), announce)
	now := time.Now().UTC()
	mustNil(c.t, inv.ApplyBinUOPDelta("edge.test", &protocol.BinUOPDelta{
		BinID: on.ID, PayloadCode: on.PayloadCode, Delta: -on.UOPRemaining, Reason: protocol.ReasonConsumeTick,
		SequenceID: 1, Epoch: on.DeltaEpoch, WindowStart: now.Add(-time.Minute), WindowEnd: now,
	}), "the line's consume ticks")
	mustNil(c.t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 0), "the line's own count")
	out, err := coreDB.GetBin(on.ID)
	mustNil(c.t, err, "the counted-out bin")
	if out.UOPRemaining != 0 || out.PayloadCode != ksrPartA {
		c.notReached("the line's bin did not count out to 0 of its part")
	}
	return out
}

// LINE-2 of the 2026-10-23 sim, end to end (DECISIONS S.12): a cart the line
// counted down to 0 of its part, taken off by a changeover with no count,
// is still marked with the part where it lands. The changeover back fetched it
// as a full of the part, and the release onto that position was refused for
// good: the position held no material to feed the line.
//
// The cell is a sequential consume pair whose outbound is A's market. The
// line's cart is counted out; the changeover to B takes it off and back into
// the market with no count, as every changeover leg releases; the changeover
// back to A fetches each position an A carrier from that market. A carrier
// counted to nothing is no full of its part, so the fetch takes a counted one,
// the release onto that position goes, and the cart at 0 stands where the
// first changeover left it, taken by nothing. Before the fix the standby
// position was brought the cart at 0, and the pulling side's release was
// refused until the changeover timed out: "the line's parts would land on the
// carrier still on KSR-LINE: KSR-LINE holds no material to feed the line".
func TestScenario_ACarrierCountedToNothingIsNoFull(t *testing.T) {
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSequential, plain: true, pair: true})
	// The Edge's inventory sink, as a plant runs it: a landing binds the seat
	// and its count, which the release guard reads (the test Edge leaves it
	// unwired).
	c.edge.Engine.SetInventoryDeltaSink(edgeuop.New(c.edge.DB, "edge.test", c.edge.DB, c.edge.DB))
	c.outboundIsMarketA()
	out := c.countOut(ksrLine)

	before := c.lastID()
	if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", ""); err != nil {
		t.Fatalf("changeover to B: %v", err)
	}
	c.tick()
	c.runChangeoverTo("to B", before, c.styleB, c.typeB, ksrLine, ksrPair)
	gone, err := c.core.eng.DB().GetBin(out.ID)
	mustNil(t, err, "the counted-out bin")
	inA := false
	if gone.NodeID != nil {
		slot, err := c.core.eng.DB().GetNode(*gone.NodeID)
		mustNil(t, err, "the counted-out bin's node")
		inA = slot.ParentID != nil && *slot.ParentID == c.mktA.ID
	}
	if !inA || gone.PayloadCode != ksrPartA || gone.UOPRemaining != 0 {
		c.dump("to B")
		t.Fatalf("NOT REACHED: the counted-out cart is at %s with %q at %d, want back in %s still marked %s at 0",
			c.nodeName(gone.NodeID), gone.PayloadCode, gone.UOPRemaining, ksrMktA, ksrPartA)
	}

	before = c.lastID()
	if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleA, "ksr", ""); err != nil {
		t.Fatalf("changeover back to A: %v", err)
	}
	c.tick()
	c.runChangeoverTo("back to A", before, c.styleA, c.typeA, ksrLine, ksrPair)

	coreDB := c.core.eng.DB()
	for _, o := range c.newRows(before) {
		held, err := coreDB.ListReservationsByOrder(c.coreOf(o).ID)
		mustNil(t, err, "reservations")
		if b := c.coreOf(o).BinID; b != nil && *b == out.ID {
			t.Errorf("order %d (%s) took the cart counted to nothing", o.ID, o.OrderType)
		}
		for _, r := range held {
			if r.BinID == out.ID {
				t.Errorf("order %d (%s) held the cart counted to nothing", o.ID, o.OrderType)
			}
		}
	}
	bins, err := coreDB.ListBins()
	mustNil(t, err, "bins")
	for _, b := range bins {
		switch at := c.nodeName(b.NodeID); {
		case b.ID == out.ID && at != c.nodeName(gone.NodeID):
			t.Errorf("the cart counted to nothing moved from %s to %s", c.nodeName(gone.NodeID), at)
		case (at == ksrLine || at == ksrPair) && (b.PayloadCode != ksrPartA || b.UOPRemaining <= 0):
			c.dump("back to A")
			t.Errorf("%s holds %s with %q at %d, want a counted full of %s", at, b.Label, b.PayloadCode,
				b.UOPRemaining, ksrPartA)
		}
	}
}
