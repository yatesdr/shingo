//go:build docker

package dispatch

// Characterization tests (P1-C1) for the two big functions in
// complex_dispatch.go — HandleComplexOrderRequest and DispatchPreparedComplex.
//
// These fill the early-return / fail / queue-reason branches the existing suite
// left uncovered, BEFORE the P1-C2 pure file move. They pin CURRENT behavior so
// that the move (bodies untouched) cannot silently drop or reorder a guard — the
// one failure mode the refactor brief names. They are NOT design intent.
//
// Gaps already covered elsewhere and deliberately not duplicated here:
//   - skip (no_source_bin) → phase0_characterization_test.go
//     (TestPhase0_DispositionTriad_NoSourceBin_Complex)
//   - swap-hold (waiting_for_partner) → swap_press_index_deadlock_test.go /
//     swap_sibling_link_test.go
//   - reserve-holding (waiting_for_material) → complex_reserve_test.go
//   - happy-path claim + dispatch end state → complex_dispatch_path_test.go

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// ── HandleComplexOrderRequest: intake rejects (no order row created) ──────────

// TestChar_HandleComplexOrderRequest_EmptySteps_RejectsAtIntake pins the
// zero-steps guard (complex_dispatch.go:44): an intake with no steps is rejected
// synchronously with invalid_steps and NO order row is created — waiting cannot
// fix an empty request.
func TestChar_HandleComplexOrderRequest_EmptySteps_RejectsAtIntake(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "char-empty-steps",
		PayloadCode: "PART-A",
		Steps:       nil,
	})

	if _, err := db.GetOrderByUUID("char-empty-steps"); err == nil {
		t.Fatal("empty-steps intake created an order row; want none (rejected before CreateOrder)")
	}
	if len(emitter.received) != 0 {
		t.Errorf("received events = %d, want 0 (rejected before order creation)", len(emitter.received))
	}
	if len(emitter.queued) != 0 {
		t.Errorf("queued events = %d, want 0", len(emitter.queued))
	}
}

// TestChar_HandleComplexOrderRequest_UnresolvableNode_RejectsAtIntake pins the
// structural/fatal resolution branch (complex_dispatch.go:95 default case): a
// pickup that names a node Core does not have resolves to ResolutionFatal, which
// terminal-rejects at intake (resolution_failed) with NO order row — unlike a
// capacity-shaped failure, an unknown node is not fixable by waiting and must not
// create a queued row that replays forever.
func TestChar_HandleComplexOrderRequest_UnresolvableNode_RejectsAtIntake(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID:   "char-bad-node",
		PayloadCode: "PART-A",
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: "NODE-DOES-NOT-EXIST"},
			{Action: protocol.ActionDropoff, Node: "ALSO-MISSING"},
		},
	})

	if _, err := db.GetOrderByUUID("char-bad-node"); err == nil {
		t.Fatal("unresolvable-node intake created an order row; want none (structural reject)")
	}
	if len(emitter.received) != 0 {
		t.Errorf("received events = %d, want 0 (structural resolution failure rejects before order creation)", len(emitter.received))
	}
}

// ── DispatchPreparedComplex: guard / fail / queue-reason branches ─────────────

// TestChar_DispatchPreparedComplex_NonAcquiringStatus_NoOp pins the entry guard
// (complex_dispatch.go:424): a non-acquiring order (already dispatched, or a
// parent mid-reshuffle) is a NO-OP that returns nil — never re-dispatched. The
// guard is defense-in-depth for any direct caller past the scanner's own gate.
func TestChar_DispatchPreparedComplex_NonAcquiringStatus_NoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// In-memory order only: the guard returns before touching the DB.
	order := &orders.Order{
		EdgeUUID:  "char-nonacquiring",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusDispatched, // not in the acquiring set {queued, sourcing}
		StepsJSON: `[{"action":"pickup","node":"X"},{"action":"dropoff","node":"Y"}]`,
	}

	if err := d.DispatchPreparedComplex(order); err != nil {
		t.Fatalf("non-acquiring order should be a nil no-op, got err=%v", err)
	}
	if order.Status != StatusDispatched {
		t.Errorf("status = %q, want unchanged %q", order.Status, StatusDispatched)
	}
	if len(emitter.dispatched)+len(emitter.failed)+len(emitter.skipped) != 0 {
		t.Errorf("non-acquiring no-op emitted terminal events: dispatched=%d failed=%d skipped=%d",
			len(emitter.dispatched), len(emitter.failed), len(emitter.skipped))
	}
}

