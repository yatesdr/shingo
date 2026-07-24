package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// captureDeliveredNotBound subscribes an alarm collector to the engine bus.
func captureDeliveredNotBound(eng *Engine) *[]DeliveredNotBoundEvent {
	var alarms []DeliveredNotBoundEvent
	eng.Events.SubscribeTypes(func(evt Event) {
		if a, ok := evt.Payload.(DeliveredNotBoundEvent); ok {
			alarms = append(alarms, a)
		}
	}, EventDeliveredNotBound)
	return &alarms
}

// TestDeliveredNotBound_MultiBinRaisesAlarm pins P2-C3: a multi-bin delivery to
// a node we own (ProcessNodeID set, envelope BinID nil — the F1b gap) no longer
// no-ops silently. It raises a named EventDeliveredNotBound and stays alarm-only:
// nothing binds (F1b is out of scope). The alarm names the node and carries the
// operator's front-door instruction.
func TestDeliveredNotBound_MultiBinRaisesAlarm(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "MB-ALARM", PayloadCode: "PART-MB", UOPCapacity: 100, InitialUOP: 50,
	})
	// Start unbound so we can prove nothing binds.
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	alarms := captureDeliveredNotBound(eng)

	// Multi-bin delivery: ProcessNodeID set, BinID nil.
	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID:       901,
		OrderUUID:     "uuid-multibin",
		ProcessNodeID: &nodeID,
		BinID:         nil,
	}})

	if len(*alarms) != 1 {
		t.Fatalf("alarms = %d, want 1 (multi-bin delivery must raise EventDeliveredNotBound): %+v", len(*alarms), *alarms)
	}
	a := (*alarms)[0]
	if a.CoreNodeName != node.CoreNodeName {
		t.Errorf("alarm node = %q, want %q", a.CoreNodeName, node.CoreNodeName)
	}
	if a.BinID != nil {
		t.Errorf("alarm BinID = %v, want nil (multi-bin has no per-bin id)", a.BinID)
	}
	if !strings.Contains(a.Reason, "multi-bin") {
		t.Errorf("alarm reason = %q, want it to mention multi-bin", a.Reason)
	}
	if a.Instruction == "" {
		t.Error("alarm must carry a front-door instruction for the operator")
	}

	// Alarm-only: nothing bound.
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.ActiveBinID != nil {
		t.Errorf("ActiveBinID = %v, want nil (multi-bin stays alarm-only, F1b open)", rt.ActiveBinID)
	}
}

// TestDeliveredNotBound_NoActiveClaimRaisesAlarm pins P2-C3: a single-bin bin
// that lands at a node we own but has no active claim to bind to no longer
// silently no-ops. It raises a named EventDeliveredNotBound identifying the bin
// and node.
func TestDeliveredNotBound_NoActiveClaimRaisesAlarm(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, _ := db.CreateProcess("NC-PROC", "no-claim test", "active_production", "", "", false, false)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:         pid,
		OperatorStationID: &sid,
		CoreNodeName:      "NOCLAIM-NODE",
		Enabled:           true,
	})
	testutil.MustNoErr(t, err, "create node")
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	// No style, no claim → findActiveClaim returns nil.

	const binID int64 = 4242
	orderID, err := db.CreateOrder("uuid-noclaim", orders.TypeRetrieve, &nodeID, false, 1,
		"NOCLAIM-NODE", "", "", "", false, "PART-NC")
	testutil.MustNoErr(t, err, "create order")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	alarms := captureDeliveredNotBound(eng)

	bid := binID
	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID:       orderID,
		OrderUUID:     "uuid-noclaim",
		OrderType:     orders.TypeRetrieve,
		ProcessNodeID: &nodeID,
		BinID:         &bid,
	}})

	if len(*alarms) != 1 {
		t.Fatalf("alarms = %d, want 1 (no active claim must raise EventDeliveredNotBound): %+v", len(*alarms), *alarms)
	}
	a := (*alarms)[0]
	if a.BinID == nil || *a.BinID != binID {
		t.Errorf("alarm BinID = %v, want %d (names the exact carrier)", a.BinID, binID)
	}
	if a.CoreNodeName != "NOCLAIM-NODE" {
		t.Errorf("alarm node = %q, want NOCLAIM-NODE", a.CoreNodeName)
	}
	if !strings.Contains(a.Reason, "claim") {
		t.Errorf("alarm reason = %q, want it to mention the missing claim", a.Reason)
	}
}

// TestDeliveredNotBound_ChangeoverAutoConfirmBindsNotSilent pins the P2-C3
// changeover requirement: a bin delivered by a changeover auto-confirm order
// (AutoConfirm=true, so the order goes terminal on arrival) must still BIND the
// runtime — it is not a silent path. The delivered event fires and binds the bin
// BEFORE auto-confirm makes the order terminal, so no alarm is needed. If this
// ever regressed to a silent no-op (unbound + no alarm), the assertions trip.
func TestDeliveredNotBound_ChangeoverAutoConfirmBindsNotSilent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "CO-AC", PayloadCode: "PART-COAC", UOPCapacity: 300, InitialUOP: 0,
	})
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "clear active bin (empty slot pre-delivery)")
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")

	const uuid = "uuid-co-autoconfirm"
	const binID int64 = 606
	// autoConfirm=true → the changeover auto-confirm order.
	orderID, err := db.CreateOrder(uuid, orders.TypeRetrieve, &nodeID, false, 1,
		node.CoreNodeName, "", "", "", true, "PART-COAC")
	testutil.MustNoErr(t, err, "create order")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "set in_transit")

	eng := testEngineWithOrderBridge(t, db)
	alarms := captureDeliveredNotBound(eng)

	const snapshotUOP = 250
	bid := binID
	uop := snapshotUOP
	testutil.MustNoErr(t,
		eng.orderMgr.HandleDeliveredWithExpiry(uuid, "changeover auto-confirm delivery", nil, &bid, &uop, 4, node.CoreNodeName),
		"handle delivered")

	// Bound (not silent) — active bin + snapshot count.
	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	if rt.ActiveBinID == nil || *rt.ActiveBinID != binID {
		t.Errorf("ActiveBinID = %v, want %d (changeover auto-confirm delivery must bind)", rt.ActiveBinID, binID)
	}
	if rt.RemainingUOPCached != snapshotUOP {
		t.Errorf("RemainingUOPCached = %d, want %d (bound at the delivered snapshot)", rt.RemainingUOPCached, snapshotUOP)
	}
	// It bound, so there is no unbound skip to alarm.
	if len(*alarms) != 0 {
		t.Errorf("alarms = %d, want 0 (a bound delivery is not a not-bound alarm): %+v", len(*alarms), *alarms)
	}

	// And the order went terminal (auto-confirmed) — the bind happened first.
	got, _ := db.GetOrderByUUID(uuid)
	if !orders.IsTerminal(got.Status) {
		t.Errorf("order status = %s, want terminal (auto-confirm ran after the bind)", got.Status)
	}
}
