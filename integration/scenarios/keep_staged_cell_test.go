// A keep-staged two-robot consume cell, end to end across the two modules:
// a call, a payload changeover and the changeover's cancel.
//
// Core is the real engine (fleet simulator) behind its real HTTP router, so the
// spot reads Edge makes on each decision are Core's own answer about the spot.
// The orders Edge decides travel the Bus to Core's real handler, and Core's
// replies travel back. The Edge engine tests count what each decision creates;
// this case asserts that Core takes each of them for what it is: the short
// swap's spot pickup lifts the spare, a refill and a return are plain orders,
// and the cancel reaches the ones not yet with the fleet.
//
//go:build docker

package scenarios

import (
	"net/http/httptest"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	"shingo/protocol/debuglog"
	coreconfig "shingocore/config"
	coreengine "shingocore/engine"
	"shingocore/fleet/simulator"
	coremessaging "shingocore/messaging"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"
	"shingocore/www"

	"shingoedge/domain"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	edgeharness "shingoedge/testharness"
)

const (
	kscLine   = "KSC-LINE"
	kscSpot   = "KSC-SPOT"
	kscMarket = "KSC-MKT"
	kscDest   = "KSC-DEST"
	kscPartA  = "KSC-PA"
	kscPartB  = "KSC-PB"
)

type kscCore struct {
	eng *coreengine.Engine
	sim *simulator.SimulatorBackend
	url string
}

func startKeepStagedCore(t *testing.T) kscCore {
	t.Helper()
	db := coreharness.OpenDB(t)
	sim := simulator.New()
	cfg := coreconfig.Defaults()
	cfg.Messaging.StationID = "core"
	eng := coreengine.New(coreengine.Config{
		AppConfig: cfg, DB: db, Fleet: sim, LogFunc: t.Logf,
	})
	eng.Start()
	t.Cleanup(eng.Stop)
	dbg, err := debuglog.New(64, nil)
	mustNil(t, err, "debuglog")
	r, stop, err := www.NewRouter(eng, dbg)
	mustNil(t, err, "core router")
	t.Cleanup(stop)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return kscCore{eng: eng, sim: sim, url: srv.URL}
}

