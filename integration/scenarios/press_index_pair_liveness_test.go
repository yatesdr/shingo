// Round-4 press-index scenarios: pair liveness, and the release path.
//
// The unit tests in shingo-edge already cover each mechanism in isolation.
// What they cannot cover is the layer this file is for: the envelope encode,
// the outbox row, the decode, and the fact that a pair created by one code
// path is still a pair by the time Core would read it. Three of round 4's
// units meet on that surface —
//
//	unit 3  both legs mint their uuids before either create, so NEITHER goes
//	        out with an empty sibling pointer
//	unit 2  the RELEASE refuses to send the placing leg while its sibling has
//	        not cleared the press
//	unit 5  the full disposition goes to the leg that takes the bin OFF, which
//	        for press-index is leg A, not the positionally-labelled one
//
// — and each of them is only true end to end if the wire agrees.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"testing"

	"shingo/protocol"

	edgeengine "shingoedge/engine"
	"shingoedge/orders"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// pressIndexCell is one press-index node with its claim, wired for a swap.
type pressIndexCell struct {
	processID int64
	nodeID    int64
	styleID   int64
	claimID   int64
}

// seedPressIndexCell builds a press-index cell. flipped sets IndexRobotSupplies
// — round 4 unit 1's choreography flip — so the same scenario can run both
// shapes; role decides whether the press is filling bins or emptying them,
// which is what makes the release disposition observable or not.
func seedPressIndexCell(t *testing.T, edge *edgeharness.Edge, prefix string, role protocol.ClaimRole, flipped bool) pressIndexCell {
	t.Helper()
	processID, err := edge.DB.CreateProcess(prefix+"-PROC", "press index scenario", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: prefix + "-PRESS", Code: prefix[:3],
		Name: prefix + " Press", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	styleID, err := edge.DB.CreateStyle(prefix+"-STYLE", "style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	claimID, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        prefix + "-PRESS",
		Role:                role,
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         "WIDGET",
		UOPCapacity:         100,
		InboundSource:       prefix + "-MARKET-EMPTIES",
		OutboundDestination: prefix + "-MARKET",
		PairedCoreNode:      prefix + "-INDEX-B",
		IndexRobotSupplies:  &flipped,
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := edge.DB.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	if err := edge.DB.SetProcessNodeRuntime(nodeID, &claimID, 100); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	return pressIndexCell{processID: processID, nodeID: nodeID, styleID: styleID, claimID: claimID}
}

// pendingComplexRequests decodes and ACKs every pending complex-order envelope.
func pendingComplexRequests(t *testing.T, edge *edgeharness.Edge) []protocol.ComplexOrderRequest {
	t.Helper()
	msgs, err := edge.DB.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	var out []protocol.ComplexOrderRequest
	for _, m := range msgs {
		if m.MsgType != protocol.TypeComplexOrderRequest {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		var req protocol.ComplexOrderRequest
		if err := env.DecodePayload(&req); err != nil {
			t.Fatalf("decode ComplexOrderRequest: %v", err)
		}
		out = append(out, req)
		if err := edge.DB.AckOutbox(m.ID); err != nil {
			t.Fatalf("ack outbox: %v", err)
		}
	}
	return out
}

// PAIR LIVENESS, ON THE WIRE. A swap is only a pair at Core if both envelopes
// name each other: Core's swap_hold checks sib.SiblingOrderUUID == order.EdgeUUID
// and fails OPEN when it does not match, which switches off both the starvation
// hold and the peer-death handler without saying so.
//
// Before round 4 the first-created leg went out with the field empty, because a
// leg could not name a sibling that did not exist yet. Both shapes of the flip
// are run because the flip changes which robot fetches the replacement, and a
// pairing that depended on leg order would show the difference here.
func TestScenario_PressIndexSwap_BothLegsGoOutMutuallyPaired(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prefix  string
		flipped bool
	}{
		{name: "unflipped", prefix: "PLU", flipped: false},
		{name: "flipped", prefix: "PLF", flipped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edge := edgeharness.NewEdge(t, "edge.test")
			cell := seedPressIndexCell(t, edge, tc.prefix, protocol.ClaimRoleProduce, tc.flipped)
			drainOutbox(t, edge)

			if _, err := edge.Engine.RequestProduceSwap(cell.nodeID); err != nil {
				t.Fatalf("request produce swap: %v", err)
			}

			reqs := pendingComplexRequests(t, edge)
			if len(reqs) != 2 {
				t.Fatalf("complex envelopes on the wire = %d, want 2 (both legs)", len(reqs))
			}
			a, b := reqs[0], reqs[1]
			for i, r := range reqs {
				if r.SiblingOrderUUID == "" {
					t.Errorf("leg %d (%s) went out with no sibling — Core reads a one-way link as no "+
						"link, so the starvation hold and the peer-death handler both switch "+
						"themselves off silently", i, r.OrderUUID)
				}
			}
			if a.SiblingOrderUUID != b.OrderUUID || b.SiblingOrderUUID != a.OrderUUID {
				t.Errorf("the legs are not mutually paired on the wire: A(uuid=%s sib=%s) B(uuid=%s sib=%s)",
					a.OrderUUID, a.SiblingOrderUUID, b.OrderUUID, b.SiblingOrderUUID)
			}
			if a.OrderUUID == b.OrderUUID {
				t.Errorf("both legs share uuid %s", a.OrderUUID)
			}

			// AND THE PAIR IS DURABLE LOCALLY TOO. The wire pointer is what
			// Core reads; sibling_order_id is what Edge's own release path and
			// supply-bin guard read. A scenario that checked only one of them
			// would pass with the other broken.
			ords, err := edge.DB.ListActiveOrders()
			if err != nil {
				t.Fatalf("list active orders: %v", err)
			}
			linked := 0
			for _, o := range ords {
				if o.SiblingOrderID != nil {
					linked++
				}
			}
			if linked != 2 {
				t.Errorf("locally sibling-linked orders = %d, want 2 — the row-id link is what "+
					"ComputeSwapReady and the supply-bin guard read", linked)
			}
		})
	}
}

// THE RELEASE PATH, both halves of it, through the engine surface the HTTP
// handler calls.
//
// Half one (unit 2): the supply leg is staged and the evac is not. Releasing
// now would drop a bin onto a press the other robot has not cleared, so the
// click is REFUSED — and refused ADVISORILY, because nothing is broken and the
// operator's only correct action is to click again in a minute.
//
// Half two: with both legs staged the click goes through and BOTH legs release
// with NO disposition — the produce contract. A produce press is pushing a full
// bin out; the lineside questions are consume framing and
// ReleaseOrderWithLineside discards remaining_uop for the role. It is asserted
// here because it is the invariant round 4's disposition re-routing must not
// have disturbed: unit 5 changed WHICH leg is handed the operator's answer, and
// on a produce node the answer that reaches the wire is still nothing.
//
// The routing itself is asserted in the consume scenario below, which is the
// only role where it is observable.
func TestScenario_PressIndexRelease_HoldsThenReleasesBothLegs(t *testing.T) {
	edge := edgeharness.NewEdge(t, "edge.test")
	cell := seedPressIndexCell(t, edge, "PRL", protocol.ClaimRoleProduce, false)
	drainOutbox(t, edge)

	res, err := edge.Engine.RequestProduceSwap(cell.nodeID)
	if err != nil {
		t.Fatalf("request produce swap: %v", err)
	}
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("expected a two-legged swap, got A=%v B=%v", res.OrderA, res.OrderB)
	}
	legA, legB := res.OrderA, res.OrderB
	drainOutbox(t, edge)

	// ── Half one: only the PLACING leg is ready. ──
	//
	// Leg B (R2) is the one that puts a bin on the press. Stage it and leave
	// leg A — the evac — queued, which is the state a slow evac robot produces
	// and which ComputeSwapReady still shows a button for (it accepts EITHER
	// staged leg for press-index).
	if err := edge.DB.UpdateOrderStatus(legB.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("stage leg B: %v", err)
	}
	disp := edgeengine.ReleaseDisposition{
		Mode: edgeengine.DispositionCaptureLineside, CalledBy: "scenario-operator",
	}
	err = edge.Engine.ReleaseStagedOrders(cell.nodeID, disp)
	if err == nil {
		t.Fatal("release must be refused: the placing leg would drop onto a press the sibling has not cleared")
	}
	var advisory interface{ Advisory() bool }
	if !asAdvisory(err, &advisory) || !advisory.Advisory() {
		t.Errorf("the hold must report itself ADVISORY (%T) — rendered red it reads as a fault to "+
			"escalate, when the right response is to click again once the other robot arrives", err)
	}
	if got := pendingReleases(t, edge); len(got) != 0 {
		t.Errorf("a refused release emitted %d OrderRelease envelope(s); want none", len(got))
	}

	// ── Half two: the evac stages too, and the click goes through. ──
	if err := edge.DB.UpdateOrderStatus(legA.ID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("stage leg A: %v", err)
	}
	if err := edge.Engine.ReleaseStagedOrders(cell.nodeID, disp); err != nil {
		t.Fatalf("release with both legs staged: %v", err)
	}

	releases := pendingReleases(t, edge)
	if len(releases) != 2 {
		t.Fatalf("OrderRelease envelopes = %d, want 2 (both legs)", len(releases))
	}
	byUUID := map[string]protocol.OrderRelease{}
	for _, r := range releases {
		byUUID[r.OrderUUID] = r
	}
	for name, uuid := range map[string]string{"leg A": legA.UUID, "leg B": legB.UUID} {
		rel, ok := byUUID[uuid]
		if !ok {
			t.Fatalf("no release envelope for %s (%s)", name, uuid)
		}
		if rel.Disposition != nil {
			t.Errorf("%s carried disposition %+v — a produce release must reach Core with none; "+
				"the bin is being pushed OUT full and the lineside questions are consume framing",
				name, rel.Disposition)
		}
		if rel.RemainingUOP != nil {
			t.Errorf("%s carried remaining_uop=%d — a produce release must leave the manifest alone",
				name, *rel.RemainingUOP)
		}
	}
}

