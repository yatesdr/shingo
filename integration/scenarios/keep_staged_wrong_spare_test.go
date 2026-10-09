// A wrong spare that lands on a keep-staged spot after the last decision, end
// to end: the state it leaves, and the operator's way out of it.
//
// The call found the spot bare, so its swap fetches from the market and stages
// its carrier on the spot. A wrong part then lands there by hand. Nothing on the
// Edge moves it: the keeper orders nothing onto an occupied spot, and Core holds
// the swap at its staging stop, as it holds any order whose staging node is
// occupied. The exit is a person moving the wrong bin off the spot at Core; the
// held swap then goes, fetching its carrier from the market.
//
//go:build docker

package scenarios

import (
	"strings"
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

func TestScenario_KeepStagedWrongSpare_APersonsMoveIsTheExit(t *testing.T) {
	core := startKeepStagedCore(t)
	coreDB, sim := core.eng.DB(), core.sim
	const (
		line, spot, market, dest, aside = "KSW-LINE", "KSW-SPOT", "KSW-MKT", "KSW-DEST", "KSW-ASIDE"
		partA, partC                    = "KSW-PA", "KSW-PC"
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
	node(&corenodes.Node{Name: aside})
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

	// ── the call, against a dry market: the swap fetches from the market, no refill ──
	_, err = edge.Engine.RequestNodeMaterial(nodeID, 1)
	mustNil(t, err, "call")
	settle()
	legs, refills, _ := rows()
	if len(legs) != 2 || len(refills) != 0 {
		t.Fatalf("call made legs=%d refills=%d, want 2 and 0 (a request orders nothing for the spot)", len(legs), len(refills))
	}

	// ── after that decision, a wrong part lands on the spot, then stock of A ──
	wrongBin := full("KSW-WRONG", partC, spotNode)
	full("KSW-A1", partA, slots[0])
	full("KSW-A2", partA, slots[1])
	core.eng.RunFulfillmentScan()
	settle()

	// THE STATE. What the station and Core's order page say about it.
	for _, o := range legs {
		c, e := coreOf(o), edgeRow(o)
		t.Logf("WEDGE %s %s->%s %s | Core: status=%s cause=%q reason=%q | Edge: status=%s reason=%q code=%q",
			o.OrderType, o.SourceNode, o.DeliveryNode, o.PayloadCode,
			c.Status, c.QueueCause, c.QueueReason, e.Status, e.QueueReason, e.QueueCode)
	}
	for _, o := range legs {
		if c := coreOf(o); c.VendorOrderID != "" {
			t.Fatalf("Core sent %s %d to the fleet with a wrong bin on its staging spot", o.OrderType, o.ID)
		}
	}
	// Nothing on the Edge moves the wrong bin or orders onto the spot it holds.
	mustNil(t, edge.Engine.ResumeKeepStaged(nodeID), "run the keeper")
	settle()
	if _, refills, returns := rows(); len(refills) != 0 || len(returns) != 0 {
		t.Fatalf("the keeper ordered refills=%d returns=%d with the wrong bin standing, want none", len(refills), len(returns))
	}

	// THE EXIT: a person moves the wrong bin off the spot at Core. The
	// manual-order door builds this request and hands it to this intake.
	moveReq := &protocol.OrderRequest{
		OrderUUID: "ksw-person-move", OrderType: protocol.OrderTypeMove, SourceNode: spot, DeliveryNode: aside,
		Quantity: 1, OriginClass: protocol.OriginClassNoDemand,
	}
	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest,
		protocol.Address{Role: protocol.RoleCore, Station: "core-operator"}, protocol.Address{Role: protocol.RoleCore}, moveReq)
	mustNil(t, err, "envelope")
	core.eng.Dispatcher().HandleOrderRequest(env, moveReq)
	var move *coreorders.Order
	eventually("the person's move to go to the fleet", func() bool {
		m, err := coreDB.GetOrderByUUID("ksw-person-move")
		if err != nil || m.VendorOrderID == "" {
			return false
		}
		move = m
		return true
	})
	if move.BinID == nil || *move.BinID != wrongBin.ID {
		t.Fatalf("the person's move holds bin %v, want the wrong spare %d", move.BinID, wrongBin.ID)
	}
	sim.DriveSimpleLifecycle(move.VendorOrderID)

	// The held swap goes once the spot has cleared, its carrier from the market.
	var supply *coreorders.Order
	eventually("the held swap to go once the spot clears", func() bool {
		core.eng.RunFulfillmentScan()
		for _, l := range legs {
			if c := coreOf(l); c.VendorOrderID != "" && strings.HasPrefix(c.SourceNode, market+"-") {
				supply = c
				return true
			}
		}
		return false
	})
	t.Logf("EXIT: move %d cleared %s, swap leg %d went from %s", move.ID, spot, supply.ID, supply.SourceNode)
}