// TestChar_DispatchPreparedComplex_InvalidStoredSteps_Fails pins the stored-steps
// parse guard (complex_dispatch.go:430) — and, through it, failOrderInternal
// (:802): unparseable StepsJSON routes to a terminal fail with invalid_steps and
// an EmitOrderFailed, and the error is returned to the scanner verbatim.
func TestChar_DispatchPreparedComplex_InvalidStoredSteps_Fails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	order := &orders.Order{
		EdgeUUID:  "char-bad-json",
		StationID: "line-1",
		OrderType: OrderTypeComplex,
		Status:    StatusQueued,
		Quantity:  1,
		StepsJSON: `{ this is not valid json`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order with bad steps json")

	err := d.DispatchPreparedComplex(order)
	if err == nil {
		t.Fatal("invalid stored steps must return a non-nil error")
	}
	// CHANGE-DETECTOR: a terminal fail emits EmitOrderFailed TWICE today — once
	// from the lifecycle transition's fireFailed hook (lifecycle.go), once from
	// failOrderInternal's own explicit emit. Both carry the same code. This
	// double-emit is pinned as current behavior, not endorsed; a dedup would be a
	// deliberate change that flips this assertion.
	if len(emitter.failed) != 2 {
		t.Fatalf("failed events = %d, want 2 (fireFailed + failOrderInternal double-emit)", len(emitter.failed))
	}
	for i, f := range emitter.failed {
		if f.errorCode != "invalid_steps" {
			t.Errorf("failed[%d] code = %q, want %q", i, f.errorCode, "invalid_steps")
		}
	}
	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "re-read order")
	if got.Status != StatusFailed {
		t.Errorf("order status = %q, want %q", got.Status, StatusFailed)
	}
}

// TestChar_DispatchPreparedComplex_FleetCreateFailure_Fails pins the fleet-create
// failure branch (complex_dispatch.go:747) — the guardless happy-path tail's one
// failure exit — routing through failOrderInternal with fleet_failed. A fully
// sourceable order (bin present, claim succeeds) that the fleet backend rejects
// terminal-fails rather than silently dropping.
func TestChar_DispatchPreparedComplex_FleetCreateFailure_Fails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	// Failing backend: everything before CreateOrder (reserve/confirm/claim) is
	// DB-only and succeeds; the fleet CreateOrder call is the failure point.
	d, emitter := newTestDispatcher(t, db, testdb.NewFailingBackend())

	testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-FLEET-FAIL")

	order := &orders.Order{
		EdgeUUID:     "char-fleet-fail",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  bp.Code,
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name, // LINE dropoff: not a concrete storage slot, no slot gate
		ProcessNode:  storageNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + storageNode.Name + `"},` +
			`{"action":"dropoff","node":"` + lineNode.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	err := d.DispatchPreparedComplex(order)
	if err == nil {
		t.Fatal("fleet CreateOrder failure must return a non-nil error")
	}
	// CHANGE-DETECTOR: same double-emit as the invalid-steps case — fireFailed via
	// the lifecycle transition plus failOrderInternal's explicit emit. Pinned, not
	// endorsed.
	if len(emitter.failed) != 2 {
		t.Fatalf("failed events = %d, want 2 (fireFailed + failOrderInternal double-emit)", len(emitter.failed))
	}
	for i, f := range emitter.failed {
		if f.errorCode != "fleet_failed" {
			t.Errorf("failed[%d] code = %q, want %q", i, f.errorCode, "fleet_failed")
		}
	}
}

// TestChar_DispatchPreparedComplex_ContendedConcreteSlot_QueuesWaitingForSlot
// pins the slot-reserve-incomplete branch (complex_dispatch.go:640) — the
// reservation-native slot guard (the split-brain fix): when a concrete storage
// dropoff slot is already reserved by another order, this order does NOT
// hard-grab or dispatch; it parks with queue_code=waiting_for_slot (cause
// slot-reserve) and returns an error so the scanner replays it on the next
// slot-vacancy tick.
func TestChar_DispatchPreparedComplex_ContendedConcreteSlot_QueuesWaitingForSlot(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	srcNode, _, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// A concrete storage slot: a direct child of an NGRP (isConcreteStorageDropoff
	// role gate). No bin sits on it and no order is inbound to it, so the capacity
	// gate (614) passes and control reaches the slot RESERVE (637).
	grpType, err := db.GetNodeTypeByCode("NGRP")
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "CHAR-SLOT-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "CHAR-SLOT-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")

	// Another order already holds the slot reservation (soft — the reservation-
	// native stand-in for "it got there first"). Give it a delivery node OTHER than
	// the slot so it never counts as inbound to the slot.
	other := &orders.Order{EdgeUUID: "char-slot-other", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusSourcing, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(other), "create contending order")
	testutil.MustNoErr(t, db.ReserveSlot(slot.ID, other.ID), "other reserves the slot")

	// This order's supply pickup has a real bin (so supply-widening resolves Found
	// and control reaches the slot leg rather than parking on material), and its
	// dropoff is the contended slot.
	testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, "BIN-SLOT-CONTEND")
	order := &orders.Order{
		EdgeUUID:     "char-slot-contend",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		PayloadCode:  bp.Code,
		SourceNode:   srcNode.Name,
		DeliveryNode: slot.Name,
		ProcessNode:  srcNode.Name,
		StepsJSON: `[{"action":"pickup","node":"` + srcNode.Name + `"},` +
			`{"action":"dropoff","node":"` + slot.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	order, _ = db.GetOrder(order.ID)

	if err := d.DispatchPreparedComplex(order); err == nil {
		t.Fatal("order should requeue on the contended slot (non-nil error), not dispatch")
	}

	got, gerr := db.GetOrder(order.ID)
	testutil.MustNoErr(t, gerr, "re-read order")
	if got.QueueCode != string(protocol.QueueWaitingForSlot) {
		t.Errorf("queue_code = %q, want %q (parked on the contended slot)",
			got.QueueCode, string(protocol.QueueWaitingForSlot))
	}
	if got.Status != StatusQueued && got.Status != StatusSourcing {
		t.Errorf("status = %q, want queued/sourcing (requeued to retry)", got.Status)
	}
}