// THE ROUTING ITSELF, on the role where it is visible.
//
// A consume press-index node is the shape unit 5's inversion actually bites on:
// the operator's disposition decides remaining_uop, and remaining_uop decides
// which bin's manifest Core clears. Positionally the pair is labelled
// staged→evac / active→supply, which for press-index is backwards — leg A (R1)
// is the leg that lifts the spent bin off. So the operator's answer must ride
// leg A, and leg B, which is putting a fresh carrier ON the press, must carry
// nothing.
//
// Driven through RequestNodeMaterial, the consume request the operator station
// calls, so the pair is built by the real planner rather than by a fixture that
// could only restate what its author believed the shapes were.
func TestScenario_PressIndexConsumeRelease_DispositionRidesTheEvacLeg(t *testing.T) {
	edge := edgeharness.NewEdge(t, "edge.test")
	cell := seedPressIndexCell(t, edge, "PRC", protocol.ClaimRoleConsume, false)
	drainOutbox(t, edge)

	res, err := edge.Engine.RequestNodeMaterial(cell.nodeID, 1)
	if err != nil {
		t.Fatalf("request node material: %v", err)
	}
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("expected a two-legged swap, got A=%v B=%v", res.OrderA, res.OrderB)
	}
	legA, legB := res.OrderA, res.OrderB
	for _, id := range []int64{legA.ID, legB.ID} {
		if err := edge.DB.UpdateOrderStatus(id, string(orders.StatusStaged)); err != nil {
			t.Fatalf("stage leg %d: %v", id, err)
		}
	}
	drainOutbox(t, edge)

	if err := edge.Engine.ReleaseStagedOrders(cell.nodeID, edgeengine.ReleaseDisposition{
		Mode: edgeengine.DispositionCaptureLineside, CalledBy: "scenario-operator",
	}); err != nil {
		t.Fatalf("release staged orders: %v", err)
	}

	byUUID := map[string]protocol.OrderRelease{}
	for _, r := range pendingReleases(t, edge) {
		byUUID[r.OrderUUID] = r
	}
	evac, ok := byUUID[legA.UUID]
	if !ok {
		t.Fatalf("no release envelope for leg A (%s)", legA.UUID)
	}
	supply, ok := byUUID[legB.UUID]
	if !ok {
		t.Fatalf("no release envelope for leg B (%s)", legB.UUID)
	}
	if evac.RemainingUOP == nil || *evac.RemainingUOP != 0 {
		t.Errorf("leg A (the EVAC — it takes the bin off the press) sent remaining_uop=%v, want &0. "+
			"The operator answered about the bin that is LEAVING; if that answer does not ride this "+
			"leg, Core never clears the manifest of the bin going back to the supermarket.",
			evac.RemainingUOP)
	}
	if supply.RemainingUOP != nil {
		t.Errorf("leg B (the SUPPLY — it puts a bin on the press) sent remaining_uop=%d; it must send "+
			"nothing, or Core wipes the manifest of the bin the line is about to need",
			*supply.RemainingUOP)
	}
}

// asAdvisory is errors.As without importing errors into a file that otherwise
// needs none — kept as a named helper so the assertion above reads as the
// question it is asking.
func asAdvisory(err error, target *interface{ Advisory() bool }) bool {
	for err != nil {
		if a, ok := err.(interface{ Advisory() bool }); ok {
			*target = a
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
