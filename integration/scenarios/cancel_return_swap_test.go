//go:build docker

package scenarios

import (
	"sync"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coreconfig "shingocore/config"
	"shingocore/dispatch"
	coreengine "shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	coremessaging "shingocore/messaging"
	"shingocore/service"
	corestore "shingocore/store"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// deckFleet is the simulator with a robot list the scenario sets: which robots
// exist, whether each is working, and whether its deck is loaded.
//
// The simulator only reports robots in sim builds, and then from its driver,
// which has no deck. The carried-bin watch runs on the engine's robot poll and
// reads JackState from it, so the scenario stands in for the vendor's robot
// report and nothing else. Orders, blocks and terminal states still go through
// the simulator and the engine's own tracker wiring.
type deckFleet struct {
	*simulator.SimulatorBackend

	mu     sync.Mutex
	robots map[string]fleet.RobotStatus
	pinned map[string]string // vendor order id -> the Vehicle Core's request named
}

var _ fleet.RobotLister = (*deckFleet)(nil)

func newDeckFleet() *deckFleet {
	return &deckFleet{SimulatorBackend: simulator.New(), robots: map[string]fleet.RobotStatus{},
		pinned: map[string]string{}}
}

// set reports one robot: busy or idle, deck loaded or empty. Busy is the
// vendor's task flag, and an idle robot is in the dispatch pool.
func (f *deckFleet) set(id string, busy, loaded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := fleet.RobotStatus{VehicleID: id, Connected: true, Available: !busy, Busy: busy,
		BatteryLevel: 100, RelocStatus: 1, Confidence: 0.95, JackState: 3}
	if loaded {
		r.JackState, r.JackIsFull, r.IsLoaded, r.LiftHeight = 1, true, true, 0.06
	}
	f.robots[id] = r
}

func (f *deckFleet) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.RobotStatus, 0, len(f.robots))
	for _, r := range f.robots {
		out = append(out, r)
	}
	return out, nil
}

// CreateOrder records which robot, if any, Core's request pinned the order to
// — the field the vendor reads — and hands the order to the simulator.
func (f *deckFleet) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	res, err := f.SimulatorBackend.CreateOrder(req)
	if err == nil {
		f.mu.Lock()
		f.pinned[res.VendorOrderID] = req.Vehicle
		f.mu.Unlock()
	}
	return res, err
}

func (f *deckFleet) pinnedTo(vendorOrderID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pinned[vendorOrderID]
}

// The rest of RobotLister: nothing in this scenario pauses, retries or
// force-completes a robot.
func (f *deckFleet) SetAvailability(string, bool) error { return nil }
func (f *deckFleet) RetryFailed(string) error           { return nil }
func (f *deckFleet) ForceComplete(string) error         { return nil }

