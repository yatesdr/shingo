package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// TestReleaseOrderWithLineside_WireRemainingUOP_PerDisposition is the
// end-to-end contract test for the operator-disposition → wire-envelope flow
// that Bug 1 (kanbans.js disposition fix) and Bug 2 (consolidated release)
// both depend on. It walks the operator's three real choices through
// ReleaseOrderWithLineside and asserts the OrderRelease envelope queued in the
// outbox carries the value that Core's BinManifestService.SyncOrClearForReleased
// then routes on:
//
//   - empty Mode (no disposition / legacy / Order A supply leg) → nil
//     → Core: no-op, bin manifest untouched.
//   - DispositionCaptureLineside ("NOTHING PULLED" / "CONFIRM & RELEASE")  → &0
//     → Core: ClearAndKeepClaim, bin's payload + manifest cleared, UOP=0.
//   - DispositionSendPartialBack ("SEND PARTIAL BACK")                     → &runtime.RemainingUOP
//     → Core: SyncUOPAndKeepClaim, manifest preserved, uop_remaining set to
//       the count Edge tracked at release time.
//
// Combined with TestHandleOrderRelease_RemainingUOPZero/Positive/Nil... on the
// Core side, this proves the full plant-visible chain: operator click →
// disposition → envelope → bin row UOP. If a future refactor silently changes
// the wire shape (e.g. defaulting empty Mode to capture, or dropping the
// runtime UOP threading), one of these subtests fails before it ships.
func TestReleaseOrderWithLineside_WireRemainingUOP_PerDisposition(t *testing.T) {
	const (
		runtimeUOP = 800 // count Edge thinks is left on the bin at release time
		capacity   = 1200
	)

	cases := []struct {
		name     string
		disp     ReleaseDisposition
		wantNil  bool // wire RemainingUOP must be nil
		wantUOP  int  // ignored if wantNil; otherwise *RemainingUOP must equal this
		caseDesc string
	}{
		{
			name:     "empty_mode_sends_nil_no_manifest_action",
			disp:     ReleaseDisposition{Mode: "", CalledBy: "test"},
			wantNil:  true,
			caseDesc: "no changes — empty disposition (legacy, Order A supply, untyped HTTP body) leaves the bin alone at Core",
		},
		{
			name:     "capture_lineside_sends_zero_clears_bin",
			disp:     ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test"},
			wantNil:  false,
			wantUOP:  0,
			caseDesc: "empties — operator confirmed empty / capture_lineside, Core clears bin manifest + UOP",
		},
		{
			name:     "send_partial_back_sends_runtime_uop_syncs_bin",
			disp:     ReleaseDisposition{Mode: DispositionSendPartialBack, CalledBy: "test"},
			wantNil:  false,
			wantUOP:  runtimeUOP,
			caseDesc: "partials — operator sending leftover back to supermarket, Core syncs uop_remaining to runtime count",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)
			_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
				Prefix:      "REL-WIRE-" + tc.name,
				PayloadCode: "PART-WIRE",
				UOPCapacity: capacity,
				InitialUOP:  runtimeUOP,
			})
			// Re-anchor runtime UOP after seeding (seedConsumeNode sets it,
			// but be explicit so a future change to the helper doesn't break
			// the partials assertion).
			if err := db.SetProcessNodeRuntime(nodeID, &claimID, runtimeUOP); err != nil {
				t.Fatalf("seed runtime UOP: %v", err)
			}

			orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-wire-"+tc.name)
			// Track on runtime so ReleaseOrderWithLineside follows the same
			// path it would in production (claim resolution + UOP reset live
			// off the runtime row).
			if err := db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID); err != nil {
				t.Fatalf("track staged order on runtime: %v", err)
			}

			eng := testEngine(t, db)

			// Drain any pre-existing outbox messages (none expected here, but
			// defensive — keeps findOutboxByType assertions exact).
			pending, _ := db.ListPendingOutbox(100)
			for _, m := range pending {
				_ = db.AckOutbox(m.ID)
			}

			if err := eng.ReleaseOrderWithLineside(orderID, tc.disp); err != nil {
				t.Fatalf("ReleaseOrderWithLineside (%s): %v", tc.caseDesc, err)
			}

			releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
			if len(releases) != 1 {
				t.Fatalf("OrderRelease envelopes queued: got %d, want 1 (%s)", len(releases), tc.caseDesc)
			}
			rel := decodeOrderRelease(t, releases[0])

			if tc.wantNil {
				if rel.RemainingUOP != nil {
					t.Errorf("wire RemainingUOP = %d, want nil — %s", *rel.RemainingUOP, tc.caseDesc)
				}
				return
			}
			if rel.RemainingUOP == nil {
				t.Fatalf("wire RemainingUOP = nil, want &%d — %s", tc.wantUOP, tc.caseDesc)
			}
			if *rel.RemainingUOP != tc.wantUOP {
				t.Errorf("wire RemainingUOP = %d, want %d — %s", *rel.RemainingUOP, tc.wantUOP, tc.caseDesc)
			}

			// Order should always advance to in_transit on a successful
			// release call regardless of disposition shape.
			got, err := db.GetOrder(orderID)
			if err != nil {
				t.Fatalf("re-read order: %v", err)
			}
			if got.Status != orders.StatusInTransit {
				t.Errorf("order status = %q, want %q after release", got.Status, orders.StatusInTransit)
			}
		})
	}
}

