// SEND BACK on a produce tote press, carried end to end across the two
// modules: the operator sends the cart on the press's outside position back,
// tops & bottoms of the press's tote land in its place, and the next swap
// indexes them onto the press.
//
// The plain press cell (keep_staged_recovery_test.go's ksrCell), its claims
// mirrored at Core (the pull's fed press and the fed shape are read there),
// with tote numbers: the part's carts are 5 layers of 8 totes, the market's
// empties full stacks, a cart 3 high on the outside and tops & bottoms in the
// market. The outside position has a process node row on the Edge, where SEND
// BACK's orders are made.
//
//go:build docker

package scenarios

import (
	"strings"
	"testing"

	"shingo/protocol"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store/processes"
	edgeuop "shingoedge/uop"
)

func TestScenario_SendBack_TheCartGoesAndTopsAndBottomsComeThenTheSwapIndexesThem(t *testing.T) {
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleProduce, mode: protocol.SwapModeTwoRobotPressIndex,
		plain: true, mirror: true})
	// The landing binds through the seat's count writer, which the harness
	// leaves unwired: wired here, so a landing at the claimless outside binds as
	// it does in the plant (and the not-bound alarm below means what it says).
	c.edge.Engine.SetInventoryDeltaSink(edgeuop.New(c.edge.DB, "edge.test", c.edge.DB, c.edge.DB))
	coreDB := c.core.eng.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := coreDB.Exec(q, args...)
		mustNil(t, err, q)
	}
	// The tote numbers, and the carts at heights.
	exec(`UPDATE payloads SET tote_layers = 5, uop_per_tote = 8 WHERE code = $1`, ksrPartA)
	exec(`UPDATE bin_types SET totes_per_layer = 8 WHERE id = $1`, c.typeA.ID)
	exec(`UPDATE bins SET totes_on_cart = 40 WHERE bin_type_id = $1`, c.typeA.ID)
	deck, err := coreDB.GetNodeByDotName(ksrDeck)
	mustNil(t, err, "the outside")
	var cart, tb int64
	mustNil(t, coreDB.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
		VALUES ($1, 'KSR-SB-CART', $2, 'available', 24) RETURNING id`, c.typeA.ID, deck.ID).Scan(&cart), "a cart 3 high outside")
	var freeSlot int64
	mustNil(t, coreDB.QueryRow(`SELECT n.id FROM nodes n WHERE n.parent_id = $1
		AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id) ORDER BY n.name LIMIT 1`, c.mktA.ID).Scan(&freeSlot),
		"a free market slot")
	mustNil(t, coreDB.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
		VALUES ($1, 'KSR-SB-TB', $2, 'available', 0) RETURNING id`, c.typeA.ID, freeSlot).Scan(&tb), "tops & bottoms in the market")
	// The outside's process node, where SEND BACK's orders go.
	outsideID, err := c.edge.DB.CreateProcessNode(processes.NodeInput{ProcessID: c.processID, CoreNodeName: ksrDeck,
		Code: "KSR9", Name: ksrDeck, Sequence: 9, Enabled: true})
	mustNil(t, err, "the outside's process node")
	// As a node edit does in the plant (the spec-change hook), the claims are
	// published again, so Core's mirror names the outside a position of this
	// station and addresses its landings here.
	mustNil(t, edgemessaging.NewPlantClaimsPublisher(c.edge.DB, "edge.test", 0).PublishChanged(c.processID),
		"publish the plant claims")
	c.bus.PushBins()
	c.settle()
	var notBound []string
	c.edge.Engine.Events.SubscribeTypes(func(evt edgeengine.Event) {
		if a, ok := evt.Payload.(edgeengine.DeliveredNotBoundEvent); ok {
			notBound = append(notBound, a.CoreNodeName)
		}
	}, edgeengine.EventDeliveredNotBound)

	// SEND BACK.
	res, err := c.edge.Engine.SendBack(c.nodeID)
	if err != nil {
		c.dump("SEND BACK refused")
		t.Fatalf("SEND BACK: %v", err)
	}
	if *res.Move.ProcessNodeID != outsideID || *res.Pull.ProcessNodeID != outsideID {
		t.Errorf("SEND BACK's orders are on %v and %v, want the outside's node %d",
			*res.Move.ProcessNodeID, *res.Pull.ProcessNodeID, outsideID)
	}
	c.eventually("SEND BACK: the cart off and tops & bottoms on the outside", func() bool {
		c.fleetStep()
		m, p := c.edgeRow(res.Move.ID), c.edgeRow(res.Pull.ID)
		return protocol.IsTerminal(m.Status) && protocol.IsTerminal(p.Status)
	})
	for _, o := range []*domain.Order{res.Move, res.Pull} {
		if s := c.coreOf(*o).Status; s != protocol.StatusConfirmed {
			t.Errorf("SEND BACK's %s ended %s at Core, want confirmed (auto)", o.OrderType, s)
		}
	}
	at := func(id int64) string {
		b, err := coreDB.GetBin(id)
		mustNil(t, err, "bin")
		return c.nodeName(b.NodeID)
	}
	if n := at(cart); !strings.HasPrefix(n, ksrDest) {
		t.Errorf("the outside cart is at %q, want the outbound %s", n, ksrDest)
	}
	if n := at(tb); n != ksrDeck {
		t.Fatalf("tops & bottoms is at %q, want the outside %s", n, ksrDeck)
	}
	if len(notBound) != 0 {
		t.Errorf("deliveries not bound at %v; the pull to a claimless outside binds by its landing", notBound)
	}

	// The next swap indexes tops & bottoms onto the press.
	mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "a count")
	c.swap("the swap after SEND BACK", func() {
		_, err := c.edge.Engine.RequestProduceSwap(c.nodeID, 0)
		mustNil(t, err, "REQUEST SWAP")
	})
	if n := at(tb); n != ksrLine {
		t.Errorf("tops & bottoms is at %q after the swap, want the press %s", n, ksrLine)
	}
}
