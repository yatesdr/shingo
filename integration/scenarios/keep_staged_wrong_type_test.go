// A wrong-type empty put on a produce keep-staged spot by hand, end to end:
// the state it leaves and the person's way out of it.
//
// The Edge did not order this bin, so no landing kick sees it, and the Edge
// reads any unstamped empty as right for a produce claim, so no reconcile sends
// it back. Core refuses it for the swap, and the refill waits behind it. The
// exit is a Core manual move of the empty off the spot; the refill then lands a
// right-type empty and the swap takes it.
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

func TestScenario_KeepStagedWrongTypeEmpty_CoreMoveIsTheExit(t *testing.T) {
	core := startKeepStagedCore(t)
	coreDB, sim := core.eng.DB(), core.sim
	const (
		line, spot, market, dest, empties = "KSE-LINE", "KSE-SPOT", "KSE-MKT", "KSE-DEST", "KSE-EMPTIES"
		part                              = "KSE-PB"
	)
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
	node(&corenodes.Node{Name: empties})
	mkt := node(&corenodes.Node{Name: market, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	slot := node(&corenodes.Node{Name: market + "-1", ParentID: &mkt.ID, NodeTypeID: &stor.ID})
	node(&corenodes.Node{Name: market + "-2", ParentID: &mkt.ID, NodeTypeID: &stor.ID})
	right := &corebins.BinType{Code: "KSE-RIGHT"}
	wrong := &corebins.BinType{Code: "KSE-WRONG"}
	for _, bt := range []*corebins.BinType{right, wrong} {
		mustNil(t, coreDB.CreateBinType(bt), "bin type "+bt.Code)
	}
	pl := &corepayloads.Payload{Code: part, UOPCapacity: 40}
	mustNil(t, coreDB.CreatePayload(pl), "payload")
	mustNil(t, coreDB.SetPayloadBinTypes(pl.ID, []int64{right.ID}), "carrier rule")
	lineBin := &corebins.Bin{BinTypeID: right.ID, Label: "KSE-LINE-BIN", NodeID: &lineNode.ID, Status: "staged"}
	mustNil(t, coreDB.CreateBin(lineBin), "line bin")
	mustNil(t, coreDB.SetBinManifest(lineBin.ID, `{"items":[]}`, part, 40), "line manifest")
	mustNil(t, coreDB.ConfirmBinManifest(lineBin.ID, ""), "line confirm")
	wrongEmpty := &corebins.Bin{BinTypeID: wrong.ID, Label: "KSE-WRONG-EMPTY", NodeID: &spotNode.ID, Status: "available"}
	mustNil(t, coreDB.CreateBin(wrongEmpty), "the wrong empty, by hand")
	rightEmpty := &corebins.Bin{BinTypeID: right.ID, Label: "KSE-RIGHT-EMPTY", NodeID: &slot.ID, Status: "available"}
	mustNil(t, coreDB.CreateBin(rightEmpty), "a right empty in the market")

	edge := edgeharness.NewEdgeWithCoreAPI(t, "edge.test", core.url)
	processID, err := edge.DB.CreateProcess("KSE-PROC", "", "", "", false)
	mustNil(t, err, "process")
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: line, Code: "KSE1", Name: line, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "process node")
	_, err = edge.DB.EnsureProcessNodeRuntime(nodeID)
	mustNil(t, err, "runtime")
	styleID, err := edge.DB.CreateStyle("KSE-A", "", processID)
	mustNil(t, err, "style")
	mustNil(t, edge.DB.SetActiveStyle(processID, &styleID), "active style")
	claimID, err := edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: line, Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: part, UOPCapacity: 40, InboundSource: market, InboundStaging: spot, OutboundDestination: dest,
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

	_, err = edge.Engine.RequestProduceSwap(nodeID)
	mustNil(t, err, "the produce call")
	settle()
	all, err := edge.DB.ListOrdersByProcess(processID)
	mustNil(t, err, "edge rows")
	var legs, refills []domain.Order
	for _, o := range all {
		switch {
		case o.OrderType == protocol.OrderTypeComplex:
			legs = append(legs, o)
		case o.DeliveryNode == spot:
			refills = append(refills, o)
		}
	}
	if len(legs) != 2 || len(refills) != 1 {
		t.Fatalf("the call made legs=%d refills=%d, want 2 and 1: Edge reads the wrong empty as a standing spare",
			len(legs), len(refills))
	}

	// THE STATE. What the station and Core's order page say about it.
	core.eng.RunFulfillmentScan()
	settle()
	for _, o := range append(append([]domain.Order{}, legs...), refills...) {
		c := coreOf(o)
		e, err := edge.DB.GetOrder(o.ID)
		mustNil(t, err, "edge order")
		t.Logf("WEDGE %s %s->%s %s | Core: status=%s cause=%q reason=%q | Edge: status=%s reason=%q code=%q",
			o.OrderType, o.SourceNode, o.DeliveryNode, o.PayloadCode,
			c.Status, c.QueueCause, c.QueueReason, e.Status, e.QueueReason, e.QueueCode)
		if c.VendorOrderID != "" && c.BinID != nil && *c.BinID == wrongEmpty.ID {
			t.Fatalf("Core sent %s %d to the fleet holding the wrong-type empty", o.OrderType, o.ID)
		}
	}
	if c := coreOf(refills[0]); c.VendorOrderID != "" {
		t.Fatalf("the refill went to the fleet with the wrong empty still on the spot (status %s)", c.Status)
	}

	// THE EXIT: a person moves the wrong empty off the spot at Core. The
	// manual-order door builds this request and hands it to this intake.
	moveReq := &protocol.OrderRequest{
		OrderUUID: "kse-person-move", OrderType: protocol.OrderTypeMove, SourceNode: spot, DeliveryNode: empties,
		Quantity: 1, OriginClass: protocol.OriginClassNoDemand,
	}
	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest,
		protocol.Address{Role: protocol.RoleCore, Station: "core-operator"}, protocol.Address{Role: protocol.RoleCore}, moveReq)
	mustNil(t, err, "envelope")
	core.eng.Dispatcher().HandleOrderRequest(env, moveReq)
	var move *coreorders.Order
	eventually("the person's move to go to the fleet", func() bool {
		m, err := coreDB.GetOrderByUUID("kse-person-move")
		if err != nil || m.VendorOrderID == "" {
			return false
		}
		move = m
		return true
	})
	if move.BinID == nil || *move.BinID != wrongEmpty.ID {
		t.Fatalf("the person's move holds bin %v, want the wrong empty %d", move.BinID, wrongEmpty.ID)
	}
	sim.DriveSimpleLifecycle(move.VendorOrderID)
	var landed *coreorders.Order
	eventually("the refill to go once the spot clears", func() bool {
		if c := coreOf(refills[0]); c.VendorOrderID != "" {
			landed = c
			return true
		}
		return false
	})
	if landed.BinID == nil || *landed.BinID != rightEmpty.ID {
		t.Fatalf("the refill holds bin %v, want the right-type empty %d", landed.BinID, rightEmpty.ID)
	}
	sim.DriveSimpleLifecycle(landed.VendorOrderID)
	eventually("the swap to take the right empty", func() bool {
		for _, l := range legs {
			if c := coreOf(l); c.VendorOrderID != "" && c.BinID != nil && *c.BinID == rightEmpty.ID {
				return true
			}
		}
		return false
	})
}
