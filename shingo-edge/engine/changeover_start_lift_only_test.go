package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/release"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// changeover_start_lift_only_test.go — an order that only LIFTS the bin of a
// changing node, at changeover START.
//
// The start gate asked one question of a live order: does it place a bin at a
// changing node. An order that only takes the node's bin away (a sequential
// pair's removal: wait(N) pickup(N) dropoff(OUT)) answered no at every status,
// so START passed it; the changeover then planned its own pickup of the same
// bin, and whichever of the two lifted second waited for good (edge2 run
// post-edge2c-c8a8cc21, PRS_010B).
//
// What START does with it is read from its steps and where it stands in them:
// with its lift still ahead and not yet released towards it, it is cancelled,
// whatever its status; released and not yet lifted, the bin is leaving and
// START is refused for that moment; once lifted, it is left alone.

// seedLiftOnlyAt creates a lift-only order at coreNode, the sequential
// removal's shape, and drives it to status.
func seedLiftOnlyAt(t *testing.T, db *store.DB, nodeID int64, coreNode, uuid string, status protocol.Status) int64 {
	t.Helper()
	id, err := db.CreateOrder(uuid, orders.TypeComplex, &nodeID, false, 1,
		"DEST-OLD", "", coreNode, "", false, "PART-OLD", "", "")
	testutil.MustNoErr(t, err, "create lift-only order "+uuid)
	steps := []protocol.ComplexOrderStep{
		stationWait(coreNode, release.PurposeReady),
		buildStep("pickup", coreNode),
		buildStep("dropoff", "DEST-OLD"),
	}
	raw, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "encode steps")
	facts, err := release.FactsFromSteps(steps, coreNode).Encode()
	testutil.MustNoErr(t, err, "encode facts")
	testutil.MustNoErr(t, db.UpdateOrderStepsAndFacts(id, string(raw), facts), "store steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(status)), "set status "+string(status))
	return id
}

// stageAtItsWait is Core reporting the order staged at its first station wait.
func stageAtItsWait(t *testing.T, db *store.DB, orderID int64) {
	t.Helper()
	o, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "get order")
	first := 0
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusStaged)), "staged")
	testutil.MustNoErr(t, db.SetOrderWaitPoint(o.UUID, &first, orders.WaitKindStation), "wait point")
}

// releaseFromItsWait is the operator's release of the first wait gone out and
// Core moving the robot on: in transit, released past the wait.
func releaseFromItsWait(t *testing.T, db *store.DB, orderID int64) {
	t.Helper()
	stageAtItsWait(t, db, orderID)
	intent, err := release.Intent{StationWait: 0, Origin: release.OriginStationOrder, SentAt: "2026-10-12T21:00:00Z"}.Encode()
	testutil.MustNoErr(t, err, "encode intent")
	testutil.MustNoErr(t, db.SetOrderReleaseIntent(orderID, intent), "intent sent")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusInTransit)), "in_transit")
}

// TestChangeoverStart_CancelsALiftOnlyOrderBeforeItsRelease: up to its release,
// the lift-only order is cancelled at START, at every status, and the
// changeover starts. The bin stays on the node for the changeover's own swap.
func TestChangeoverStart_CancelsALiftOnlyOrderBeforeItsRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		drive func(t *testing.T, db *store.DB, id int64)
	}{
		{"pending", func(t *testing.T, db *store.DB, id int64) {}},
		{"submitted", func(t *testing.T, db *store.DB, id int64) {
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusSubmitted)), "submitted")
		}},
		{"acknowledged", func(t *testing.T, db *store.DB, id int64) {
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusAcknowledged)), "acknowledged")
		}},
		{"queued", func(t *testing.T, db *store.DB, id int64) {
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusQueued)), "queued")
		}},
		{"dispatched", func(t *testing.T, db *store.DB, id int64) {
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusDispatched)), "dispatched")
		}},
		{"in transit to its wait", func(t *testing.T, db *store.DB, id int64) {
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusInTransit)), "in_transit")
		}},
		{"staged at its wait", stageAtItsWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
			eng := testEngine(t, db)

			id := seedLiftOnlyAt(t, db, nodeID, "CO-NODE", "uuid-lift-"+tc.name, orders.StatusPending)
			tc.drive(t, db, id)

			if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
				t.Fatalf("START refused with a lift-only order %s at CO-NODE: %v — its lift is still ahead "+
					"and not released, so it is cancelled, not waited for", tc.name, err)
			}
			if got := reloadStatus(t, db, id); got != orders.StatusCancelled {
				t.Errorf("lift-only order %s = %s after START, want cancelled — left alive, it and the "+
					"changeover's own swap both lift CO-NODE's bin and the second waits for good", tc.name, got)
			}
		})
	}
}