func TestScenario_KeepStagedCell_CallChangeoverCancel(t *testing.T) {
	core := startKeepStagedCore(t)
	coreDB, sim := core.eng.DB(), core.sim

	// ── Core: the cell, its spot and a market of one-bin slots ──
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
	line, spot := node(&corenodes.Node{Name: kscLine}), node(&corenodes.Node{Name: kscSpot})
	node(&corenodes.Node{Name: kscDest})
	market := node(&corenodes.Node{Name: kscMarket, IsSynthetic: true, NodeTypeID: &ngrp.ID})
	var slots []*corenodes.Node
	for i := 1; i <= 8; i++ {
		slots = append(slots, node(&corenodes.Node{
			Name: kscMarket + "-" + string(rune('0'+i)), ParentID: &market.ID, NodeTypeID: &stor.ID,
		}))
	}
	tote := &corebins.BinType{Code: "KSC-TOTE"}
	mustNil(t, coreDB.CreateBinType(tote), "bin type")
	for _, code := range []string{kscPartA, kscPartB} {
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
	lineBin := full("KSC-LINE-BIN", kscPartA, line)
	spare := full("KSC-SPARE", kscPartA, spot)
	full("KSC-A1", kscPartA, slots[0])
	full("KSC-A2", kscPartA, slots[1])
	full("KSC-B1", kscPartB, slots[2])
	full("KSC-B2", kscPartB, slots[3])
	full("KSC-B3", kscPartB, slots[4])

	// ── Edge: one line node, two styles keeping the same spot ──
	edge := edgeharness.NewEdgeWithCoreAPI(t, "edge.test", core.url)
	processID, err := edge.DB.CreateProcess("KSC-PROC", "keep-staged cell", "active_production", "", "", false)
	mustNil(t, err, "process")
	stationID, err := edge.DB.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "KSC-ST", Name: "KSC Station", Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "station")
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID, CoreNodeName: kscLine,
		Code: "KSC1", Name: kscLine, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "process node")
	_, err = edge.DB.EnsureProcessNodeRuntime(nodeID)
	mustNil(t, err, "runtime")
	styleA, err := edge.DB.CreateStyle("KSC-A", "", processID)
	mustNil(t, err, "style A")
	styleB, err := edge.DB.CreateStyle("KSC-B", "", processID)
	mustNil(t, err, "style B")
	mustNil(t, edge.DB.SetActiveStyle(processID, &styleA), "active style")
	var claimA int64
	for _, s := range []struct {
		style int64
		part  string
	}{{styleA, kscPartA}, {styleB, kscPartB}} {
		id, err := edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: s.style, CoreNodeName: kscLine, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeTwoRobot, PayloadCode: s.part, UOPCapacity: 40,
			InboundSource: kscMarket, InboundStaging: kscSpot, OutboundDestination: kscDest,
			KeepStaged: domain.Ptr(true),
		})
		mustNil(t, err, "claim "+s.part)
		if s.style == styleA {
			claimA = id
		}
	}
	mustNil(t, edge.DB.SetProcessNodeRuntime(nodeID, &claimA, 30), "runtime claim")

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
		for i := 0; i < 20; i++ {
			if bus.PumpAll() == 0 {
				time.Sleep(50 * time.Millisecond)
				if bus.PumpAll() == 0 {
					return
				}
			}
		}
	}
	settle()

	// step sorts what one step made — the Edge rows newer than the last step —
	// into the swap's legs, the spot's refills and its returns.
	type made struct {
		legs, refills, returns []domain.Order
	}
	var seen int64
	step := func() made {
		rows, err := edge.DB.ListOrdersByProcess(processID)
		mustNil(t, err, "edge rows")
		var m made
		top := seen
		for _, o := range rows {
			if o.ID <= seen {
				continue
			}
			top = max(top, o.ID)
			switch {
			case o.OrderType == protocol.OrderTypeComplex:
				m.legs = append(m.legs, o)
			case o.OrderType == protocol.OrderTypeRetrieve && o.DeliveryNode == kscSpot:
				m.refills = append(m.refills, o)
			case o.OrderType == protocol.OrderTypeMove && o.SourceNode == kscSpot:
				m.returns = append(m.returns, o)
			default:
				t.Errorf("unexpected order %d: %s %s->%s", o.ID, o.OrderType, o.SourceNode, o.DeliveryNode)
			}
		}
		seen = top
		return m
	}
	coreOf := func(o domain.Order) *coreorders.Order {
		t.Helper()
		c, err := coreDB.GetOrderByUUID(o.UUID)
		mustNil(t, err, "core order for edge "+o.UUID)
		return c
	}
	edgeStatus := func(o domain.Order) protocol.Status {
		t.Helper()
		e, err := edge.DB.GetOrder(o.ID)
		mustNil(t, err, "edge order")
		return e.Status
	}
	holds := func(c *coreorders.Order) int64 {
		if c.BinID == nil {
			return 0
		}
		return *c.BinID
	}

	// ── 1. The call: the short swap lifts the spare, one refill comes ──
	if _, err := edge.Engine.RequestNodeMaterial(nodeID, 1); err != nil {
		t.Fatalf("call: %v", err)
	}
	settle()
	call := step()
	if len(call.legs) != 2 || len(call.refills) != 1 || len(call.returns) != 0 {
		t.Fatalf("call made legs=%d refills=%d returns=%d, want 2, 1, 0",
			len(call.legs), len(call.refills), len(call.returns))
	}
	lifted := map[int64]bool{}
	for _, leg := range call.legs {
		c := coreOf(leg)
		if c.VendorOrderID == "" {
			t.Errorf("call leg %s: Core status %q (%s), want it with the fleet", leg.UUID, c.Status, c.QueueCause)
		}
		lifted[holds(c)] = true
	}
	if !lifted[spare.ID] || !lifted[lineBin.ID] {
		t.Fatalf("the call's legs hold bins %v, want the spare %d and the line's bin %d", lifted, spare.ID, lineBin.ID)
	}
	// The refill waits at Core while the spare stands: the spot takes one
	// landing at a time.
	if c := coreOf(call.refills[0]); c.VendorOrderID != "" || c.PayloadCode != kscPartA {
		t.Fatalf("call refill at Core: status %q payload %q vendor %q, want held for the spot, part %s",
			c.Status, c.PayloadCode, c.VendorOrderID, kscPartA)
	}

	// The call's robots are recalled at Core. The spot is left where a finished
	// call leaves it: one spare of the running part standing.
	callOrders := append(append([]domain.Order{}, call.legs...), call.refills...)
	for _, o := range callOrders {
		c := coreOf(o)
		if protocol.IsTerminal(c.Status) {
			continue // a leg's cancel takes its sibling with it
		}
		mustNil(t, core.eng.TerminateOrder(c.ID, "scenario"), "recall "+o.UUID)
	}
	settle()
	for _, o := range callOrders {
		if s := edgeStatus(o); s != protocol.StatusCancelled {
			t.Fatalf("recalled order %d reads %q at Edge, want cancelled", o.ID, s)
		}
	}
	step()

	// ── 2. The payload changeover: the spare goes back, the new part comes ──
	if _, err := edge.Engine.StartProcessChangeover(processID, styleB, "scenario", ""); err != nil {
		t.Fatalf("changeover: %v", err)
	}
	settle()
	co := step()
	if len(co.legs) != 2 || len(co.refills) != 2 || len(co.returns) != 1 {
		t.Fatalf("changeover made legs=%d refills=%d returns=%d, want 2, 2, 1",
			len(co.legs), len(co.refills), len(co.returns))
	}
	ret := co.returns[0]
	if c := coreOf(ret); holds(c) != spare.ID || c.VendorOrderID == "" || ret.DeliveryNode != kscMarket ||
		ret.PayloadCode != kscPartA {
		t.Fatalf("return: Core holds bin %d vendor %q, Edge to %q carrying %q; want the spare %d with the fleet, "+
			"to %s carrying %s", holds(c), c.VendorOrderID, ret.DeliveryNode, ret.PayloadCode, spare.ID, kscMarket, kscPartA)
	}
	for _, r := range co.refills {
		if c := coreOf(r); c.PayloadCode != kscPartB || c.VendorOrderID != "" {
			t.Fatalf("changeover refill %d at Core: payload %q vendor %q, want %s held for the spot",
				r.ID, c.PayloadCode, c.VendorOrderID, kscPartB)
		}
	}
	for _, leg := range co.legs {
		if c := coreOf(leg); c.VendorOrderID != "" || holds(c) == spare.ID {
			t.Fatalf("changeover leg %s went with Core holding bin %d: the supply must wait for a %s spare",
				leg.UUID, holds(c), kscPartB)
		}
	}
	// A robot takes the return. Edge learns an order is with the fleet at its
	// waybill; until then a cancel withdraws a vendor order nothing is driving.
	sim.DriveStateWithRobot(coreOf(ret).VendorOrderID, "RUNNING", "KSC-AMR")
	settle()
	if s := edgeStatus(ret); protocol.ChangeoverStartActionFor(s) == protocol.ChangeoverStartCancel {
		t.Fatalf("the return reads %q at Edge with a robot on it, want a status a cancel leaves alone", s)
	}

	// ── 3. The cancel: what had not flown is cancelled at Core, the flown
	// return lands, and the spare standing is the staying part's, kept. ──
	mustNil(t, edge.Engine.CancelProcessChangeover(processID), "cancel")
	settle()
	for _, o := range append(append([]domain.Order{}, co.legs...), co.refills...) {
		if c := coreOf(o); c.Status != protocol.StatusCancelled {
			t.Errorf("after cancel, %s %d reads %q at Core, want cancelled", o.OrderType, o.ID, c.Status)
		}
	}
	if c := coreOf(ret); protocol.IsTerminal(c.Status) {
		t.Errorf("the flown return reads %q at Core after the cancel, want it still landing", c.Status)
	}
	// Core still has the spare on the spot and it is the staying part, but its
	// return is already with the fleet: the spare is leaving. The reverted
	// reconcile decides the spot as bare, so it sends nothing back a second time
	// and orders one refill of the staying part to stand behind it.
	after := step()
	if len(after.legs) != 0 || len(after.returns) != 0 || len(after.refills) != 1 {
		t.Fatalf("the cancel made legs=%d refills=%d returns=%d, want one refill and nothing else: "+
			"the spare on the spot is leaving", len(after.legs), len(after.refills), len(after.returns))
	}
	if r := after.refills[0]; r.PayloadCode != kscPartA {
		t.Errorf("the cancel's refill carries %q, want the staying part %s", r.PayloadCode, kscPartA)
	}
}
