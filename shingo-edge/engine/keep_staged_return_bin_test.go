package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
)

// A SPARE'S RETURN NAMES THE BIN IT IS FOR.
//
// The read that decided the return saw the bin standing on the spot, and the
// return is for that bin. It travels on the move as the bin's Core id, so a
// return still waiting when something else lifts the spare lifts nothing,
// instead of the refill that lands after it. Every other move names no bin.

// sentMoves is every move request in the outbox, by its source node.
func sentMoves(t *testing.T, db *store.DB) map[string][]protocol.OrderRequest {
	t.Helper()
	out := map[string][]protocol.OrderRequest{}
	for _, m := range findOutboxByType(t, db, protocol.TypeOrderRequest) {
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "envelope")
		var req protocol.OrderRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "order request")
		if req.OrderType == protocol.OrderTypeMove {
			out[req.SourceNode] = append(out[req.SourceNode], req)
		}
	}
	return out
}

func TestKeepStagedReturn_NamesTheBinItWasFor(t *testing.T) {
	t.Parallel()

	t.Run("request", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart},
				ksSpot: {Occupied: true, PayloadCode: "PART-OTHER", BinID: 77}})
		_, err := eng.RequestNodeMaterial(nodeID, 1)
		testutil.MustNoErr(t, err, "request")
		if got := sentMoves(t, db)[ksSpot]; len(got) != 1 || got[0].BinID != 77 {
			t.Fatalf("moves off the spot = %+v, want one return naming bin 77", got)
		}
	})

	t.Run("changeover start", func(t *testing.T) {
		t.Parallel()
		fx := seedKeepStagedChangeover(t,
			[]coClaim{{"L1", "SPOT", "SRC-OLD", "PART-OLD", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
			[]coClaim{{"L1", "SPOT", "SRC-NEW", "PART-NEW", protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot, true, false}},
			map[string]NodeBinInfo{"L1": {Occupied: true, PayloadCode: "PART-OLD"},
				"SPOT": {Occupied: true, PayloadCode: "PART-OLD", BinID: 88}})
		startKSChangeover(t, fx)
		if got := sentMoves(t, fx.db)["SPOT"]; len(got) != 1 || got[0].BinID != 88 {
			t.Fatalf("moves off the spot = %+v, want one return naming bin 88", got)
		}
	})

	// The line's delivery from the spot is not a return: it names no bin.
	t.Run("a delivery from the spot names none", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
			map[string]NodeBinInfo{ksLine: {}, ksSpot: {Occupied: true, PayloadCode: ksPart, BinID: 99}})
		_, err := eng.RequestNodeMaterial(nodeID, 1)
		testutil.MustNoErr(t, err, "request")
		got := sentMoves(t, db)[ksSpot]
		if len(got) != 1 || got[0].DeliveryNode != ksLine || got[0].BinID != 0 {
			t.Fatalf("moves off the spot = %+v, want the one delivery to the line, naming no bin", got)
		}
	})
}