// TestScenario_ChangeoverSwapCancelledMidCarry_BothBinsGoHome is the
// cancel-return feature end to end, on the shape it was built for: a two-robot
// changeover swap cancelled while BOTH robots have a bin on the deck.
//
//	the supply robot has lifted the NEW payload's bin off staging, on its way
//	to the line; the evac robot has lifted the OLD payload's bin off the line,
//	on its way out. The operator cancels the evac at the Edge. Core cancels it,
//	the swap-peer rule cancels the supply, both bins park on their robots'
//	carrier nodes, and the carried-bin watch sends each one home — the OLD bin
//	to where the claims source the OLD payload, the NEW bin to where they source
//	the NEW payload, each on the robot already carrying it. The Edge is told
//	"returning" for both cancelled orders, then "returned" as each return is
//	delivered.
//
// WHAT IS REAL. Every step runs through the production code path:
//   - the Edge plans and creates the swap (StartProcessChangeover, two_robot),
//     publishes its claims (PlantClaimsPublisher), releases the wait
//     (ReleaseChangeoverWait) and cancels the evac (Manager.AbortOrder);
//   - every envelope crosses the Bus both ways and is handled by Core's real
//     CoreHandler / CoreDataService and the Edge's real handlers;
//   - Core's intake, pair dispatch, release and cancel are the engine's, against
//     the simulator backend; the robot is assigned by a RUNNING status carrying
//     the vehicle, and each lift is the simulator's CompleteBlock for the
//     pickup block Core itself sent — so mission_events, _TRANSIT and the
//     swap-peer cascade are the engine's own;
//   - branch B (bin onto _ROBOT:<id>) runs from the real cancel event, and the
//     return is created by the engine's robot poll, not by calling the sweep;
//   - each return is delivered by the simulator reporting FINISHED for it.
//
// WHAT IS SYNTHESIZED. Only the robot report: deckFleet says which robot is
// busy and whether its deck is loaded (see deckFleet). The scenario flips each
// robot to loaded when its pickup block completes and to idle-and-loaded after
// the cancel — the order the vendor would report them in. The simulator's
// state transitions are driven by hand (no driver), so timing is the
// scenario's, not a fleet's.
//
// WHAT IT CATCHES that the Core docker tests do not: those seed the carried
// bin, the cancelled order and its claims by SQL and call the sweep directly.
// Here the carrier order is whatever Core's intake made of the Edge's real
// swap (its process/delivery/source nodes, which choose "own" claims), the
// claims are whatever the Edge publishes for the two styles, the peer's cancel
// is the cascade's (peer_terminal, which returnEligible must accept), the
// pickup leg is the one the tracker recorded, and the notices are decoded and
// stored by the Edge. A crossed return (old bin to the new payload's source)
// or a missing notice on either leg fails here.
//
// WHAT IT DOES NOT CATCH: the vendor's real jack report and its timing against
// the cancel, a fleet that is slow to stop the cancelled order, and the
// reconciliation sweep's backstop (only the robot poll triggers the return).
func TestScenario_ChangeoverSwapCancelledMidCarry_BothBinsGoHome(t *testing.T) {
	const (
		stationID = "edge.test"
		lineName  = "CRS-LINE"
		oldPart   = "CRS-OLD-P"
		newPart   = "CRS-NEW-P"
		staging   = "CRS-STAGING"
		outDest   = "CRS-OUT"
		supplyAMR = "AMR-CRS-1"
		evacAMR   = "AMR-CRS-2"
	)

	// ── Core: a started engine on the simulator, with the topology ──
	coreDB := coreharness.OpenDB(t)
	coreharness.SetupStandardData(t, coreDB) // ensures the STOR node type
	for _, code := range []string{oldPart, newPart} {
		if err := coreDB.CreatePayload(&corepayloads.Payload{Code: code, Description: code, UOPCapacity: 100}); err != nil {
			t.Fatalf("create payload %s: %v", code, err)
		}
	}
	oldGrp, oldSlots := crsStoreGroup(t, coreDB, "CRS-OLD", 2)
	newGrp, newSlots := crsStoreGroup(t, coreDB, "CRS-NEW", 2)
	line := crsNode(t, coreDB, lineName)
	crsNode(t, coreDB, staging)
	crsNode(t, coreDB, outDest)

	// The OLD payload's bin sits on the line; the NEW payload's bin in its
	// store. Each group keeps a free slot, so a return has somewhere to go.
	oldBin := coreharness.CreateBinAtNode(t, coreDB, oldPart, line.ID, "CRS-OLD-BIN")
	newBin := coreharness.CreateBinAtNode(t, coreDB, newPart, newSlots[0].ID, "CRS-NEW-BIN")

	fl := newDeckFleet()
	fl.set(supplyAMR, false, false)
	fl.set(evacAMR, false, false)

	cfg := coreconfig.Defaults()
	cfg.Messaging.StationID = "core.test"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	eng := coreengine.New(coreengine.Config{AppConfig: cfg, DB: coreDB, Fleet: fl, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)

	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, eng.Dispatcher())
	dataSvc := coremessaging.NewCoreDataService(coreDB, coreHandler, service.EpochAnnounce{
		Topic: cfg.Messaging.DispatchTopic, CoreStation: cfg.Messaging.StationID,
	})
	subjects := router.NewSubject()
	router.RegisterSubject(subjects, protocol.SubjectPlantClaims, dataSvc.HandlePlantClaims)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		subjects.Dispatch(env, p)
	})
	router.Register(coreRouter, protocol.TypeComplexOrderRequest, coreHandler.HandleComplexOrderRequest)
	router.Register(coreRouter, protocol.TypeOrderRelease, coreHandler.HandleOrderRelease)
	router.Register(coreRouter, protocol.TypeOrderCancel, coreHandler.HandleOrderCancel)
	router.Register(coreRouter, protocol.TypeOrderReceipt, coreHandler.HandleOrderReceipt)
	coreIngestor := protocol.NewIngestor(nil)
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

	// ── Edge: a consume line changing over from OLD to NEW, two_robot ──
	edge := edgeharness.NewEdge(t, stationID)
	processID, err := edge.DB.CreateProcess("CRS-PROC", "cancel-return swap", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: lineName, Code: "CRS1", Name: "CRS line", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	fromStyleID, err := edge.DB.CreateStyle("CRS-FROM", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := edge.DB.CreateStyle("CRS-TO", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	fromClaimID, err := edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: lineName, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: oldPart, UOPCapacity: 100,
		InboundSource: oldGrp.Name, InboundStaging: staging, OutboundDestination: outDest,
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	if _, err := edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: lineName, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: newPart, UOPCapacity: 100,
		InboundSource: newGrp.Name, InboundStaging: staging, OutboundDestination: outDest,
	}); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}
	if _, err := edge.DB.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	if err := edge.DB.SetProcessNodeRuntime(nodeID, &fromClaimID, 60); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
	)

	// The Edge's claims reach Core's mirror the way they do in the plant: the
	// publisher's report, over the wire. This is what the return reads.
	if err := edgemessaging.NewPlantClaimsPublisher(edge.DB, stationID, 0).PublishChanged(processID); err != nil {
		t.Fatalf("publish plant claims: %v", err)
	}
	bus.PumpAll()

	// ── The changeover creates the paired swap; Core takes both legs ──
	co, err := edge.Engine.StartProcessChangeover(processID, toStyleID, "scenario", "cancel-return swap")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := edge.DB.GetChangeoverNodeTaskByNode(co.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.NextMaterialOrderID == nil || task.OldMaterialReleaseOrderID == nil {
		t.Fatal("the two_robot changeover did not create both legs")
	}
	edgeSupply, err := edge.DB.GetOrder(*task.NextMaterialOrderID)
	if err != nil {
		t.Fatalf("read edge supply: %v", err)
	}
	edgeEvac, err := edge.DB.GetOrder(*task.OldMaterialReleaseOrderID)
	if err != nil {
		t.Fatalf("read edge evac: %v", err)
	}
	bus.PumpAll()

	supply := crsOrder(t, coreDB, edgeSupply.UUID)
	evac := crsOrder(t, coreDB, edgeEvac.UUID)
	if supply.VendorOrderID == "" || evac.VendorOrderID == "" {
		t.Fatalf("Core did not dispatch the pair: supply %s (%q), evac %s (%q)",
			supply.Status, supply.VendorOrderID, evac.Status, evac.VendorOrderID)
	}

	// ── Both robots drive to their waits. The supply lifts the NEW bin from
	// its store and sets it down at staging on the way. ──
	fl.set(supplyAMR, true, false)
	fl.set(evacAMR, true, false)
	crsDrive(t, fl, supply, "RUNNING", supplyAMR)
	crsDrive(t, fl, evac, "RUNNING", evacAMR)
	lifted := crsCompleteNext(t, fl, supply.VendorOrderID, 0, coreengine.IsPickupBlock)
	staged := crsCompleteNext(t, fl, supply.VendorOrderID, lifted+1, coreengine.IsDropoffBlock)
	crsDrive(t, fl, supply, "WAITING", supplyAMR)
	crsDrive(t, fl, evac, "WAITING", evacAMR)
	bus.PumpAll()

	// ── The operator releases the swap; each robot lifts its bin. ──
	if _, err := edge.Engine.ReleaseChangeoverWait(processID, edgeengine.ReleaseDisposition{
		Mode: edgeengine.DispositionSendPartialBack, PartialCount: ptr(40), CalledBy: "scenario-operator",
	}); err != nil {
		t.Fatalf("release the swap: %v", err)
	}
	bus.PumpAll()
	crsDrive(t, fl, supply, "RUNNING", supplyAMR)
	crsDrive(t, fl, evac, "RUNNING", evacAMR)
	crsCompleteNext(t, fl, supply.VendorOrderID, staged+1, coreengine.IsPickupBlock) // the lift off staging
	crsCompleteNext(t, fl, evac.VendorOrderID, 0, coreengine.IsPickupBlock)          // the lift off the line
	fl.set(supplyAMR, true, true)
	fl.set(evacAMR, true, true)
	crsEventually(t, "both robots read loaded in Core's cache", func() bool {
		a, okA := eng.GetCachedRobotStatus(supplyAMR)
		b, okB := eng.GetCachedRobotStatus(evacAMR)
		return okA && okB && a.JackState == 1 && b.JackState == 1
	})
	for _, b := range []*corebins.Bin{oldBin, newBin} {
		if got := crsBin(t, coreDB, b.ID); got.NodeName != "_TRANSIT" {
			t.Fatalf("bin %s is at %q after its lift, want _TRANSIT", b.Label, got.NodeName)
		}
	}
	bus.PumpAll()

	// ── The operator cancels the evac at the Edge. ──
	if err := edge.Engine.OrderManager().AbortOrder(edgeEvac.ID); err != nil {
		t.Fatalf("abort the evac: %v", err)
	}
	bus.PumpAll()

	for _, o := range []*coreorders.Order{evac, supply} {
		if got := crsOrder(t, coreDB, o.EdgeUUID); got.Status != protocol.StatusCancelled {
			t.Fatalf("order %s is %s after the cancel, want cancelled (the evac by the operator, the supply "+
				"by the swap-peer rule)", o.EdgeUUID, got.Status)
		}
	}
	// Branch B: each bin rides its own robot.
	if got := crsBin(t, coreDB, oldBin.ID).NodeName; got != corebins.CarrierNodePrefix+evacAMR {
		t.Fatalf("old bin is at %q after the cancel, want the evac robot's carrier node", got)
	}
	if got := crsBin(t, coreDB, newBin.ID).NodeName; got != corebins.CarrierNodePrefix+supplyAMR {
		t.Fatalf("new bin is at %q after the cancel, want the supply robot's carrier node", got)
	}

	// ── The fleet stops both jobs; the decks stay loaded. The robot poll's
	// watch makes the returns. ──
	fl.set(supplyAMR, false, true)
	fl.set(evacAMR, false, true)
	var oldRet, newRet *coreorders.Order
	crsEventually(t, "the watch creates and dispatches a return for each bin", func() bool {
		oldRet, newRet = crsReturn(t, coreDB, oldBin.ID), crsReturn(t, coreDB, newBin.ID)
		return oldRet != nil && newRet != nil && oldRet.VendorOrderID != "" && newRet.VendorOrderID != ""
	})

	// Each return is pinned to the robot carrying the bin, recovers the order
	// that bin was on, and goes to ITS payload's source — never crossed.
	crsCheckReturn(t, fl, "old bin", oldRet, evacAMR, evac.ID, oldSlots, newSlots)
	crsCheckReturn(t, fl, "new bin", newRet, supplyAMR, supply.ID, newSlots, oldSlots)

	bus.PumpAll()
	crsCheckNotices(t, edge, map[string]crsNotice{
		edgeEvac.UUID:   {protocol.BinReturnReturning, oldRet.DeliveryNode, oldBin.Label},
		edgeSupply.UUID: {protocol.BinReturnReturning, newRet.DeliveryNode, newBin.Label},
	})

	// ── Each robot sets its bin down: the fleet reports the return FINISHED. ──
	for _, ret := range []struct {
		o     *coreorders.Order
		robot string
	}{{oldRet, evacAMR}, {newRet, supplyAMR}} {
		crsDrive(t, fl, ret.o, "RUNNING", ret.robot)
		crsDrive(t, fl, ret.o, "FINISHED", ret.robot)
		if got := crsOrder(t, coreDB, ret.o.EdgeUUID); got.Status != protocol.StatusDelivered &&
			got.Status != protocol.StatusConfirmed {
			t.Fatalf("return %d is %s after FINISHED, want delivered", ret.o.ID, got.Status)
		}
	}
	if got := crsBin(t, coreDB, oldBin.ID).NodeName; got != oldRet.DeliveryNode {
		t.Errorf("old bin is at %q after its return, want %s", got, oldRet.DeliveryNode)
	}
	if got := crsBin(t, coreDB, newBin.ID).NodeName; got != newRet.DeliveryNode {
		t.Errorf("new bin is at %q after its return, want %s", got, newRet.DeliveryNode)
	}

	bus.PumpAll()
	crsCheckNotices(t, edge, map[string]crsNotice{
		edgeEvac.UUID:   {protocol.BinReturnReturned, oldRet.DeliveryNode, oldBin.Label},
		edgeSupply.UUID: {protocol.BinReturnReturned, newRet.DeliveryNode, newBin.Label},
	})
}

