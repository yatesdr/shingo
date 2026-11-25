package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// A CANCELLED SINGLE-ROBOT CHANGEOVER AND THE LINE'S BIN ON OUTBOUND STAGING.
//
// The single-robot changeover leg lifts the line's bin, parks it on the
// outgoing claim's outbound staging, collects the incoming bin, delivers it,
// and only then takes the parked bin on to the outbound destination. Cancelled
// in between, the leg is aborted and the parked bin stands on outbound staging,
// which every later single-robot swap at the line drops on. The cancel finishes
// that bin's trip: one plain move from outbound staging to where the leg was
// taking it, carrying the bin's own payload, attributed to the line.

// parkMoves is every live plain move off a node.
func parkMoves(t *testing.T, fx *coFixture, from string) []domain.Order {
	t.Helper()
	live, err := fx.db.ListActiveOrders()
	testutil.MustNoErr(t, err, "orders")
	var out []domain.Order
	for _, o := range live {
		if o.OrderType == orders.TypeMove && o.SourceNode == from {
			out = append(out, o)
		}
	}
	return out
}

func TestSingleRobotCancel_FinishesTheParkedBinsTrip(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		role       protocol.ClaimRole
		keepStaged bool
	}{
		{"consume", protocol.ClaimRoleConsume, false},
		{"produce", protocol.ClaimRoleProduce, false},
		{"consume keep-staged", protocol.ClaimRoleConsume, true},
		{"produce keep-staged", protocol.ClaimRoleProduce, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			const out = "KSCO-OUT-L1"
			spare := protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}
			if c.role == protocol.ClaimRoleProduce {
				spare = protocol.NodeBinInfo{Occupied: true}
			}
			rows := map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}, "SPOT": spare}
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", c.role, protocol.SwapModeSingleRobot, c.keepStaged, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", c.role, protocol.SwapModeSingleRobot, c.keepStaged, false}},
				rows)
			startKSChangeover(t, fx)
			// The leg lifted the line's bin and parked it on outbound staging.
			rows["L1"] = protocol.NodeBinInfo{}
			rows[out] = protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}
			bridgeCore(t, fx.eng, ksNodeBinsStub(t, rows).URL)
			fx.holdRows(t, rows)

			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

			moves := parkMoves(t, fx, out)
			if len(moves) != 1 {
				t.Fatalf("moves off %s = %d, want 1: the parked bin's trip finished", out, len(moves))
			}
			m := moves[0]
			if m.DeliveryNode != "KSCO-DEST" || m.PayloadCode != "PART-OLD" || !m.AutoConfirm {
				t.Errorf("the move goes %s->%s carrying %q autoConfirm=%v, want ->KSCO-DEST carrying PART-OLD, auto-confirmed",
					m.SourceNode, m.DeliveryNode, m.PayloadCode, m.AutoConfirm)
			}
			if m.ProcessNodeID == nil || *m.ProcessNodeID != fx.nodeIDs["L1"] {
				t.Errorf("the move is attributed to %v, want the line L1 (%d)", m.ProcessNodeID, fx.nodeIDs["L1"])
			}
			rt, err := fx.db.GetProcessNodeRuntime(fx.nodeIDs["L1"])
			testutil.MustNoErr(t, err, "runtime")
			for _, slot := range []*int64{rt.ActiveOrderID, rt.StagedOrderID} {
				if slot != nil && *slot == m.ID {
					t.Errorf("the move %d sits in the line's runtime slots", m.ID)
				}
			}
		})
	}
}

// Nothing parked, nothing moved: the cancel came before the leg lifted the
// line's bin, or after it took the bin off outbound staging again.
func TestSingleRobotCancel_NothingParkedMovesNothing(t *testing.T) {
	t.Parallel()
	rows := map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}}
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false, false}},
		rows)
	startKSChangeover(t, fx)
	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")
	if moves := parkMoves(t, fx, "KSCO-OUT-L1"); len(moves) != 0 {
		t.Fatalf("moves off outbound staging = %+v, want none: nothing stands there", moves)
	}
}

