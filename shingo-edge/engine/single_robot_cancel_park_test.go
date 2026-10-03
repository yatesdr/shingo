package engine

import (
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
			spare := NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}
			if c.role == protocol.ClaimRoleProduce {
				spare = NodeBinInfo{Occupied: true}
			}
			rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}, "SPOT": spare}
			fx := seedKeepStagedChangeover(t,
				[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", c.role, protocol.SwapModeSingleRobot, c.keepStaged, false}},
				[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", c.role, protocol.SwapModeSingleRobot, c.keepStaged, false}},
				rows)
			startKSChangeover(t, fx)
			// The leg lifted the line's bin and parked it on outbound staging.
			rows["L1"] = NodeBinInfo{}
			rows[out] = NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}
			fx.eng.coreClient = NewCoreClient(ksNodeBinsStub(t, rows).URL)

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
	rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"}}
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
	rows := map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
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
