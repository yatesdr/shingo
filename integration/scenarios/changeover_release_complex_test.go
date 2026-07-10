// F' Phase 3 — release-is-changeover-independent scenario.
//
// During an active changeover, releasing an order that is NOT linked to
// any changeover_node_task (e.g. a regular production order on a node
// the changeover left as `unchanged`, or a complex order in flight for
// a different process) must fire normally — no interaction with
// HandleBinPickedUp's deferred-supply branch, no interference from the
// changeover gate, no auto-cutover monitor side effects.
//
// Unit-level coverage already exists for the changeover-independent
// release path (TestHandleBinPickedUp_NoOpForNonChangeoverOrder). This
// scenario verifies the same invariant end-to-end through Edge's wire
// encoding, ingestor dispatch, and outbox emission.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"

	"shingo/protocol"

	"shingoedge/orders"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"

	edgeengine "shingoedge/engine"
)

// TestScenario_ReleaseIsChangeoverIndependent validates that a release
// on a non-changeover order during an active changeover:
//
//  1. Fires through ReleaseOrderWithLineside normally — no engine-side
//     gate consults the changeover state for the release path.
//  2. The OrderRelease envelope encodes the operator's disposition
//     correctly through the wire (matches partial_release_test's
//     manifest contract for this wire-shape sanity).
//  3. A subsequent BinPickedUp on that order does NOT trigger any
//     sibling auto-release — the order isn't linked to a
//     changeover_node_task as evac, so HandleBinPickedUp's deferred-
//     supply branch is a no-op.
func TestScenario_ReleaseIsChangeoverIndependent(t *testing.T) {
	const stationID = "edge.test"
	edge := edgeharness.NewEdge(t, stationID)

	// ── Seed: process with TWO nodes. Changeover affects node A.
	// Node B's claim is identical across both styles → "unchanged" task,
	// no changeover-driven orders on node B. We then create an
	// independent retrieve order on node B and exercise its release.
	processID, err := edge.DB.CreateProcess("INDEP-PROC", "release independence", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeA, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "NODE-A", Code: "NA",
		Name: "Node A", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node A: %v", err)
	}
	nodeB, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "NODE-B", Code: "NB",
		Name: "Node B", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node B: %v", err)
	}
	fromStyleID, _ := edge.DB.CreateStyle("From", "from", processID)
	toStyleID, _ := edge.DB.CreateStyle("To", "to", processID)
	if err := edge.DB.SetActiveStyle(processID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	// Node A: from-claim and to-claim differ → changeover creates Phase-3 swap orders.
	if _, err := upsertClaimLegacySimple(edge.DB, processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "NODE-A",
		Role: "consume", SwapMode: "simple",
		PayloadCode: "OLD-A", UOPCapacity: 100,
		InboundSource: "SRC-OLD-A", OutboundStaging: "OUT-STG-A", OutboundDestination: "DST-OLD-A",
	}); err != nil {
		t.Fatalf("upsert from claim A: %v", err)
	}
	if _, err := upsertClaimLegacySimple(edge.DB, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "NODE-A",
		Role: "consume", SwapMode: "simple",
		PayloadCode: "NEW-A", UOPCapacity: 200,
		InboundSource: "SRC-NEW-A", InboundStaging: "IN-STG-A",
	}); err != nil {
		t.Fatalf("upsert to claim A: %v", err)
	}
	// Node B: same payload across both styles → changeover task lands as "unchanged".
	for _, sid := range []int64{fromStyleID, toStyleID} {
		if _, err := upsertClaimLegacySimple(edge.DB, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: "NODE-B",
			Role: "consume", SwapMode: "simple",
			PayloadCode: "STABLE-B", UOPCapacity: 100,
			InboundSource: "SRC-B",
		}); err != nil {
			t.Fatalf("upsert claim B for style %d: %v", sid, err)
		}
	}
	// Runtimes: node A active on from-claim with UOP=50; node B active on its from-claim too.
	for _, nid := range []int64{nodeA, nodeB} {
		if _, err := edge.DB.EnsureProcessNodeRuntime(nid); err != nil {
			t.Fatalf("ensure runtime %d: %v", nid, err)
		}
	}
	claimA, _ := edge.DB.GetStyleNodeClaimByNode(fromStyleID, "NODE-A")
	claimB, _ := edge.DB.GetStyleNodeClaimByNode(fromStyleID, "NODE-B")
	if err := edge.DB.SetProcessNodeRuntime(nodeA, &claimA.ID, 50); err != nil {
		t.Fatalf("runtime A: %v", err)
	}
	if err := edge.DB.SetProcessNodeRuntime(nodeB, &claimB.ID, 80); err != nil {
		t.Fatalf("runtime B: %v", err)
	}

	// ── Pre-changeover: independent retrieve order on node B. ──
	// Represents a normal production order in flight when the operator
	// initiates the changeover. Forced to staged so the release path
	// fires (Manager.ReleaseOrder pre-dispatch guard otherwise).
	indepOrder, err := edge.Engine.OrderManager().CreateRetrieveOrder(&nodeB, false, 1, "NODE-B", "SRC-B", "", "fork", "STABLE-B", false, false)
	if err != nil {
		t.Fatalf("create independent order: %v", err)
	}
	if err := edge.DB.UpdateOrderStatus(indepOrder.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force independent staged: %v", err)
	}

	// ── Start the changeover. Affects node A only; node B task is "unchanged". ──
	changeover, err := edge.Engine.StartProcessChangeover(processID, toStyleID, "test", "release independence")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	taskB, err := edge.DB.GetChangeoverNodeTaskByNode(changeover.ID, nodeB)
	if err != nil {
		t.Fatalf("get task B: %v", err)
	}
	if taskB.Situation != "unchanged" {
		t.Fatalf("expected task B situation=unchanged, got %q (test premise)", taskB.Situation)
	}

	drainOutbox(t, edge)

	// ── Release the independent order. Mid-changeover. ──
	if err := edge.Engine.ReleaseOrderWithLineside(indepOrder.ID, edgeengine.ReleaseDisposition{
		CalledBy: "indep-release-test",
	}); err != nil {
		t.Fatalf("release independent order during changeover: %v", err)
	}

	releases := pendingReleases(t, edge)
	if len(releases) != 1 {
		t.Fatalf("after release: got %d envelopes, want 1 (the independent order, no chained auto-release)", len(releases))
	}
	if releases[0].OrderUUID != indepOrder.UUID {
		t.Errorf("released wrong order: got %s, want independent %s", releases[0].OrderUUID, indepOrder.UUID)
	}

	// ── BinPickedUp arrives for the independent order. No sibling chain. ──
	bpEnv, err := protocol.NewDataEnvelope(
		protocol.SubjectBinPickedUp,
		protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		&protocol.BinPickedUp{
			OrderUUID: indepOrder.UUID, BinID: 7777, Location: "NODE-B",
			PickedUpAt: time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatalf("build BinPickedUp envelope: %v", err)
	}
	encoded, _ := bpEnv.Encode()
	edge.Ingestor.HandleRaw(encoded)

	releases = pendingReleases(t, edge)
	if len(releases) != 0 {
		t.Errorf("BinPickedUp on independent order produced %d auto-releases, want 0 (the order has no changeover_node_task evac linkage)", len(releases))
	}
}