// TestChangeoverStart_RefusesWhileAReleasedLiftIsOnItsWay: released and not
// yet lifted, the bin is leaving. START is refused with the carrier sentence,
// and the order is left exactly as it was.
func TestChangeoverStart_RefusesWhileAReleasedLiftIsOnItsWay(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	id := seedLiftOnlyAt(t, db, nodeID, "CO-NODE", "uuid-lift-released", orders.StatusPending)
	releaseFromItsWait(t, db, id)
	bin := int64(41)
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, &bin), "CO-NODE's seat holds its bin")

	_, err := eng.StartProcessChangeover(processID, toStyleID, "test", "")
	if err == nil {
		t.Fatal("START accepted while a released order is on its way to lift CO-NODE's bin — the " +
			"changeover would plan its own pickup of a bin that is leaving")
	}
	if !strings.Contains(err.Error(), "a carrier is on its way") {
		t.Errorf("refusal = %q, want the existing \"a carrier is on its way\" sentence", err.Error())
	}
	if got := reloadStatus(t, db, id); got != orders.StatusInTransit {
		t.Errorf("released order = %s after the refusal, want in_transit, untouched", got)
	}
	co, err := db.GetActiveProcessChangeover(processID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("active changeover: %v", err)
	}
	if co != nil {
		t.Errorf("a refused START left changeover %d active", co.ID)
	}
}

// TestChangeoverStart_LeavesALiftOnlyOrderAloneAfterItsLift: the bin has left
// the node, so the order neither blocks nor is cancelled.
func TestChangeoverStart_LeavesALiftOnlyOrderAloneAfterItsLift(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID, _, _ := seedChangeoverScenario(t, db)
	eng := testEngine(t, db)

	id := seedLiftOnlyAt(t, db, nodeID, "CO-NODE", "uuid-lift-lifted", orders.StatusPending)
	releaseFromItsWait(t, db, id)
	// The lift, as the seat records it: the pointer cleared and the carrier
	// recorded as gone (carrierLeft).
	testutil.MustNoErr(t, db.SetProcessNodeActiveBinID(nodeID, nil), "the seat's pointer cleared")
	eng.recordLinesideCarrier(nodeID, "CO-NODE", domain.UnknownCarrier(), domain.CarrierDeparted)

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
		t.Fatalf("START refused after the order lifted CO-NODE's bin: %v — the bin has left", err)
	}
	if got := reloadStatus(t, db, id); got != orders.StatusInTransit {
		t.Errorf("lifted order = %s after START, want in_transit, left alone", got)
	}
}

// TestApplyNodeState_EmptyIsAdd_ThreeAnswers: a Swap whose seat says its carrier has
// gone is planned as a refill only; a seat holding a bin, or one that cannot
// say, leaves the swap as it is. Nothing but a Swap is touched.
func TestApplyNodeState_EmptyIsAdd_ThreeAnswers(t *testing.T) {
	t.Parallel()
	to := &processes.NodeClaim{CoreNodeName: "N", PayloadCode: "NEW"}
	for _, tc := range []struct {
		name      string
		situation ChangeoverSituation
		seat      Seat
		want      ChangeoverSituation
	}{
		{"swap, no bin", SituationSwap, SeatEmpty, SituationAdd},
		{"swap, a bin", SituationSwap, SeatBin, SituationSwap},
		{"swap, unknown", SituationSwap, SeatUnknown, SituationSwap},
		{"evacuate, no bin", SituationEvacuate, SeatEmpty, SituationEvacuate},
		{"drop, no bin", SituationDrop, SeatEmpty, SituationDrop},
	} {
		diffs := []ChangeoverNodeDiff{{CoreNodeName: "N", Situation: tc.situation, ToClaim: to}}
		got := ApplyNodeState(diffs, func(string) NodeState { return NodeState{Seat: tc.seat} })[0].Situation
		if got != tc.want {
			t.Errorf("%s: situation %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestChangeoverStart_LeavesALiftOnlyOrderAtAnUnchangedNodeAlone: at a node
// the changeover does nothing to, a lift-only order is ordinary production
// traffic, at any status.
func TestChangeoverStart_LeavesALiftOnlyOrderAtAnUnchangedNodeAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		drive func(t *testing.T, db *store.DB, id int64)
	}{
		{"queued", func(t *testing.T, db *store.DB, id int64) {
			testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(orders.StatusQueued)), "queued")
		}},
		{"staged at its wait", stageAtItsWait},
		{"released", releaseFromItsWait},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, nodes, _, toStyleID := seedMultiNodeScenario(t, db)
			eng := testEngine(t, db)

			id := seedLiftOnlyAt(t, db, nodes["NODE-UNCHANGED"], "NODE-UNCHANGED", "uuid-unch-"+tc.name, orders.StatusPending)
			tc.drive(t, db, id)
			before := reloadStatus(t, db, id)

			if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", ""); err != nil {
				t.Fatalf("START refused over a lift-only order at an unchanged node: %v", err)
			}
			if got := reloadStatus(t, db, id); got != before {
				t.Errorf("lift-only order at NODE-UNCHANGED = %s after START, want %s — the changeover "+
					"does nothing there", got, before)
			}
		})
	}
}
