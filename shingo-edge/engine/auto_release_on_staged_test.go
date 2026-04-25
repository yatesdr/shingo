package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
)

// TestHandleAutoReleaseOnStaged_PerSiblingStatus is the predicate test for
// the Bug 2 timing-window fix. Walks every sibling status and asserts whether
// the auto-release fires.
//
// The contract: auto-release fires ONLY when the sibling is in a post-release
// status (in_transit OR delivered), proving the operator already triggered
// the consolidated path against it. Anything else — pre-staged (dispatched,
// submitted, sourcing, etc.), staged (both arrived simultaneously, operator
// hasn't clicked yet), or terminal (cycle is over) — must NOT fire.
//
// This is the regression test for the Round 4 review bug: pre-fix the
// predicate was "!staged && !terminal", which admitted pre-staged statuses
// and would auto-release the FIRST robot to arrive at staged with no
// operator consent. Caught by Dev B; this table prevents reintroduction.
func TestHandleAutoReleaseOnStaged_PerSiblingStatus(t *testing.T) {
	cases := []struct {
		name           string
		siblingStatus  string
		wantFire       bool
		wantDispMode   ReleaseDispositionMode // when wantFire=true
		caseDesc       string
	}{
		// --- post-release statuses: SHOULD fire ---
		{
			name:          "sibling_in_transit_fires",
			siblingStatus: orders.StatusInTransit,
			wantFire:      true,
			wantDispMode:  DispositionCaptureLineside, // Order B (evac) leg
			caseDesc:      "operator already released sibling, robot is moving — late arrival auto-fires",
		},
		{
			name:          "sibling_delivered_fires",
			siblingStatus: orders.StatusDelivered,
			wantFire:      true,
			wantDispMode:  DispositionCaptureLineside,
			caseDesc:      "operator already released sibling, robot completed delivery — late arrival auto-fires",
		},

		// --- pre-staged statuses: MUST NOT fire (this is the Round 4 bug) ---
		{
			name:          "sibling_dispatched_does_NOT_fire",
			siblingStatus: "dispatched",
			wantFire:      false,
			caseDesc:      "ROUND 4 REGRESSION: sibling robot hasn't arrived at wait point yet — operator has not clicked, must NOT auto-release",
		},
		{
			name:          "sibling_submitted_does_NOT_fire",
			siblingStatus: orders.StatusSubmitted,
			wantFire:      false,
			caseDesc:      "sibling not yet acknowledged by Core — operator has not clicked",
		},

		// --- both staged: hold for operator click ---
		{
			name:          "sibling_staged_does_NOT_fire",
			siblingStatus: orders.StatusStaged,
			wantFire:      false,
			caseDesc:      "both staged simultaneously — operator may still want to click manually with a specific disposition",
		},

		// --- terminal: cycle is over ---
		{
			name:          "sibling_confirmed_does_NOT_fire",
			siblingStatus: orders.StatusConfirmed,
			wantFire:      false,
			caseDesc:      "sibling cycle complete — terminal sibling means we're done, no auto-release",
		},
		{
			name:          "sibling_failed_does_NOT_fire",
			siblingStatus: "failed",
			wantFire:      false,
			caseDesc:      "sibling failed — cycle is dead, do not propagate",
		},
		{
			name:          "sibling_cancelled_does_NOT_fire",
			siblingStatus: "cancelled",
			wantFire:      false,
			caseDesc:      "sibling cancelled — cycle is dead, do not propagate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, eng, nodeID, orderA, orderB := seedTwoRobotPair(t, "AUTO-REL-"+tc.name)

			// "Arriving" order is Order B (evac slot). Set it to staged in the
			// DB (the event reflects a status that already happened) and set
			// the sibling (Order A, supply slot) to the case's status.
			if err := db.UpdateOrderStatus(orderB, orders.StatusStaged); err != nil {
				t.Fatalf("set arriving order staged: %v", err)
			}
			if err := db.UpdateOrderStatus(orderA, tc.siblingStatus); err != nil {
				t.Fatalf("set sibling status %q: %v", tc.siblingStatus, err)
			}

			drainOutbox(t, db)

			eng.handleAutoReleaseOnStaged(OrderStatusChangedEvent{
				OrderID:       orderB,
				NewStatus:     orders.StatusStaged,
				ProcessNodeID: &nodeID,
			})

			releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
			if !tc.wantFire {
				if len(releases) != 0 {
					rel := decodeOrderRelease(t, releases[0])
					t.Fatalf("auto-release fired (RemainingUOP=%v) but should NOT have — %s",
						rel.RemainingUOP, tc.caseDesc)
				}
				return
			}
			// wantFire=true path
			if len(releases) != 1 {
				t.Fatalf("expected exactly 1 OrderRelease envelope, got %d — %s", len(releases), tc.caseDesc)
			}
			rel := decodeOrderRelease(t, releases[0])
			// Order B (StagedOrderID slot) gets capture_lineside → wire RemainingUOP=&0.
			if tc.wantDispMode == DispositionCaptureLineside {
				if rel.RemainingUOP == nil {
					t.Errorf("RemainingUOP = nil, want &0 (capture_lineside on evac leg) — %s", tc.caseDesc)
				} else if *rel.RemainingUOP != 0 {
					t.Errorf("RemainingUOP = %d, want 0 (capture_lineside on evac leg) — %s", *rel.RemainingUOP, tc.caseDesc)
				}
			}
			if rel.CalledBy != "auto-release-on-staged" {
				t.Errorf("CalledBy = %q, want %q (audit identity) — %s",
					rel.CalledBy, "auto-release-on-staged", tc.caseDesc)
			}
		})
	}
}

