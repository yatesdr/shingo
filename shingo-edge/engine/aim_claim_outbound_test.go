package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// aim_claim_outbound_test.go — the Edge aims every leg at the outbound of the
// claim the leg is for, whatever stands on the cell. A carrier of another
// part is Core's to send home: Core holds the bin and, at the lift, sends a
// carrier aimed at another part's outbound to its own claim's
// (shingo-core dispatch's by-contents choice, TestByContents_ACarrierOfAnotherPartGoesToItsOwnHome).

// residentClaimFixture is ONE line node two styles take turns on, each with its
// own home as both source and destination. Returns the two claim ids and the
// process node id.
func residentClaimFixture(t *testing.T, db *store.DB) (nodeID, oldClaimID, newClaimID int64) {
	t.Helper()
	processID, nodeID := seedProcessNode(t, db)
	oldStyle, err := db.CreateStyle("STYLE-OLD", "outgoing", processID)
	testutil.MustNoErr(t, err, "create outgoing style")
	newStyle, err := db.CreateStyle("STYLE-NEW", "incoming", processID)
	testutil.MustNoErr(t, err, "create incoming style")
	oldClaimID, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: oldStyle, CoreNodeName: "TEST-NODE", Role: "consume",
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-OLD", UOPCapacity: 100,
		InboundSource: "HOME-OLD", InboundStaging: "STAGE-IN", OutboundStaging: "STAGE-OUT",
		OutboundDestination: "HOME-OLD",
	})
	testutil.MustNoErr(t, err, "upsert outgoing claim")
	newClaimID, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: newStyle, CoreNodeName: "TEST-NODE", Role: "consume",
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-NEW", UOPCapacity: 100,
		InboundSource: "HOME-NEW", InboundStaging: "STAGE-IN", OutboundStaging: "STAGE-OUT",
		OutboundDestination: "HOME-NEW",
	})
	testutil.MustNoErr(t, err, "upsert incoming claim")
	// The runtime row is created on first use in production; the setters below
	// are UPDATEs and silently affect nothing without it.
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime row")
	return nodeID, oldClaimID, newClaimID
}

// runIncomingWithOutgoingCarrier puts the process on the incoming style with
// the outgoing style's carrier still standing on the cell, as Core reported it.
func runIncomingWithOutgoingCarrier(t *testing.T, e *Engine, db *store.DB, nodeID, oldClaimID, newClaimID int64) {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	newClaim, err := db.GetStyleNodeClaim(newClaimID)
	testutil.MustNoErr(t, err, "read incoming claim")
	testutil.MustNoErr(t, db.SetActiveStyle(node.ProcessID, &newClaim.StyleID), "set active style")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &oldClaimID, nil, 0), "seat the outgoing carrier")
	holdPartAt(t, e, nodeID, "TEST-NODE", "PART-OLD")
}

// TestRequest_AForeignCarrierIsAimedAtTheRequestedClaimsOutbound: a REQUEST on a
// cell still holding the outgoing style's carrier builds the swap from the
// requested claim: its evac leg is aimed at the requested claim's outbound.
func TestRequest_AForeignCarrierIsAimedAtTheRequestedClaimsOutbound(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, oldClaimID, newClaimID := residentClaimFixture(t, db)
	runIncomingWithOutgoingCarrier(t, e, db, nodeID, oldClaimID, newClaimID)
	bridgeCore(t, e, occupiedStub(t).URL)

	res, err := e.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "REQUEST")
	if res.OrderB == nil {
		t.Fatalf("the REQUEST made no evac leg: %+v", res)
	}
	var evac []protocol.ComplexOrderStep
	for _, r := range complexRequestsOnTheWire(t, db) {
		if r.OrderUUID == res.OrderB.UUID {
			evac = r.Steps
		}
	}
	if dest := finalDropoff(evac); dest != "HOME-NEW" {
		t.Errorf("the evac leg is aimed at %q, want HOME-NEW, the requested claim's outbound", dest)
	}
}

// TestReleaseNodeWithClaim_AimsAtTheClaimsOutbound: the Material page's release
// of a cell holding the outgoing style's carrier is a move to the claim's
// outbound, carrying the carrier's own part.
func TestReleaseNodeWithClaim_AimsAtTheClaimsOutbound(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, oldClaimID, newClaimID := residentClaimFixture(t, db)
	runIncomingWithOutgoingCarrier(t, e, db, nodeID, oldClaimID, newClaimID)

	order, err := e.releaseNodeWithClaim(nodeID, 1, nil, nil)
	testutil.MustNoErr(t, err, "release")
	if order.DeliveryNode != "HOME-NEW" || order.PayloadCode != "PART-OLD" {
		t.Errorf("the release is a move to %q carrying %q, want HOME-NEW carrying PART-OLD",
			order.DeliveryNode, order.PayloadCode)
	}
}

// TestReleaseNodeWithClaim_SameStyleResidentIsUnchanged: when the resident IS
// the requested style — every routine release in the plant — the destination
// is the claim's own.
func TestReleaseNodeWithClaim_SameStyleResidentIsUnchanged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read node")
	newClaim, err := db.GetStyleNodeClaim(newClaimID)
	testutil.MustNoErr(t, err, "read incoming claim")
	testutil.MustNoErr(t, db.SetActiveStyle(node.ProcessID, &newClaim.StyleID), "set active style")
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &newClaimID, nil, 0), "seat the matching carrier")

	order, err := e.releaseNodeWithClaim(nodeID, 1, nil, nil)
	testutil.MustNoErr(t, err, "release")
	if order.DeliveryNode != "HOME-NEW" {
		t.Errorf("release sent the carrier to %q, want HOME-NEW, the claim's own outbound", order.DeliveryNode)
	}
}