// ── helpers ─────────────────────────────────────────────────────────────────

func crsNode(t *testing.T, db *corestore.DB, name string) *corenodes.Node {
	t.Helper()
	n := &corenodes.Node{Name: name, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n
}

// crsStoreGroup is a node group with n flat storage slots.
func crsStoreGroup(t *testing.T, db *corestore.DB, prefix string, n int) (*corenodes.Node, []*corenodes.Node) {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	stor, err := db.GetNodeTypeByCode("STOR")
	if err != nil {
		t.Fatalf("get STOR type: %v", err)
	}
	grp := &corenodes.Node{Name: prefix + "-GRP", NodeTypeID: &ngrp.ID, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create group %s: %v", grp.Name, err)
	}
	var slots []*corenodes.Node
	for i := 1; i <= n; i++ {
		s := &corenodes.Node{Name: prefix + "-S" + string(rune('0'+i)), NodeTypeID: &stor.ID, ParentID: &grp.ID, Enabled: true}
		if err := db.CreateNode(s); err != nil {
			t.Fatalf("create slot %s: %v", s.Name, err)
		}
		slots = append(slots, s)
	}
	return grp, slots
}

func crsOrder(t *testing.T, db *corestore.DB, uuid string) *coreorders.Order {
	t.Helper()
	o, err := db.GetOrderByUUID(uuid)
	if err != nil || o == nil {
		t.Fatalf("Core has no order %s: %v", uuid, err)
	}
	return o
}

func crsBin(t *testing.T, db *corestore.DB, id int64) *corebins.Bin {
	t.Helper()
	b, err := db.GetBin(id)
	if err != nil || b == nil {
		t.Fatalf("read bin %d: %v", id, err)
	}
	return b
}

// crsReturn is the on-deck order the watch made for the bin, or nil.
func crsReturn(t *testing.T, db *corestore.DB, binID int64) *coreorders.Order {
	t.Helper()
	ords, err := db.ListOrdersByBin(binID, 20)
	if err != nil {
		t.Fatalf("list orders of bin %d: %v", binID, err)
	}
	var found *coreorders.Order
	for _, o := range ords {
		if o.SourceIntent == dispatch.SourceIntentOnDeck {
			if found != nil {
				t.Fatalf("bin %d has more than one on-deck order (%d and %d) — one return per episode", binID, found.ID, o.ID)
			}
			found = o
		}
	}
	return found
}

// crsDrive moves a simulated order to state with the robot on it — the
// fleet's status report, which assigns the robot on Core's order.
func crsDrive(t *testing.T, fl *deckFleet, o *coreorders.Order, state, robot string) {
	t.Helper()
	if _, _, deferred := fl.DriveStateWithRobot(o.VendorOrderID, state, robot); deferred {
		t.Fatalf("simulator deferred %s for order %d", state, o.ID)
	}
}

// crsCompleteNext completes the first block of the vendor order, at or after
// index from, whose binTask satisfies want — the fleet reporting that block
// FINISHED, through the simulator's tracker emitter — and returns its index.
// Every block it passes over is completed too, in order, as a fleet would.
func crsCompleteNext(t *testing.T, fl *deckFleet, vendorID string, from int, want func(string) bool) int {
	t.Helper()
	blocks := fl.BlocksForOrder(vendorID)
	now := time.Now().Unix()
	for i := from; i < len(blocks); i++ {
		b := blocks[i]
		if !fl.CompleteBlock(vendorID, b.BlockID, b.Location, b.BinTask, now-1, now) {
			t.Fatalf("complete block %s of %s: not emitted", b.BlockID, vendorID)
		}
		if want(b.BinTask) {
			return i
		}
	}
	t.Fatalf("vendor order %s has no wanted block at or after %d: %+v", vendorID, from, blocks)
	return -1
}

func crsCheckReturn(t *testing.T, fl *deckFleet, what string, ret *coreorders.Order, robot string, carrierID int64,
	ownSlots, otherSlots []*corenodes.Node) {
	t.Helper()
	if got := fl.pinnedTo(ret.VendorOrderID); got != robot {
		t.Errorf("%s: return %d went to the fleet pinned to %q, want %s — the robot carrying it", what, ret.ID, got, robot)
	}
	if ret.RecoversOrderID == nil || *ret.RecoversOrderID != carrierID {
		t.Errorf("%s: return %d recovers %v, want cancelled order %d", what, ret.ID, ret.RecoversOrderID, carrierID)
	}
	in := func(slots []*corenodes.Node) bool {
		for _, s := range slots {
			if s.Name == ret.DeliveryNode {
				return true
			}
		}
		return false
	}
	switch {
	case in(otherSlots):
		t.Errorf("%s: return goes to %s, the OTHER payload's source — the returns crossed", what, ret.DeliveryNode)
	case !in(ownSlots):
		t.Errorf("%s: return goes to %q, want a slot of its payload's claim source", what, ret.DeliveryNode)
	}
}

type crsNotice struct{ state, destination, label string }

func crsCheckNotices(t *testing.T, edge *edgeharness.Edge, want map[string]crsNotice) {
	t.Helper()
	uuids := make([]string, 0, len(want))
	for u := range want {
		uuids = append(uuids, u)
	}
	got, err := edge.DB.BinReturnsForOrders(uuids)
	if err != nil {
		t.Fatalf("read edge bin_returns: %v", err)
	}
	for u, w := range want {
		g, ok := got[u]
		if !ok {
			t.Errorf("edge has no bin_return for cancelled order %s, want %s", u, w.state)
			continue
		}
		if g.State != w.state || g.Destination != w.destination || g.BinLabel != w.label {
			t.Errorf("edge bin_return for %s = {%s → %s, bin %s}, want {%s → %s, bin %s}",
				u, g.State, g.Destination, g.BinLabel, w.state, w.destination, w.label)
		}
	}
}

// crsEventually waits for a condition the engine's 2-second robot poll makes
// true.
func crsEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}