// A two-robot changeover parks nothing: its evac goes straight to the outbound
// destination, and its supply's stop on inbound staging is the incoming bin's,
// which a cancel must not carry on to the line.
func TestTwoRobotCancel_MovesNothingOffStaging(t *testing.T) {
	t.Parallel()
	rows := map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
		"SPOT": {Occupied: true, PayloadCode: "PART-NEW"}}
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, false, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, false, false}},
		rows)
	startKSChangeover(t, fx)
	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")
	if moves := parkMoves(t, fx, "SPOT"); len(moves) != 0 {
		t.Fatalf("moves off inbound staging = %+v, want none", moves)
	}
}

// THE INCOMING BIN LEFT ON INBOUND STAGING. A single-robot changeover onto a
// claim that keeps no spare is one order: it parks the incoming bin on inbound
// staging first, and collects it from there itself after parking the line's
// bin. Cancelled with the robot past the park and before the collect, the bin
// stands on inbound staging, where every later swap at the line sets its own
// incoming bin down. The cancel sends it back to the incoming claim's inbound
// source, by the same plain move as the park, carrying its own payload.
func TestSingleRobotCancel_SendsTheStagedBinBack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		role   protocol.ClaimRole
		status protocol.Status
	}{
		{"consume, at the line's wait", protocol.ClaimRoleConsume, protocol.StatusStaged},
		{"consume, past the wait", protocol.ClaimRoleConsume, protocol.StatusInTransit},
		{"produce, at the line's wait", protocol.ClaimRoleProduce, protocol.StatusStaged},
		{"produce, past the wait", protocol.ClaimRoleProduce, protocol.StatusInTransit},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			incoming := protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-NEW"}
			if c.role == protocol.ClaimRoleProduce {
				incoming = protocol.NodeBinInfo{Occupied: true}
			}
			rows := map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}, "SPOT": incoming}
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", c.role, protocol.SwapModeSingleRobot, false, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", c.role, protocol.SwapModeSingleRobot, false, false}},
				rows)
			coID := startKSChangeover(t, fx)
			leg := singleRobotLegOf(t, fx, coID)
			testutil.MustNoErr(t, fx.db.UpdateOrderStatus(leg, string(c.status)), "the robot is past the park")

			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

			moves := parkMoves(t, fx, "SPOT")
			if len(moves) != 1 {
				t.Fatalf("moves off inbound staging = %d, want 1: the staged bin sent back", len(moves))
			}
			m := moves[0]
			if m.DeliveryNode != "SRC-NEW" || m.PayloadCode != incoming.PayloadCode || !m.AutoConfirm {
				t.Errorf("the move goes %s->%s carrying %q autoConfirm=%v, want ->SRC-NEW carrying %q, auto-confirmed",
					m.SourceNode, m.DeliveryNode, m.PayloadCode, m.AutoConfirm, incoming.PayloadCode)
			}
			if m.ProcessNodeID == nil || *m.ProcessNodeID != fx.nodeIDs["L1"] {
				t.Errorf("the move is attributed to %v, want the line L1 (%d)", m.ProcessNodeID, fx.nodeIDs["L1"])
			}
		})
	}
}

// An order that never reached a robot parked nothing: a bin Core reports on
// inbound staging then is not the changeover's, and the cancel leaves it.
func TestSingleRobotCancel_StageNotYetFlownMovesNothing(t *testing.T) {
	t.Parallel()
	rows := map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
		"SPOT": {Occupied: true, PayloadCode: "PART-OTHER"}}
	fx := seedKeepStagedChangeover(t,
		[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false, false}},
		[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false, false}},
		rows)
	startKSChangeover(t, fx)
	testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")
	if moves := parkMoves(t, fx, "SPOT"); len(moves) != 0 {
		t.Fatalf("moves off inbound staging = %+v, want none", moves)
	}
}