// TestHandleAutoReleaseOnStaged_OrderASupplyLegSendsEmptyDisposition asserts
// the disposition split: when the late arrival is Order A (supply slot,
// runtime.ActiveOrderID), the auto-release uses the empty Mode so Core does
// NOT clear the freshly-loaded supply bin's manifest. Mirrors the manual
// ReleaseStagedOrders B-then-A split in operator_stations.go.
func TestHandleAutoReleaseOnStaged_OrderASupplyLegSendsEmptyDisposition(t *testing.T) {
	db, eng, nodeID, orderA, orderB := seedTwoRobotPair(t, "AUTO-REL-A-LEG")

	// Order A (supply) is the LATE arrival to staged. Order B (evac) was
	// released already and is now in_transit (post-release marker).
	if err := db.UpdateOrderStatus(orderA, orders.StatusStaged); err != nil {
		t.Fatalf("set Order A staged: %v", err)
	}
	if err := db.UpdateOrderStatus(orderB, orders.StatusInTransit); err != nil {
		t.Fatalf("set Order B in_transit: %v", err)
	}

	drainOutbox(t, db)

	eng.handleAutoReleaseOnStaged(OrderStatusChangedEvent{
		OrderID:       orderA,
		NewStatus:     orders.StatusStaged,
		ProcessNodeID: &nodeID,
	})

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("expected 1 OrderRelease envelope, got %d", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])

	// Empty Mode → wire RemainingUOP must be nil so Core does not strip
	// the supply bin's manifest. This is the symmetric-to-supply-bin guard
	// from operator_release_uop_test.go applied to the auto-release path.
	if rel.RemainingUOP != nil {
		t.Errorf("Order A auto-release wire RemainingUOP = %d, want nil — supply bin's manifest must not be cleared by auto-release", *rel.RemainingUOP)
	}
	if rel.CalledBy != "auto-release-on-staged" {
		t.Errorf("CalledBy = %q, want %q", rel.CalledBy, "auto-release-on-staged")
	}
}

// TestHandleAutoReleaseOnStaged_NonTwoRobotModeNeverFires asserts the
// defense-in-depth claim-mode check: even when the runtime slots and sibling
// status would normally trigger auto-release, a sequential or single_robot
// claim must not fire (those modes use different release semantics — the
// per-order path handles them, not the consolidated one).
func TestHandleAutoReleaseOnStaged_NonTwoRobotModeNeverFires(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "AUTO-REL-SEQ",
		PayloadCode: "PART-SEQ",
		UOPCapacity: 1200,
		InitialUOP:  800,
	})
	// Claim seeded as SwapMode="simple" — not two_robot. Hook must skip.
	_ = claimID

	orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-seq-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-seq-B")
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
		t.Fatalf("track A+B on runtime: %v", err)
	}
	if err := db.UpdateOrderStatus(orderB, orders.StatusStaged); err != nil {
		t.Fatalf("set B staged: %v", err)
	}
	if err := db.UpdateOrderStatus(orderA, orders.StatusInTransit); err != nil {
		t.Fatalf("set A in_transit: %v", err)
	}

	eng := testEngine(t, db)
	drainOutbox(t, db)

	eng.handleAutoReleaseOnStaged(OrderStatusChangedEvent{
		OrderID:       orderB,
		NewStatus:     orders.StatusStaged,
		ProcessNodeID: &nodeID,
	})

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 0 {
		t.Errorf("auto-release fired on non-two_robot claim (%d envelopes); the swap-mode guard in handleAutoReleaseOnStaged is broken", len(releases))
	}
}

