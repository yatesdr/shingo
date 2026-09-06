package engine

import (
	"testing"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// THE FOURTH DOOR.
//
// 1b17b0f9 taught the three swap builders to send an outgoing carrier to the
// home it belongs to rather than the home the request names. releaseNodeWithClaim
// opens the same kind of leg — a bin leaving the cell — and was not taught,
// because it does not build a swap. Core does not net it either: it mints a
// move order, and move orders are planned by planTransport, which never reaches
// the park-side guard that refuses a mismatched carrier at a pinned home.
//
// This is SMN_029's shape through the one door both defences miss.
func TestReleaseNodeWithClaim_RoutesTheResidentCarrierHome(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, oldClaimID, newClaimID := residentClaimFixture(t, db)

	// The process has moved on to the incoming style; the outgoing style's
	// carrier is still standing on the cell.
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}
	newClaim, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read incoming claim: %v", err)
	}
	if err := db.SetActiveStyle(node.ProcessID, &newClaim.StyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &oldClaimID, nil, 0); err != nil {
		t.Fatalf("seat the outgoing carrier: %v", err)
	}

	order, err := e.releaseNodeWithClaim(nodeID, 1, nil, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if order.DeliveryNode != "HOME-OLD" {
		t.Fatalf("release sent the carrier to %q, want HOME-OLD. The bin on the cell belongs to "+
			"the OUTGOING style; HOME-NEW is the incoming style's dedicated home and parking a "+
			"foreign carrier there is SPR 2026-09-02 exactly. runtime has been in scope at the "+
			"top of this function the whole time.", order.DeliveryNode)
	}
}

// The ordinary case must be untouched: when the resident IS the requested
// style — every routine release in the plant — the destination is the claim's
// own, and nothing new happens.
func TestReleaseNodeWithClaim_SameStyleResidentIsUnchanged(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)

	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}
	newClaim, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read incoming claim: %v", err)
	}
	if err := db.SetActiveStyle(node.ProcessID, &newClaim.StyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &newClaimID, nil, 0); err != nil {
		t.Fatalf("seat the matching carrier: %v", err)
	}

	order, err := e.releaseNodeWithClaim(nodeID, 1, nil, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if order.DeliveryNode != "HOME-NEW" {
		t.Errorf("release sent the carrier to %q, want HOME-NEW — the resident and the requested "+
			"style agree, so this is the ordinary path and must be byte-identical to before.",
			order.DeliveryNode)
	}
}

// residentEvacDest asks "where does THIS carrier live". It used to answer with
// EvacDestinationFor, which prefers ChangeoverEvacDestination — the TOOLING
// clearance destination. This path runs on every routine consume and produce
// swap, so preferring the tooling field would send an ordinary outgoing carrier
// to the tooling area whenever a cell happened to hold a foreign style. That is
// a different wrong place, not a right one.
func TestResidentEvacDest_IgnoresTheToolingDestination(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, oldClaimID, newClaimID := residentClaimFixture(t, db)

	// Give the OUTGOING claim a tooling-clearance destination. Empty at both
	// plants today, which is a fuse length, not a verdict.
	old, err := db.GetStyleNodeClaim(oldClaimID)
	if err != nil {
		t.Fatalf("read outgoing claim: %v", err)
	}
	in := processes.NodeClaimInput{
		StyleID: old.StyleID, CoreNodeName: old.CoreNodeName, Role: old.Role,
		SwapMode: old.SwapMode, PayloadCode: old.PayloadCode, UOPCapacity: old.UOPCapacity,
		InboundSource: old.InboundSource, InboundStaging: old.InboundStaging,
		OutboundStaging: old.OutboundStaging, OutboundDestination: old.OutboundDestination,
		ChangeoverEvacDestination: domain.Ptr("TOOLING-CLEARANCE"),
	}
	if _, err := db.UpsertStyleNodeClaim(in); err != nil {
		t.Fatalf("set the tooling destination: %v", err)
	}

	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &oldClaimID, nil, 0); err != nil {
		t.Fatalf("seat the outgoing carrier: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	requested, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read requested claim: %v", err)
	}

	if got := e.residentEvacDest(rt, requested); got != "HOME-OLD" {
		t.Fatalf("residentEvacDest = %q, want HOME-OLD. This is a routine swap, not a changeover: "+
			"the carrier goes to the home it lives at, never to the tooling clearance area.", got)
	}
}
