// F' Phase 3 — two-robot changeover release scenario.
//
// Exercises the deferred-supply chain end-to-end through Edge's wire
// encoding / decoding / dispatch surfaces. The unit tests in shingo-edge
// already cover the engine logic; this scenario adds the layer they
// miss: envelope marshal/unmarshal, ingestor routing, and handler
// dispatch through the EdgeHandler the production binary wires.
//
// Doesn't drive Core. The picked-up signal that production gets from
// Core's RDS poller is synthesized inline as a BinPickedUp envelope and
// fed directly to Edge's ingestor — same path Core's published message
// would take. Core-side coverage of the OrderRelease envelope shape is
// already in partial_release_test.go.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"

	"shingoedge/orders"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"

	edgeengine "shingoedge/engine"
)

// TestScenario_TwoRobotChangeoverRelease_EvacFirstThenSupplyOnPickup is
// the Phase-3 wire-level regression for F' Phase 2's deferred-supply
// chain.
//
//	Step 1: operator clicks Release Wait. Edge enqueues exactly ONE
//	        OrderRelease envelope — for the evac leg, with the auto-
//	        detected disposition (release_partial / release_empty).
//	        Supply leg is deferred.
//
//	Step 2: Core's RDS poller observes the evac robot finish its
//	        pickup block — the robot has the old bin and is now in
//	        transit toward outbound. The evac ORDER is still running
//	        (dropoff blocks remain), but the slot is physically clear.
//	        Core sends a BinPickedUp envelope to Edge. We synthesize
//	        that envelope inline and feed it to Edge's ingestor —
//	        same path as production.
//
//	Step 3: Edge's HandleBinPickedUp fires the deferred-supply auto-
//	        release. Edge enqueues a SECOND OrderRelease envelope for
//	        the supply leg, with NIL disposition and NIL RemainingUOP
//	        (manifest preservation — the bug fingerprint from order
//	        682 / 2026-05-06).
//
// Failure modes this catches that unit tests miss:
//   - JSON round-trip on BinPickedUp (a type-shape change on the
//     envelope would break decoding here even when handler logic is
//     fine).
//   - EdgeHandler dispatch — verifies the SetBinPickedUpHandler wiring
//     matches main.go's wiring; if a refactor renames handlers without
//     updating both, this scenario surfaces it.
//   - End-to-end manifest contract on the supply leg through the wire,
//     not just at the engine boundary.
func TestScenario_TwoRobotChangeoverRelease_EvacFirstThenSupplyOnPickup(t *testing.T) {
	const stationID = "edge.test"
	edge := edgeharness.NewEdge(t, stationID)

	// ── Seed: process + styles + node + claims for a Phase-3 swap ──
	processID, err := edge.DB.CreateProcess("P3-PROC", "phase3 swap test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "P3-NODE",
		Code:         "P3N1",
		Name:         "Phase3 Swap Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	fromStyleID, err := edge.DB.CreateStyle("Style-P3-FROM", "from style", processID)
	if err != nil {
		t.Fatalf("create from style: %v", err)
	}
	toStyleID, err := edge.DB.CreateStyle("Style-P3-TO", "to style", processID)
	if err != nil {
		t.Fatalf("create to style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &fromStyleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	fromClaimID, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             fromStyleID,
		CoreNodeName:        "P3-NODE",
		Role:                "consume",
		SwapMode: "simple",
		PayloadCode:         "PART-OLD",
		UOPCapacity:         100,
		InboundSource:       "SOURCE-OLD",
		OutboundStaging:     "OUT-STAGING",
		OutboundDestination: "DEST-OLD",
	})
	if err != nil {
		t.Fatalf("upsert from claim: %v", err)
	}
	if _, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:        toStyleID,
		CoreNodeName:   "P3-NODE",
		Role:           "consume",
		SwapMode: "simple",
		PayloadCode:    "PART-NEW",
		UOPCapacity:    200,
		InboundSource:  "SOURCE-NEW",
		InboundStaging: "IN-STAGING",
	}); err != nil {
		t.Fatalf("upsert to claim: %v", err)
	}
	if _, err := edge.DB.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	if err := edge.DB.SetProcessNodeRuntime(nodeID, &fromClaimID, 50); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	// ── Start changeover; Phase-3 applier creates evac + supply orders ──
	changeover, err := edge.Engine.StartProcessChangeover(processID, toStyleID, "test", "scenario two-robot release")
	if err != nil {
		t.Fatalf("start changeover: %v", err)
	}
	task, err := edge.DB.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if task.OldMaterialReleaseOrderID == nil || task.NextMaterialOrderID == nil {
		t.Fatal("expected both evac+supply order legs created by Phase-3 swap")
	}
	evacOrder, _ := edge.DB.GetOrder(*task.OldMaterialReleaseOrderID)
	supplyOrder, _ := edge.DB.GetOrder(*task.NextMaterialOrderID)

	// Force both to staged. Production gets here via the fleet tracker
	// observing each robot dwelling at its wait point; the integration
	// scenario has no fleet wiring, so we set it directly. Both must be
	// past pre-dispatch (StatusPending / StatusSubmitted) so the
	// Manager.ReleaseOrder pre-dispatch guard doesn't silently skip.
	if err := edge.DB.UpdateOrderStatus(evacOrder.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force evac staged: %v", err)
	}
	if err := edge.DB.UpdateOrderStatus(supplyOrder.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("force supply staged: %v", err)
	}

	// Drain outbox so we count exactly what Step 1 emits.
	drainOutbox(t, edge)

	// ── Step 1: operator clicks Release Wait. Evac fires; supply deferred. ──
	const partial = 47
	disp := edgeengine.ReleaseDisposition{
		Mode:         edgeengine.DispositionSendPartialBack,
		PartialCount: ptr(partial),
		CalledBy:     "scenario-operator",
	}
	result, err := edge.Engine.ReleaseChangeoverWait(processID, disp)
	if err != nil {
		t.Fatalf("ReleaseChangeoverWait: %v", err)
	}
	if result.Released != 1 {
		t.Errorf("step 1 result.Released = %d, want 1 (evac only)", result.Released)
	}
	if result.Pending != 1 {
		t.Errorf("step 1 result.Pending = %d, want 1 (supply deferred)", result.Pending)
	}

	releases := pendingReleases(t, edge)
	if len(releases) != 1 {
		t.Fatalf("step 1: OrderRelease envelopes = %d, want 1 (evac only)", len(releases))
	}
	evacRel := releases[0]
	if evacRel.OrderUUID != evacOrder.UUID {
		t.Errorf("step 1 OrderRelease UUID = %q, want evac %q", evacRel.OrderUUID, evacOrder.UUID)
	}
	// Evac carries the operator's partial count; this end-to-end through
	// the wire (envelope encode → outbox row → decode) must preserve it.
	if evacRel.Disposition == nil {
		t.Fatal("step 1 evac OrderRelease.Disposition = nil; want release_partial")
	}
	if evacRel.Disposition.Kind != protocol.DispositionReleasePartial {
		t.Errorf("step 1 evac disposition kind = %q, want %q",
			evacRel.Disposition.Kind, protocol.DispositionReleasePartial)
	}
	if evacRel.Disposition.Count != partial {
		t.Errorf("step 1 evac disposition count = %d, want %d",
			evacRel.Disposition.Count, partial)
	}

	// Drain Step-1 envelopes so Step 2's count is unambiguous.
	drainOutbox(t, edge)

	// ── Step 2: evac robot picks up old bin and goes in transit. ──
	// Production: shingo-core/engine/wiring_block_completed.go publishes
	// a BinPickedUp envelope to the Edge station via SendDataToEdge.
	// Scenario: synthesize the envelope inline and feed it directly to
	// Edge's ingestor — same wire path the Kafka subscription would
	// dispatch into.
	bpEnv, err := protocol.NewDataEnvelope(
		protocol.SubjectBinPickedUp,
		protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		&protocol.BinPickedUp{
			OrderUUID:  evacOrder.UUID,
			BinID:      9999,
			Location:   "P3-NODE",
			PickedUpAt: time.Now().UTC(),
		},
	)
	if err != nil {
		t.Fatalf("build BinPickedUp envelope: %v", err)
	}
	encoded, err := bpEnv.Encode()
	if err != nil {
		t.Fatalf("encode BinPickedUp envelope: %v", err)
	}
	edge.Ingestor.HandleRaw(encoded)

	// ── Step 3: HandleBinPickedUp's deferred-supply branch fires. ──
	releases = pendingReleases(t, edge)
	if len(releases) != 1 {
		t.Fatalf("step 3: OrderRelease envelopes after BinPickedUp = %d, want 1 (deferred supply)",
			len(releases))
	}
	supplyRel := releases[0]
	if supplyRel.OrderUUID != supplyOrder.UUID {
		t.Errorf("step 3 OrderRelease UUID = %q, want supply %q",
			supplyRel.OrderUUID, supplyOrder.UUID)
	}
	// Supply leg manifest-preservation contract through the wire. This is
	// the regression lock from order 682 / 2026-05-06: anything other
	// than nil here means we wiped Core's bin manifest by accident.
	if supplyRel.Disposition != nil {
		t.Errorf("step 3 supply OrderRelease.Disposition = %+v, want nil (manifest preservation)",
			supplyRel.Disposition)
	}
	if supplyRel.RemainingUOP != nil {
		t.Errorf("step 3 supply OrderRelease.RemainingUOP = &%d, want nil (manifest preservation)",
			*supplyRel.RemainingUOP)
	}
}

// pendingReleases drains the OrderRelease envelopes off Edge's outbox and
// returns them decoded. ACKs each row so subsequent calls observe only
// new envelopes.
func pendingReleases(t *testing.T, edge *edgeharness.Edge) []protocol.OrderRelease {
	t.Helper()
	msgs, err := edge.DB.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	var out []protocol.OrderRelease
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderRelease {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		var rel protocol.OrderRelease
		if err := env.DecodePayload(&rel); err != nil {
			t.Fatalf("decode OrderRelease: %v", err)
		}
		out = append(out, rel)
		if err := edge.DB.AckOutbox(m.ID); err != nil {
			t.Fatalf("ack outbox: %v", err)
		}
	}
	return out
}

// drainOutbox ACKs every pending row regardless of type so the next
// pendingReleases call observes only post-drain envelopes.
func drainOutbox(t *testing.T, edge *edgeharness.Edge) {
	t.Helper()
	msgs, err := edge.DB.ListPendingOutbox(1000)
	if err != nil {
		t.Fatalf("list outbox for drain: %v", err)
	}
	for _, m := range msgs {
		if err := edge.DB.AckOutbox(m.ID); err != nil {
			t.Fatalf("ack outbox during drain: %v", err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
