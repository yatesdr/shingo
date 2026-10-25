package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/release"
	"shingoedge/store"
)

// cancel_staged_carrier_test.go — a carrier an order set down at staging to
// wait, and the order then cancelled.
//
// A two-robot supply fetches the new carrier, sets it down on staging, waits
// there for the release, and only then takes it on to the line. Cancelled at
// that wait — from Core's admin page or anywhere — it left the carrier standing
// on staging, outside every pool and blocking the next swap's dropoff (SPR
// 2026-10-14: CARRIER-0026, 350 of 76682, on SLN_001). The cancel now sends it
// back to where it was fetched from, by the changeover cancel's own plain move.

func twoRobotSupplySteps(source, stage, line string) []protocol.ComplexOrderStep {
	drop := buildStep("dropoff", stage)
	drop.ExclusiveSlot = true
	return []protocol.ComplexOrderStep{
		buildStep("pickup", source),
		drop,
		stationWait(stage, release.PurposeSwap),
		buildStep("pickup", stage),
		buildStep("dropoff", line),
	}
}

func TestWaitingAtStage(t *testing.T) {
	t.Parallel()
	stage, dest, ord, ok := waitingAtStage(twoRobotSupplySteps("SMN_015", "SLN_001", "ALN_001"), "ALN_001")
	if !ok || stage != "SLN_001" || dest != "SMN_015" || ord != 0 {
		t.Errorf("two-robot supply = (%q, %q, %d, %v), want (SLN_001, SMN_015, 0, true)", stage, dest, ord, ok)
	}
	// A stage order ends at staging and waits for another order: stagedTrip's,
	// not this.
	drop := buildStep("dropoff", "SLN_001")
	drop.ExclusiveSlot = true
	if _, _, _, ok := waitingAtStage([]protocol.ComplexOrderStep{buildStep("pickup", "SMN_015"), drop}, "ALN_001"); ok {
		t.Error("a stage order with no wait after its drop read as a carrier waiting at staging")
	}
	// The line's own bin parked on its way out is the park's, not this.
	if _, _, _, ok := waitingAtStage(twoRobotSupplySteps("ALN_001", "SLN_001", "SMN_015"), "ALN_001"); ok {
		t.Error("a carrier lifted off the line read as an incoming carrier waiting at staging")
	}
}

// seedSupplyStagedThenCancelled is a two-robot supply that reached its wait at
// staging (staged, station wait 0) and was then cancelled.
func seedSupplyStagedThenCancelled(t *testing.T, db *store.DB, nodeID int64, line, source, stage string) int64 {
	t.Helper()
	id, err := db.CreateOrder("uuid-supply-staged", orders.TypeComplex, &nodeID, false, 1,
		line, "", source, protocol.Height{}, false, "PART-NEW", "", "")
	testutil.MustNoErr(t, err, "create supply")
	raw, err := json.Marshal(twoRobotSupplySteps(source, stage, line))
	testutil.MustNoErr(t, err, "encode steps")
	testutil.MustNoErr(t, db.UpdateOrderStepsAndFacts(id, string(raw), ""), "store steps")
	stageAtItsWait(t, db, id)
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusCancelled)), "cancelled")
	return id
}

func TestCancelledAtItsStagingWait_SendsTheCarrierBack(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	id := seedSupplyStagedThenCancelled(t, db, nodeID, "CO-NODE", "SRC-LOADER", "STG-1")

	changed := OrderStatusChangedEvent{OrderID: id, OldStatus: string(protocol.StatusStaged),
		NewStatus: string(protocol.StatusCancelled), ProcessNodeID: &nodeID}
	p, ok := eng.carrierLeftWaiting(changed)
	if !ok || p.park != "STG-1" || p.dest != "SRC-LOADER" {
		t.Fatalf("carrier left waiting = (%+v, %v), want STG-1 back to SRC-LOADER", p, ok)
	}

	// The carrier on the staging node, from the bins table (staging is on
	// the watch list; the cancel's park read, site 1 of the reads).
	coreNeverAnswers(t, eng)
	holdActableBins(t, eng, protocol.NodeBinsRow{Node: "STG-1", Occupied: true, PayloadCode: "PART-NEW"})
	eng.sendCarrierBack(p)

	live, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	var moves int
	for _, o := range live {
		if o.OrderType == orders.TypeMove && o.SourceNode == "STG-1" {
			moves++
			if o.DeliveryNode != "SRC-LOADER" || o.PayloadCode != "PART-NEW" {
				t.Errorf("move goes %s->%s carrying %q, want ->SRC-LOADER carrying PART-NEW",
					o.SourceNode, o.DeliveryNode, o.PayloadCode)
			}
		}
	}
	if moves != 1 {
		t.Errorf("moves off STG-1 = %d, want 1: the carrier goes back where it came from", moves)
	}
}

func TestCancelledAtItsStagingWait_OnlyThatCase(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)
	id := seedSupplyStagedThenCancelled(t, db, nodeID, "CO-NODE", "SRC-LOADER", "STG-1")

	for _, c := range []struct {
		name     string
		old, now protocol.Status
	}{
		{"cancelled on its way, before it set the carrier down", protocol.StatusInTransit, protocol.StatusCancelled},
		{"staged and then released, not cancelled", protocol.StatusStaged, protocol.StatusInTransit},
		{"failed at its wait", protocol.StatusStaged, protocol.StatusFailed},
	} {
		if _, ok := eng.carrierLeftWaiting(OrderStatusChangedEvent{OrderID: id, OldStatus: string(c.old),
			NewStatus: string(c.now), ProcessNodeID: &nodeID}); ok {
			t.Errorf("%s: read as a carrier left waiting at staging", c.name)
		}
	}
}

// A keep-staged spot is the keeper's: a carrier standing there is the spare it
// wants, so a cancel leaves it, as the changeover cancel's park does.
func TestCancelledAtItsStagingWait_LeavesAKeepStagedSpare(t *testing.T) {
	t.Parallel()
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
		map[string]protocol.NodeBinInfo{})
	nodeID := fx.nodeIDs["L1"]
	id := seedSupplyStagedThenCancelled(t, fx.db, nodeID, "L1", "SRC-OLD", "SPOT")
	if p, ok := fx.eng.carrierLeftWaiting(OrderStatusChangedEvent{OrderID: id, OldStatus: string(protocol.StatusStaged),
		NewStatus: string(protocol.StatusCancelled), ProcessNodeID: &nodeID}); ok {
		t.Errorf("a cancelled supply's carrier on the keep-staged spot read as stranded (%+v)", p)
	}
}
