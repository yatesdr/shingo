// F' Phase 3 — multi-node changeover release scenario.
//
// Two two-robot nodes simultaneously mid-changeover. Each node has its
// own paired evac+supply orders; releasing one node's evac must fire
// only that node's supply on pickup-confirm — never the other node's.
// This is the regression lock against crosstalk in
// GetChangeoverNodeTaskByEvacOrderID's lookup or in HandleBinPickedUp's
// per-event dispatch.
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

// TestScenario_MultiNodeChangeover_DeferredSupplyChainsAreIsolated
// validates that two simultaneous Phase-3 swap pairs don't crosstalk:
// pickup on node-A's evac releases node-A's supply only, pickup on
// node-B's evac releases node-B's supply only. No mixing.
//
// What this catches that unit tests miss:
//   - GetChangeoverNodeTaskByEvacOrderID's `WHERE old_material_release_order_id=? LIMIT 1`
//     under multi-task pressure (regression risk if the predicate ever
//     drifted to a wider WHERE that could collide).
//   - HandleBinPickedUp dispatch under back-to-back BinPickedUp events
//     for distinct evacs — verifies no shared state leaks between
//     invocations.
//   - End-to-end through the wire on both pickup deliveries.
func TestScenario_MultiNodeChangeover_DeferredSupplyChainsAreIsolated(t *testing.T) {
	const stationID = "edge.test"
	edge := edgeharness.NewEdge(t, stationID)

	// ── Seed: process + two nodes, full Phase-3 staging on each ──
	processID, err := edge.DB.CreateProcess("MULTI-PROC", "multi-node phase3", "active_production", "", "", false, false)
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
	fromStyleID, err := edge.DB.CreateStyle("From", "from", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := edge.DB.CreateStyle("To", "to", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	for _, nodeName := range []string{"NODE-A", "NODE-B"} {
		if _, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: fromStyleID, CoreNodeName: nodeName,
			Role: "consume", SwapMode: "simple",
			PayloadCode: "OLD-" + nodeName, UOPCapacity: 100,
			InboundSource: "SRC-OLD-" + nodeName,
			OutboundStaging: "OUT-STG-" + nodeName,
			OutboundDestination: "DST-OLD-" + nodeName,
		}); err != nil {
			t.Fatalf("upsert from claim %s: %v", nodeName, err)
		}
		if _, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: toStyleID, CoreNodeName: nodeName,
			Role: "consume", SwapMode: "simple",
			PayloadCode: "NEW-" + nodeName, UOPCapacity: 200,
			InboundSource: "SRC-NEW-" + nodeName,
			InboundStaging: "IN-STG-" + nodeName,
		}); err != nil {
			t.Fatalf("upsert to claim %s: %v", nodeName, err)
		}
	}
	// Initialize runtimes with the from-claim active and a non-zero UOP
	// so both evacs auto-detect as send_partial_back.
	for _, nodeID := range []int64{nodeA, nodeB} {
		if _, err := edge.DB.EnsureProcessNodeRuntime(nodeID); err != nil {
			t.Fatalf("ensure runtime %d: %v", nodeID, err)
		}
	}
	// Resolve the from-claim IDs by node + style. Easier than threading
	// them out of the upsert loop above.
	for _, ent := range []struct {
		nodeID   int64
		nodeName string
	}{{nodeA, "NODE-A"}, {nodeB, "NODE-B"}} {
		c, err := edge.DB.GetStyleNodeClaimByNode(fromStyleID, ent.nodeName)
		if err != nil {
			t.Fatalf("get from claim for %s: %v", ent.nodeName, err)
		}
		if err := edge.DB.SetProcessNodeRuntime(ent.nodeID, &c.ID, 50); err != nil {
			t.Fatalf("set runtime for %s: %v", ent.nodeName, err)
		}
	}

	changeover, err := edge.Engine.StartProcessChangeover(processID, toStyleID, "test", "multi-node scenario")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	taskA, err := edge.DB.GetChangeoverNodeTaskByNode(changeover.ID, nodeA)
	if err != nil {
		t.Fatalf("get task A: %v", err)
	}
	taskB, err := edge.DB.GetChangeoverNodeTaskByNode(changeover.ID, nodeB)
	if err != nil {
		t.Fatalf("get task B: %v", err)
	}
	if taskA.OldMaterialReleaseOrderID == nil || taskA.NextMaterialOrderID == nil {
		t.Fatal("task A: expected paired evac+supply")
	}
	if taskB.OldMaterialReleaseOrderID == nil || taskB.NextMaterialOrderID == nil {
		t.Fatal("task B: expected paired evac+supply")
	}
	evacA, _ := edge.DB.GetOrder(*taskA.OldMaterialReleaseOrderID)
	supplyA, _ := edge.DB.GetOrder(*taskA.NextMaterialOrderID)
	evacB, _ := edge.DB.GetOrder(*taskB.OldMaterialReleaseOrderID)
	supplyB, _ := edge.DB.GetOrder(*taskB.NextMaterialOrderID)

	for _, o := range []int64{evacA.ID, supplyA.ID, evacB.ID, supplyB.ID} {
		if err := edge.DB.UpdateOrderStatus(o, string(orders.StatusStaged)); err != nil {
			t.Fatalf("force order %d staged: %v", o, err)
		}
	}

	// Drain the changeover-start envelopes so each step's count is clean.
	drainOutbox(t, edge)

	// ── Operator releases BOTH nodes via the per-node modal. ──
	// In production each click is its own POST to apiReleaseOrder.
	// Engine-level we exercise ReleaseOrderWithLineside directly.
	disp := edgeengine.ReleaseDisposition{CalledBy: "multi-node-test"}
	if err := edge.Engine.ReleaseOrderWithLineside(evacA.ID, disp); err != nil {
		t.Fatalf("release evac A: %v", err)
	}
	if err := edge.Engine.ReleaseOrderWithLineside(evacB.ID, disp); err != nil {
		t.Fatalf("release evac B: %v", err)
	}

	releases := pendingReleases(t, edge)
	if len(releases) != 2 {
		t.Fatalf("after evac releases: got %d envelopes, want 2 (one per evac)", len(releases))
	}
	uuids := map[string]bool{}
	for _, r := range releases {
		uuids[r.OrderUUID] = true
	}
	if !uuids[evacA.UUID] || !uuids[evacB.UUID] {
		t.Errorf("evac releases missing — got %+v, want both %s and %s", uuids, evacA.UUID, evacB.UUID)
	}

	// ── Pickup-confirm fires for evac A FIRST. Only supply A should release. ──
	bpEnv, err := protocol.NewDataEnvelope(
		protocol.SubjectBinPickedUp,
		protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		&protocol.BinPickedUp{
			OrderUUID: evacA.UUID, BinID: 1001, Location: "NODE-A",
			PickedUpAt: time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatalf("build BinPickedUp A envelope: %v", err)
	}
	encoded, _ := bpEnv.Encode()
	edge.Ingestor.HandleRaw(encoded)

	releases = pendingReleases(t, edge)
	if len(releases) != 1 {
		t.Fatalf("after evac A pickup: got %d envelopes, want 1 (supply A only — supply B must NOT cross-fire)", len(releases))
	}
	if releases[0].OrderUUID != supplyA.UUID {
		t.Errorf("evac A pickup fired wrong supply: got %s, want supply A %s", releases[0].OrderUUID, supplyA.UUID)
	}

	// ── Now evac B picks up. Only supply B fires. ──
	bpEnv, err = protocol.NewDataEnvelope(
		protocol.SubjectBinPickedUp,
		protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		&protocol.BinPickedUp{
			OrderUUID: evacB.UUID, BinID: 1002, Location: "NODE-B",
			PickedUpAt: time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatalf("build BinPickedUp B envelope: %v", err)
	}
	encoded, _ = bpEnv.Encode()
	edge.Ingestor.HandleRaw(encoded)

	releases = pendingReleases(t, edge)
	if len(releases) != 1 {
		t.Fatalf("after evac B pickup: got %d envelopes, want 1 (supply B only)", len(releases))
	}
	if releases[0].OrderUUID != supplyB.UUID {
		t.Errorf("evac B pickup fired wrong supply: got %s, want supply B %s", releases[0].OrderUUID, supplyB.UUID)
	}
}