// THE CANCEL MEETING A DELIVERED LEG (final brief 1e). A changeover leg is
// minted auto-confirmed, so the Edge confirms it as Core's delivered lands; it
// reads delivered for longer only when Core's delivered facts land late, or an
// Edge restart drops them (orders coreDelivered), and then until Core's sweep
// confirms it. A cancel in that window, characterised at 37146992 before the
// abort refused a delivered order: the cancel aborts the leg as it aborts a
// live one (taskLeftOnStaging), sending its order.cancel with the operator's
// default reason and ending its row cancelled, so the Orders page offers it
// nothing. It counts the leg's park and stage by the status it read before the
// abort answered, so a bin Core reports on outbound staging is sent on to the
// outbound destination, though the leg had already taken the line's bin there
// itself. Nothing moves when staging is empty. The style stays the outgoing
// one and the changeover ends cancelled.
func TestSingleRobotCancel_ADeliveredLeg(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		onPark bool // Core reports a bin on outbound staging
	}{
		{"the leg's trip finished", false},
		{"a bin stands on outbound staging", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			const out = "KSCO-OUT-L1"
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot, false, false}},
				map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}})
			coID := startKSChangeover(t, fx)
			leg := singleRobotLegOf(t, fx, coID)
			testutil.MustNoErr(t, fx.db.UpdateOrderStatus(leg, string(protocol.StatusDelivered)), "the leg delivered")
			// What Core reports once the leg has delivered: the incoming bin on
			// the line, the line's old bin at the outbound destination, staging
			// empty but for the case's bin.
			rows := map[string]protocol.NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-NEW"},
				"KSCO-DEST": {Occupied: true, PayloadCode: "PART-OLD"}}
			if c.onPark {
				rows[out] = protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-OTHER"}
			}
			bridgeCore(t, fx.eng, ksNodeBinsStub(t, rows).URL)
			fx.holdRows(t, rows)
			uuid := orderRow(t, fx.db, leg).UUID

			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

			// What it sends.
			msgs, err := fx.db.ListUnsentOutboxByType([]string{protocol.TypeOrderCancel})
			testutil.MustNoErr(t, err, "read the outbox")
			var sent []string
			for _, m := range msgs {
				var env protocol.Envelope
				testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode an outbox envelope")
				var cancel protocol.OrderCancel
				testutil.MustNoErr(t, env.DecodePayload(&cancel), "decode an order.cancel")
				sent = append(sent, cancel.OrderUUID+" "+cancel.Reason)
			}
			expectRows(t, "the order.cancel rows queued", sent, []string{uuid + " aborted by operator"})

			// What the operator sees: the leg's row, and the changeover.
			if o := orderRow(t, fx.db, leg); o.Status != protocol.StatusCancelled {
				t.Errorf("the delivered leg reads %s after the cancel, want cancelled", o.Status)
			}
			if h := orderHistory(t, fx.db, leg); len(h) == 0 || h[len(h)-1] != "delivered->cancelled: aborted by operator" {
				t.Errorf("the leg's history %q, want it to end delivered->cancelled with the operator's default reason", h)
			}
			co, err := fx.db.GetProcessChangeoverByID(coID)
			testutil.MustNoErr(t, err, "read the changeover")
			if co.State != domain.ChangeoverCancelled {
				t.Errorf("the changeover reads %s, want cancelled", co.State)
			}

			// What it counts, and where the bin on outbound staging ends.
			moves := parkMoves(t, fx, out)
			if !c.onPark {
				if len(moves) != 0 {
					t.Errorf("moves off %s = %+v, want none: nothing stands there", out, moves)
				}
				return
			}
			if len(moves) != 1 || moves[0].DeliveryNode != "KSCO-DEST" || moves[0].PayloadCode != "PART-OTHER" {
				t.Errorf("moves off %s = %+v, want one to KSCO-DEST carrying PART-OTHER: the leg's park counted", out, moves)
			}
		})
	}
}

// singleRobotLegOf is the single-robot changeover's one order: its one task's
// swap, which parks the incoming bin on inbound staging (SPOT) itself.
func singleRobotLegOf(t *testing.T, fx *coFixture, coID int64) int64 {
	t.Helper()
	tasks, err := fx.db.ListChangeoverNodeTasks(coID)
	testutil.MustNoErr(t, err, "tasks")
	for _, task := range tasks {
		if task.NextMaterialOrderID != nil {
			t.Fatalf("task %d links a stage order %d: the single robot's changeover is one order", task.ID, *task.NextMaterialOrderID)
		}
		if task.OldMaterialReleaseOrderID != nil {
			return *task.OldMaterialReleaseOrderID
		}
	}
	t.Fatalf("no order on the changeover's tasks")
	return 0
}
