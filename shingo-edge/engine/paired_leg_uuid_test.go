package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	ordermgr "shingoedge/orders"
	"shingoedge/store"
)

// complexRequestsOnTheWire decodes every pending complex-order envelope.
//
// THE WIRE IS THE ONLY PLACE THIS IS OBSERVABLE. Edge's order row has no
// sibling_order_uuid column — the local pairing is row-id based
// (LinkOrderSiblings) and is not what fails. SiblingOrderUUID rides the
// ComplexOrderRequest, and it is what Core's intake back-link, swap_hold and
// HandleSwapPeerTerminal all read.
func complexRequestsOnTheWire(t *testing.T, db *store.DB) []protocol.ComplexOrderRequest {
	t.Helper()
	msgs, err := db.ListPendingOutbox(100)
	testutil.MustNoErr(t, err, "ListPendingOutbox")
	var out []protocol.ComplexOrderRequest
	for _, m := range msgs {
		if m.MsgType != protocol.TypeComplexOrderRequest {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var req protocol.ComplexOrderRequest
		testutil.MustNoErr(t, env.DecodePayload(&req), "decode ComplexOrderRequest")
		out = append(out, req)
	}
	return out
}

// assertMutuallyPaired is the whole point of unit 3: BOTH legs name the other.
//
// Not "the second one names the first". A one-way link is what the old
// mint-inside-create produced, and Core treats it as no link at all —
// swap_hold checks sib.SiblingOrderUUID == order.EdgeUUID and fails OPEN when
// it doesn't match, which is the starvation hold and the peer-death handler
// both silently switching themselves off.
func assertMutuallyPaired(t *testing.T, reqs []protocol.ComplexOrderRequest) {
	t.Helper()
	if len(reqs) != 2 {
		t.Fatalf("want exactly two complex legs on the wire, got %d", len(reqs))
	}
	a, b := reqs[0], reqs[1]
	for i, r := range reqs {
		if r.SiblingOrderUUID == "" {
			t.Errorf("leg %d (uuid %s) went out with no sibling — creation order decided "+
				"which leg is unpaired, which is exactly what pre-minting removes", i, r.OrderUUID)
		}
	}
	if a.SiblingOrderUUID != b.OrderUUID || b.SiblingOrderUUID != a.OrderUUID {
		t.Errorf("legs are not mutually paired: A(uuid=%s sib=%s) B(uuid=%s sib=%s)",
			a.OrderUUID, a.SiblingOrderUUID, b.OrderUUID, b.SiblingOrderUUID)
	}
	if a.OrderUUID == b.OrderUUID {
		t.Errorf("both legs share uuid %s — pre-minting must mint two", a.OrderUUID)
	}
}

// THE PRODUCE PATH. Leg A (supply) is created first, so before this change it
// was the leg that could not name a sibling that did not exist yet.
func TestApplyProducePlan_BothLegsGoOutPaired(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")

	plan := &ProducePlan{
		Manifest:          []protocol.IngestManifestItem{{PartNumber: "WIDGET-A", Quantity: 1}},
		ProducedAtRFC3339: "2026-08-23T00:00:00Z",
		Dispatch: &SwapDispatch{
			CycleMode:     protocol.SwapModeTwoRobotPressIndex,
			ProcessNode:   claim.CoreNodeName,
			StepsA:        []protocol.ComplexOrderStep{{Action: "pickup", Node: "MARKET"}, {Action: "dropoff", Node: "INDEX-B"}},
			DeliveryNodeA: "INDEX-B",
			StepsB:        []protocol.ComplexOrderStep{{Action: "pickup", Node: claim.CoreNodeName}, {Action: "dropoff", Node: "MARKET"}},
		},
	}

	_, err = eng.applyProducePlan(node, runtime, claim, plan, ordermgr.Attached("episode-pair"))
	testutil.MustNoErr(t, err, "applyProducePlan")

	assertMutuallyPaired(t, complexRequestsOnTheWire(t, db))
	_ = nodeID
}

// THE CHANGEOVER PATH, which needed it more: it created the supply with an
// empty sibling and then stamped the evac with a uuid READ BACK from the
// database, so a swap's pairing also depended on that refetch succeeding.
//
// Driven end-to-end through StartProcessChangeover rather than by calling the
// applier directly — the planner decides which leg is built first, and the
// ordering is precisely the thing under test.
func TestChangeoverApplier_BothLegsGoOutPaired(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	drainAbandonOutbox(t, db)

	_, err := eng.StartProcessChangeover(processID, toStyleID, "test", "sibling pairing")
	testutil.MustNoErr(t, err, "start changeover")

	assertMutuallyPaired(t, complexRequestsOnTheWire(t, db))
}