// TestHandleAutoReleaseOnStaged_ArrivingOrderNotInRuntimeSlots is the
// "unrelated order on the same node" case. If the arriving order isn't in
// either runtime slot, the hook must skip — even if the runtime has a stale
// pair populated. Without this, an unrelated retrieve order arriving at
// staged on the same node could trigger a bogus release.
func TestHandleAutoReleaseOnStaged_ArrivingOrderNotInRuntimeSlots(t *testing.T) {
	db, eng, nodeID, orderA, orderB := seedTwoRobotPair(t, "AUTO-REL-UNREL")

	// Set the runtime pair to in-transit so the predicate would otherwise fire.
	if err := db.UpdateOrderStatus(orderA, orders.StatusInTransit); err != nil {
		t.Fatalf("set A in_transit: %v", err)
	}
	if err := db.UpdateOrderStatus(orderB, orders.StatusInTransit); err != nil {
		t.Fatalf("set B in_transit: %v", err)
	}

	// Create a third order on the same node, NOT tracked in the runtime slots.
	unrelatedID := stageOrderForConsumeNode(t, db, nodeID, "uuid-unrelated")
	if err := db.UpdateOrderStatus(unrelatedID, orders.StatusStaged); err != nil {
		t.Fatalf("set unrelated staged: %v", err)
	}

	drainOutbox(t, db)

	eng.handleAutoReleaseOnStaged(OrderStatusChangedEvent{
		OrderID:       unrelatedID,
		NewStatus:     orders.StatusStaged,
		ProcessNodeID: &nodeID,
	})

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 0 {
		t.Errorf("auto-release fired on an unrelated order (%d envelopes); the runtime-slot membership check in handleAutoReleaseOnStaged is broken", len(releases))
	}
}

// TestHandleAutoReleaseOnStaged_NonStagedTransitionIgnored asserts the gate
// at the top of the handler: any transition that isn't to "staged" must be a
// no-op regardless of runtime/claim/sibling state.
func TestHandleAutoReleaseOnStaged_NonStagedTransitionIgnored(t *testing.T) {
	db, eng, nodeID, orderA, orderB := seedTwoRobotPair(t, "AUTO-REL-NONSTAGE")

	// Set up the "would-fire" preconditions.
	if err := db.UpdateOrderStatus(orderA, orders.StatusInTransit); err != nil {
		t.Fatalf("set A in_transit: %v", err)
	}
	if err := db.UpdateOrderStatus(orderB, orders.StatusStaged); err != nil {
		t.Fatalf("set B staged: %v", err)
	}

	drainOutbox(t, db)

	// Fire with NewStatus=in_transit (not staged) — must be ignored.
	eng.handleAutoReleaseOnStaged(OrderStatusChangedEvent{
		OrderID:       orderB,
		NewStatus:     orders.StatusInTransit,
		ProcessNodeID: &nodeID,
	})
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("auto-release fired on NewStatus=in_transit transition; gate at handler entry is broken")
	}

	// Fire with ProcessNodeID=nil — must be ignored.
	eng.handleAutoReleaseOnStaged(OrderStatusChangedEvent{
		OrderID:   orderB,
		NewStatus: orders.StatusStaged,
		// ProcessNodeID intentionally nil
	})
	if releases := findOutboxByType(t, db, protocol.TypeOrderRelease); len(releases) != 0 {
		t.Errorf("auto-release fired with nil ProcessNodeID; gate at handler entry is broken")
	}
}

// ── Helpers ─────────────────────────────────────────────────────────

// seedTwoRobotPair seeds a consume node, promotes its claim to two_robot,
// stages two orders (A=ActiveOrderID supply, B=StagedOrderID evac), and
// returns the engine + node ID + both order IDs ready for the auto-release
// hook to be invoked. Status of both orders defaults to "staged" — callers
// override per-test.
func seedTwoRobotPair(t *testing.T, prefix string) (*store.DB, *Engine, int64, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      prefix,
		PayloadCode: "PART-2R",
		UOPCapacity: 1200,
		InitialUOP:  800,
	})

	// Promote the seeded claim to two_robot so the auto-release hook's
	// claim-mode guard passes.
	styleID := activeStyleForNode(t, db, nodeID)
	claim, _ := db.GetStyleNodeClaimByNode(styleID, prefix+"-NODE")
	if claim == nil {
		t.Fatalf("seedTwoRobotPair(%q): claim lookup returned nil — seed contract changed", prefix)
	}
	if _, err := db.UpsertStyleNodeClaim(store.StyleNodeClaimInput{
		StyleID:        claim.StyleID,
		CoreNodeName:   claim.CoreNodeName,
		Role:           claim.Role,
		SwapMode:       "two_robot",
		PayloadCode:    claim.PayloadCode,
		UOPCapacity:    claim.UOPCapacity,
		InboundSource:  prefix + "-SOURCE",
		InboundStaging: prefix + "-STAGING",
	}); err != nil {
		t.Fatalf("promote claim to two_robot: %v", err)
	}

	orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-"+prefix+"-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-"+prefix+"-B")
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
		t.Fatalf("track A+B on runtime: %v", err)
	}

	eng := testEngine(t, db)
	return db, eng, nodeID, orderA, orderB
}

// drainOutbox acks every pending outbox row so subsequent findOutboxByType
// assertions are exact. Cheap — only used at test boundaries.
func drainOutbox(t *testing.T, db *store.DB) {
	t.Helper()
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}
}
