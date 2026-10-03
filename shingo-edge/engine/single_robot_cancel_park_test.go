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
