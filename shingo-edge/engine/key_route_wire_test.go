package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	ordermgr "shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// setClaimRouting adds a key route to the seeded claim, the way an operator
// saving the Routing fieldset would.
func setClaimRouting(t *testing.T, db *store.DB, claim *processes.NodeClaim, route []string, task string) {
	t.Helper()
	in := processes.NodeClaimInput{
		StyleID:             claim.StyleID,
		CoreNodeName:        claim.CoreNodeName,
		Role:                claim.Role,
		SwapMode:            claim.SwapMode,
		PayloadCode:         claim.PayloadCode,
		UOPCapacity:         claim.UOPCapacity,
		InboundSource:       claim.InboundSource,
		InboundStaging:      claim.InboundStaging,
		OutboundStaging:     claim.OutboundStaging,
		OutboundDestination: claim.OutboundDestination,
		PairedCoreNode:      claim.PairedCoreNode,
		KeyRoute:            route,
		KeyTask:             task,
	}
	_, err := db.UpsertStyleNodeClaim(in)
	testutil.MustNoErr(t, err, "set claim routing")
}

func producePlanFor(claim *processes.NodeClaim) *ProducePlan {
	return &ProducePlan{
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
}

// THE CLAIM IS THE SEAM. Routing is not a parameter on the create call — it is
// read from the claim at create time, the same way and at the same moment as
// the payload. This is what makes that true rather than merely intended.
//
// Order matters and is asserted as a sequence: SEER walks the points in order,
// and a route stored as a set would be a different route.
func TestComplexOrder_CarriesTheClaimsKeyRoute(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	setClaimRouting(t, db, claim, []string{"AISLE_B", "AISLE_A"}, "load")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")
	claim = findActiveClaim(db, node)

	_, err = eng.applyProducePlan(node, runtime, claim, producePlanFor(claim), ordermgr.Attached("episode-route"))
	testutil.MustNoErr(t, err, "applyProducePlan")

	reqs := complexRequestsOnTheWire(t, db)
	if len(reqs) != 2 {
		t.Fatalf("want two legs on the wire, got %d", len(reqs))
	}
	// BOTH legs carry it. The route describes how the cell is reached, not
	// which leg is which, so a version that stamped only the leg whose claim
	// was consulted would send one robot the long way.
	for i, r := range reqs {
		if len(r.KeyRoute) != 2 || r.KeyRoute[0] != "AISLE_B" || r.KeyRoute[1] != "AISLE_A" {
			t.Errorf("leg %d KeyRoute = %v, want [AISLE_B AISLE_A] in that order", i, r.KeyRoute)
		}
		if r.KeyTask != "load" {
			t.Errorf("leg %d KeyTask = %q, want \"load\"", i, r.KeyTask)
		}
	}
}

// THE DEFAULT MUST STAY EXACTLY WHAT IT WAS. Every claim in the plant has no
// route, and an empty keyRoute is what makes SEER auto-pick. A nil that became
// an empty-but-present array, or a "" that became something else, would change
// every order in the plant on the strength of a feature nobody configured.
func TestComplexOrder_NoClaimRouteSendsNothing(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)
	runtime, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "get runtime")

	_, err = eng.applyProducePlan(node, runtime, claim, producePlanFor(claim), ordermgr.Attached("episode-plain"))
	testutil.MustNoErr(t, err, "applyProducePlan")

	for i, r := range complexRequestsOnTheWire(t, db) {
		if len(r.KeyRoute) != 0 {
			t.Errorf("leg %d sent KeyRoute %v for a claim that configured none", i, r.KeyRoute)
		}
		if r.KeyTask != "" {
			t.Errorf("leg %d sent KeyTask %q for a claim that configured none", i, r.KeyTask)
		}
	}
	_ = nodeID
}
