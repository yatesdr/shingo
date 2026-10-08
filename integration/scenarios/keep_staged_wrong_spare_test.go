// A wrong spare that lands on a keep-staged spot after the last decision, end
// to end: the state it leaves, and the operator's way out of it.
//
// No decision point re-judges a spot whose spare changes after the last
// reconcile. A consume cell's swap then waits at Core for the right part, the
// refill waits behind the wrong bin, and REQUEST is refused while the swap is in
// flight. That is a state that needs a person. The exit is to cancel the swap
// and REQUEST again: the cancel is an order of the line ending, so the keeper
// re-reads the spot and sends the wrong spare back, the refill lands, and the
// new swap takes it.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"

	"shingoedge/domain"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

func TestScenario_KeepStagedWrongSpare_CancelAndRequestIsTheExit(t *testing.T) {
	core := startKeepStagedCore(t)
	coreDB, sim := core.eng.DB(), core.sim
	const (
		line, spot, market, dest = "KSW-LINE", "KSW-SPOT", "KSW-MKT", "KSW-DEST"
		partA, partC             = "KSW-PA", "KSW-PC"
	)

	// ── Core: the cell, its spot, a market of one-bin slots, no stock of A yet ──
	stor, err := coreDB.GetNodeTypeByCode("STOR")
	if err != nil {
		stor = &corenodes.NodeType{Code: "STOR", Name: "Storage Slot"}
		mustNil(t, coreDB.CreateNodeType(stor), "STOR type")
	}
	ngrp, err := coreDB.GetNodeTypeByCode("NGRP")
	mustNil(t, err, "NGRP type")
	node := func(n *corenodes.Node) *corenodes.Node {
		n.Enabled = true
		mustNil(t, coreDB.CreateNode(n), "node "+n.Name)
		return n
	}
	lineNode, spotNode := node(&corenodes.Node{Name: line}), node(&corenodes.Node{Name: spot})
	node(&corenodes.Node{Name: dest})
	mkt := node(&corenodes.Node{Name: market, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	var slots []*corenodes.Node
	for i := 1; i <= 6; i++ {
		slots = append(slots, node(&corenodes.Node{
			Name: market + "-" + string(rune('0'+i)), ParentID: &mkt.ID, NodeTypeID: &stor.ID,
		}))
	}
	tote := &corebins.BinType{Code: "KSW-TOTE"}
	mustNil(t, coreDB.CreateBinType(tote), "bin type")
	for _, code := range []string{partA, partC} {
		pl := &corepayloads.Payload{Code: code, UOPCapacity: 40}
		mustNil(t, coreDB.CreatePayload(pl), "payload "+code)
		mustNil(t, coreDB.SetPayloadBinTypes(pl.ID, []int64{tote.ID}), "carrier rule "+code)
	}
	full := func(label, payload string, at *corenodes.Node) *corebins.Bin {
		b := &corebins.Bin{BinTypeID: tote.ID, Label: label, NodeID: &at.ID, Status: "available"}
		mustNil(t, coreDB.CreateBin(b), "bin "+label)
		mustNil(t, coreDB.SetBinManifest(b.ID, `{"items":[]}`, payload, 40), "manifest "+label)
		mustNil(t, coreDB.ConfirmBinManifest(b.ID, ""), "confirm "+label)
		return b
	}
	full("KSW-LINE-BIN", partA, lineNode)

	// ── Edge: one keep-staged two-robot consume claim ──
	edge := edgeharness.NewEdgeWithCoreAPI(t, "edge.test", core.url)
	processID, err := edge.DB.CreateProcess("KSW-PROC", "", "", "", false)
	mustNil(t, err, "process")
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: line, Code: "KSW1", Name: line, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "process node")
	_, err = edge.DB.EnsureProcessNodeRuntime(nodeID)
	mustNil(t, err, "runtime")
	styleID, err := edge.DB.CreateStyle("KSW-A", "", processID)
	mustNil(t, err, "style")
	mustNil(t, edge.DB.SetActiveStyle(processID, &styleID), "active style")
	claimID, err := edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: line, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: partA, UOPCapacity: 40, InboundSource: market, InboundStaging: spot, OutboundDestination: dest,
		KeepStagedNode: domain.Ptr(spot),
	})
	mustNil(t, err, "claim")
	mustNil(t, edge.DB.SetProcessNodeRuntime(nodeID, &claimID, 30), "runtime claim")

	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", core.eng.Dispatcher())
	coreIngestor := protocol.NewIngestor(nil)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeOrderRequest, coreHandler.HandleOrderRequest)
	router.Register(coreRouter, protocol.TypeComplexOrderRequest, coreHandler.HandleComplexOrderRequest)
	router.Register(coreRouter, protocol.TypeOrderCancel, coreHandler.HandleOrderCancel)
	router.Register(coreRouter, protocol.TypeOrderReceipt, coreHandler.HandleOrderReceipt)
	router.Register(coreRouter, protocol.TypeOrderRelease, coreHandler.HandleOrderRelease)
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }
	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
	)
	settle := func() {
		for i := 0; i < 40; i++ {
			if bus.PumpAll() == 0 {
				time.Sleep(50 * time.Millisecond)
				if bus.PumpAll() == 0 {
					return
				}
			}
		}
	}
	coreOf := func(o domain.Order) *coreorders.Order {
		t.Helper()
		c, err := coreDB.GetOrderByUUID(o.UUID)
		mustNil(t, err, "core order for "+o.UUID)
		return c
	}
	edgeRow := func(o domain.Order) *domain.Order {
		t.Helper()
		e, err := edge.DB.GetOrder(o.ID)
		mustNil(t, err, "edge order")
		return e
	}
	rows := func() (legs, refills, returns []domain.Order) {
		all, err := edge.DB.ListOrdersByProcess(processID)
		mustNil(t, err, "edge rows")
		for _, o := range all {
			if protocol.IsTerminal(o.Status) {
				continue
			}
			switch {
			case o.OrderType == protocol.OrderTypeComplex:
				legs = append(legs, o)
			case o.DeliveryNode == spot:
				refills = append(refills, o)
			case o.SourceNode == spot:
				returns = append(returns, o)
			}
		}
		return
	}
	// eventually polls Core's state, pumping the bus, up to a few seconds.
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 60; i++ {
			settle()
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for: %s", what)
	}
	settle()

	// ── the call, against a dry market: the swap and one refill wait (one in flight at a time) ──
	_, err = edge.Engine.RequestNodeMaterial(nodeID, 1)
	mustNil(t, err, "call")
	settle()
	legs, refills, _ := rows()
	if len(legs) != 2 || len(refills) != 1 {
		t.Fatalf("call made legs=%d refills=%d, want 2 and 1 (bare spot, one refill in flight)", len(legs), len(refills))
	}

	// ── after that decision, a wrong part lands on the spot, then stock of A ──
	full("KSW-WRONG", partC, spotNode)
	full("KSW-A1", partA, slots[0])
	full("KSW-A2", partA, slots[1])
	core.eng.RunFulfillmentScan()
	settle()

	// THE STATE. What the station and Core's order page say about it.
	for _, o := range append(append([]domain.Order{}, legs...), refills...) {
		c, e := coreOf(o), edgeRow(o)
		t.Logf("WEDGE %s %s->%s %s | Core: status=%s cause=%q reason=%q | Edge: status=%s reason=%q code=%q",
			o.OrderType, o.SourceNode, o.DeliveryNode, o.PayloadCode,
			c.Status, c.QueueCause, c.QueueReason, e.Status, e.QueueReason, e.QueueCode)
		if c.VendorOrderID != "" {
			t.Errorf("%s %d went to the fleet with a wrong part on the spot", o.OrderType, o.ID)
		}
	}
	// The station reads Core's CURRENT wait for each refill. Each refill entered
	// the queue waiting for material in a dry market; it now waits for its slot
	// behind the wrong part, and the station says so.
	for _, o := range refills {
		if c, e := coreOf(o), edgeRow(o); e.QueueReason != c.QueueReason {
			t.Errorf("refill %d: the station reads %q, Core %q", o.ID, e.QueueReason, c.QueueReason)
		}
	}
	// And a wait that does not change sends nothing more, pass after pass.
	updates := func() int {
		var n int
		mustNil(t, core.eng.DB().DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = $1`,
			protocol.TypeOrderUpdate).Scan(&n), "count order updates")
		return n
	}
	before := updates()
	for i := 0; i < 5; i++ {
		core.eng.RunFulfillmentScan()
		settle()
	}
	if n := updates() - before; n != 0 {
		t.Errorf("five scan passes with nothing changed sent %d order updates, want 0", n)
	}
	if _, err := edge.Engine.RequestNodeMaterial(nodeID, 1); err == nil {
		t.Fatal("REQUEST was accepted with the swap in flight; the state is supposed to need the cancel")
	}

	// ── THE EXIT: cancel the swap, then REQUEST ──
	for _, leg := range legs {
		if e := edgeRow(leg); !protocol.IsTerminal(e.Status) {
			mustNil(t, edge.Engine.OrderManager().AbortOrder(leg.ID), "cancel swap leg")
		}
	}
	settle()
	_, err = edge.Engine.RequestNodeMaterial(nodeID, 1)
	mustNil(t, err, "REQUEST after the cancel")
	settle()
	legs2, refills2, returns := rows()
	if len(returns) != 1 || returns[0].PayloadCode != partC || returns[0].DeliveryNode != market {
		t.Fatalf("returns = %+v, want one sending the %s spare back to %s carrying it", returns, partC, market)
	}
	var newLegs []domain.Order
	for _, l := range legs2 {
		if l.ID > legs[len(legs)-1].ID {
			newLegs = append(newLegs, l)
		}
	}
	if len(newLegs) != 2 {
		t.Fatalf("the REQUEST made %d swap legs, want 2", len(newLegs))
	}
	// One refill is already coming, and at most one is in flight, so neither the
	// keeper's return nor the REQUEST adds another.
	for _, r := range refills2 {
		if r.ID > returns[0].ID {
			t.Errorf("refill %d written after the return: the one already coming should have sufficed", r.ID)
		}
	}
	if len(refills2) != 1 {
		t.Fatalf("refills coming = %d, want the 1 from the call", len(refills2))
	}

	// The return goes, a refill lands, the new swap takes the right spare.
	eventually("the return to go to the fleet", func() bool { return coreOf(returns[0]).VendorOrderID != "" })
	ret := coreOf(returns[0])
	sim.DriveSimpleLifecycle(ret.VendorOrderID)
	var landed *coreorders.Order
	eventually("a refill to go to the fleet once the spot clears", func() bool {
		for _, r := range refills2 {
			if c := coreOf(r); c.VendorOrderID != "" {
				landed = c
				return true
			}
		}
		return false
	})
	sim.DriveSimpleLifecycle(landed.VendorOrderID)
	var supply *coreorders.Order
	eventually("the new swap to take the right spare", func() bool {
		for _, l := range newLegs {
			c := coreOf(l)
			if c.VendorOrderID != "" && c.BinID != nil && landed.BinID != nil && *c.BinID == *landed.BinID {
				supply = c
				return true
			}
		}
		return false
	})
	t.Logf("EXIT: return %d, refill %d landed bin %d, new swap leg %d took it", ret.ID, landed.ID, *landed.BinID, supply.ID)
}