// TestReleaseOrderWithLineside_TwoRobotSupplyOrderForcesNilWire is the
// regression test for the supply-bin protection in two-robot swaps. Even
// when the operator picks DispositionCaptureLineside (which would normally
// clear the bin at Core), the supply leg (Order A — the new bin coming in
// from the supermarket) must NOT have its manifest cleared — only the
// evacuation leg (Order B) should. ReleaseOrderWithLineside detects the
// supply role via runtime slot membership + claim.SwapMode and overrides
// the disposition's wire value to nil for Order A.
//
// Without this guard a single operator click on the consolidated RELEASE
// button would strip the manifest off the freshly-loaded supply bin before
// it even reached the line — re-introducing the SMN_003 stale-UOP bug class.
func TestReleaseOrderWithLineside_TwoRobotSupplyOrderForcesNilWire(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-WIRE-TR-SUPPLY",
		PayloadCode: "PART-TR",
		UOPCapacity: 1200,
		InitialUOP:  800,
	})

	// Promote the claim to two_robot so the supply-order detection fires.
	claim, _ := db.GetStyleNodeClaimByNode(activeStyleForNode(t, db, nodeID), "REL-WIRE-TR-SUPPLY-NODE")
	if claim == nil {
		t.Fatal("claim lookup returned nil — seed contract changed")
	}
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:        claim.StyleID,
		CoreNodeName:   claim.CoreNodeName,
		Role:           claim.Role,
		SwapMode:       "two_robot",
		PayloadCode:    claim.PayloadCode,
		UOPCapacity:    claim.UOPCapacity,
		InboundSource:  "TR-SOURCE",
		InboundStaging: "TR-STAGING",
	}); err != nil {
		t.Fatalf("promote claim to two_robot: %v", err)
	}

	// Stage Order A (supply) and Order B (evac), track both on runtime.
	orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-supply-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-supply-B")
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
		t.Fatalf("track A+B on runtime: %v", err)
	}

	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	eng := testEngine(t, db)

	// Operator picks capture_lineside — for Order A this MUST be overridden
	// to nil so Core leaves the supply bin alone.
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "test-operator"}
	if err := eng.ReleaseOrderWithLineside(orderA, disp); err != nil {
		t.Fatalf("release Order A: %v", err)
	}

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes queued: got %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])
	if rel.OrderUUID != "uuid-tr-supply-A" {
		t.Fatalf("released wrong order: got UUID %q, want uuid-tr-supply-A", rel.OrderUUID)
	}
	if rel.RemainingUOP != nil {
		t.Errorf("supply-order wire RemainingUOP = %d, want nil — capture_lineside must be suppressed for Order A so Core does not strip the supply bin's manifest", *rel.RemainingUOP)
	}
	// CalledBy should still be threaded through for the audit row even when
	// the manifest sync is suppressed.
	if rel.CalledBy != "test-operator" {
		t.Errorf("CalledBy = %q, want %q (audit identity must survive the supply-order suppression)", rel.CalledBy, "test-operator")
	}
}

// activeStyleForNode returns the active style ID for a node by walking
// process → ActiveStyleID. Internal scaffolding for the supply-order test.
func activeStyleForNode(t *testing.T, db *store.DB, nodeID int64) int64 {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get process node %d: %v", nodeID, err)
	}
	process, err := db.GetProcess(node.ProcessID)
	if err != nil {
		t.Fatalf("get process %d: %v", node.ProcessID, err)
	}
	if process.ActiveStyleID == nil {
		t.Fatalf("process %d has no ActiveStyleID — seed contract changed", node.ProcessID)
	}
	return *process.ActiveStyleID
}
