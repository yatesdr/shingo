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
// confirms it. A cancel in that window leaves the leg as it is: the abort
// refuses a delivered order (orders.ErrDelivered), so the cancel sends nothing
// for it and the leg keeps its Confirm, and it counts no park for a leg it did
// not abort (taskLeftOnStaging), so a bin Core reports on outbound staging is
// not moved for it. The style stays the outgoing one and the changeover ends
// cancelled; the leg's confirm, when it comes, changes nothing at the line (no
// changeover is open to complete). Until 2026-11-25 (characterised at
// 37146992) the cancel aborted the leg as a live one, with the operator's
// default reason, and counted its park by the status it read before the abort
// answered, sending a bin on outbound staging on to the outbound destination.
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
			before := orderHistory(t, fx.db, leg)

			testutil.MustNoErr(t, fx.eng.CancelProcessChangeover(fx.processID), "cancel")

			// What it sends: nothing for the delivered leg.
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
			expectRows(t, "the order.cancel rows queued", sent, nil)

			// What the operator sees: the leg still delivered, so the Orders page
			// offers its Confirm, and the changeover cancelled.
			if o := orderRow(t, fx.db, leg); o.Status != protocol.StatusDelivered {
				t.Errorf("the delivered leg reads %s after the cancel, want delivered: its abort is refused", o.Status)
			}
			expectRows(t, "the leg's history", orderHistory(t, fx.db, leg), before)
			co, err := fx.db.GetProcessChangeoverByID(coID)
			testutil.MustNoErr(t, err, "read the changeover")
			if co.State != domain.ChangeoverCancelled {
				t.Errorf("the changeover reads %s, want cancelled", co.State)
			}

			// What it counts, and where the bin on outbound staging ends: no park
			// for a leg the cancel did not abort, so nothing is moved for it.
			if moves := parkMoves(t, fx, out); len(moves) != 0 {
				t.Errorf("moves off %s = %+v, want none: the delivered leg's park is not counted", out, moves)
			}

			// The leg's confirm, as its auto-confirm makes it once Core's
			// delivered facts land: the line stays as the cancel left it.
			proc, err := fx.db.GetProcess(fx.processID)
			testutil.MustNoErr(t, err, "read the process")
			rt, err := fx.db.GetProcessNodeRuntime(fx.nodeIDs["L1"])
			testutil.MustNoErr(t, err, "read the line's runtime")
			live, err := fx.db.ListActiveOrders()
			testutil.MustNoErr(t, err, "read the live orders")
			testutil.MustNoErr(t, fx.eng.OrderManager().ConfirmDelivery(leg, 1), "the leg's confirm")
			if o := orderRow(t, fx.db, leg); o.Status != protocol.StatusConfirmed {
				t.Errorf("the leg reads %s after its confirm, want confirmed", o.Status)
			}
			after, err := fx.db.GetProcess(fx.processID)
			testutil.MustNoErr(t, err, "read the process")
			if styleOf(after.ActiveStyleID) != styleOf(proc.ActiveStyleID) || after.TargetStyleID != nil {
				t.Errorf("after the leg's confirm the process runs style %s, target %s; want %s, none",
					styleOf(after.ActiveStyleID), styleOf(after.TargetStyleID), styleOf(proc.ActiveStyleID))
			}
			rtAfter, err := fx.db.GetProcessNodeRuntime(fx.nodeIDs["L1"])
			testutil.MustNoErr(t, err, "read the line's runtime")
			if styleOf(rtAfter.ActiveClaimID) != styleOf(rt.ActiveClaimID) {
				t.Errorf("after the leg's confirm the line points at claim %s, want %s as the cancel left it",
					styleOf(rtAfter.ActiveClaimID), styleOf(rt.ActiveClaimID))
			}
			liveAfter, err := fx.db.ListActiveOrders()
			testutil.MustNoErr(t, err, "read the live orders")
			had := map[int64]bool{}
			for _, o := range live {
				had[o.ID] = true
			}
			for _, o := range liveAfter {
				if !had[o.ID] {
					t.Errorf("the leg's confirm made order %d (%s %s->%s), want nothing new", o.ID, o.OrderType,
						o.SourceNode, o.DeliveryNode)
				}
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
