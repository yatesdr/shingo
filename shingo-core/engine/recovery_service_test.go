//go:build docker

package engine

import (
	"strings"
	"testing"
	"time"

	"shingocore/fleet/simulator"
	"shingocore/store"
)

// recovery_service_test.go — coverage for recovery_service.go.
//
// Covers newRecoveryService + each of its four methods plus their
// validation guards. The engine-level pass-throughs in recovery.go
// are exercised in the same motions (they delegate 1:1).

// ── newRecoveryService ──────────────────────────────────────────────

func TestNewRecoveryService_WiresEngine(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	svc := newRecoveryService(eng)
	if svc == nil {
		t.Fatal("newRecoveryService returned nil")
	}
	if svc.engine != eng {
		t.Error("engine not stored on recovery service")
	}
}

// ── ReapplyOrderCompletion ──────────────────────────────────────────

// TestReapplyOrderCompletion_Success seeds a confirmed order whose
// completed_at is still NULL (the state recovery is designed for),
// runs the method, and asserts the bin moved, the claim released, and
// the order's completed_at was set. Also verifies a recovery_actions
// audit row landed.
func TestReapplyOrderCompletion_Success(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REAPPLY")

	// Seed an order that reached confirmed status without completed_at.
	order := &store.Order{
		EdgeUUID:     "recovery-reapply-1",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "confirmed",
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		BinID:        &bin.ID,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	if err := eng.Recovery().ReapplyOrderCompletion(order.ID, "op-recovery"); err != nil {
		t.Fatalf("ReapplyOrderCompletion: %v", err)
	}

	// Order completed_at set.
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}

	// Bin moved + claim released.
	gotBin, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("reload bin: %v", err)
	}
	if gotBin.NodeID == nil || *gotBin.NodeID != lineNode.ID {
		t.Errorf("bin node = %v, want %d", gotBin.NodeID, lineNode.ID)
	}
	if gotBin.ClaimedBy != nil {
		t.Errorf("bin still claimed by %d, expected released", *gotBin.ClaimedBy)
	}

	// Recovery action row logged.
	acts, _ := db.ListRecoveryActions(10)
	found := false
	for _, a := range acts {
		if a.Action == "reapply_completion" && a.TargetID == order.ID && a.Actor == "op-recovery" {
			found = true
		}
	}
	if !found {
		t.Errorf("no reapply_completion action in %+v", acts)
	}
}

func TestReapplyOrderCompletion_RejectsAlreadyCompleted(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-REAPPLY-DONE")
	now := time.Now()
	order := &store.Order{
		EdgeUUID:     "recovery-reapply-done",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "confirmed",
		SourceNode:   storageNode.Name,
		DeliveryNode: lineNode.Name,
		BinID:        &bin.ID,
		CompletedAt:  &now,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Force completed_at via direct update (CreateOrder drops it).
	if _, err := db.Exec(`UPDATE orders SET completed_at = NOW() WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("set completed_at: %v", err)
	}

	err := eng.Recovery().ReapplyOrderCompletion(order.ID, "op")
	if err == nil {
		t.Fatal("expected error for already-completed order")
	}
	if !strings.Contains(err.Error(), "not awaiting") {
		t.Errorf("err = %v, want 'not awaiting'", err)
	}
}

func TestReapplyOrderCompletion_RejectsMissingOrder(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	err := eng.Recovery().ReapplyOrderCompletion(9999999, "op")
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
}

func TestReapplyOrderCompletion_RejectsNoBin(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	order := &store.Order{
		EdgeUUID:     "recovery-no-bin",
		StationID:    "line-1",
		OrderType:    "retrieve",
		Status:       "confirmed",
		DeliveryNode: "LINE1-IN",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	err := eng.Recovery().ReapplyOrderCompletion(order.ID, "op")
	if err == nil {
		t.Fatal("expected error for order with no bin")
	}
	if !strings.Contains(err.Error(), "no bin") {
		t.Errorf("err = %v", err)
	}
}

// ── ReleaseTerminalBinClaim ─────────────────────────────────────────

// TestReleaseTerminalBinClaim_Success clears a stale claim when the
// claiming order is cancelled. Engine pass-through (Engine.ReleaseTerminalBinClaim)
// tests the same path — we call it directly to also exercise recovery.go.
func TestReleaseTerminalBinClaim_Success(t *testing.T) {
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-RELEASE-TERM")
	order := &store.Order{
		EdgeUUID:  "recovery-release-1",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "cancelled",
		BinID:     &bin.ID,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(order.ID, "cancelled", "seed"); err != nil {
		t.Fatalf("cancel order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	if err := eng.ReleaseTerminalBinClaim(bin.ID, "op-term"); err != nil {
		t.Fatalf("ReleaseTerminalBinClaim: %v", err)
	}

	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.ClaimedBy != nil {
		t.Errorf("bin still claimed by %d, expected nil", *gotBin.ClaimedBy)
	}

	acts, _ := db.ListRecoveryActions(10)
	found := false
	for _, a := range acts {
		if a.Action == "release_terminal_claim" && a.TargetID == bin.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("no release_terminal_claim action: %+v", acts)
	}
}

func TestReleaseTerminalBinClaim_RefusesActiveClaim(t *testing.T) {
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-ACTIVE")
	order := &store.Order{
		EdgeUUID:  "active-claim",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "dispatched", // non-terminal
		BinID:     &bin.ID,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := eng.Recovery().ReleaseTerminalBinClaim(bin.ID, "op"); err == nil {
		t.Fatal("expected refusal to release active claim")
	}

	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.ClaimedBy == nil {
		t.Error("claim should still be in place")
	}
}

// ── ReleaseStagedBin ────────────────────────────────────────────────

func TestReleaseStagedBin_Success(t *testing.T) {
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-STAGED")
	// Stage the bin so its status is "staged".
	future := time.Now().Add(1 * time.Hour)
	if err := db.StageBin(bin.ID, &future); err != nil {
		t.Fatalf("stage bin: %v", err)
	}

	if err := eng.Recovery().ReleaseStagedBin(bin.ID, "op-unstage"); err != nil {
		t.Fatalf("ReleaseStagedBin: %v", err)
	}

	gotBin, _ := db.GetBin(bin.ID)
	if gotBin.Status == "staged" {
		t.Errorf("status = %q, want not staged", gotBin.Status)
	}

	acts, _ := db.ListRecoveryActions(10)
	found := false
	for _, a := range acts {
		if a.Action == "release_staged_bin" && a.TargetID == bin.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("no release_staged_bin recovery action: %+v", acts)
	}
}

func TestReleaseStagedBin_RejectsNonStaged(t *testing.T) {
	db := testDB(t)
	storageNode, _, bp := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-NOT-STAGED")
	// bin is "available" out of the gate — not staged.

	err := eng.Recovery().ReleaseStagedBin(bin.ID, "op")
	if err == nil {
		t.Fatal("expected error for non-staged bin")
	}
	if !strings.Contains(err.Error(), "not staged") {
		t.Errorf("err = %v", err)
	}
}

func TestReleaseStagedBin_MissingBin(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	err := eng.Recovery().ReleaseStagedBin(9999999, "op")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

// ── CancelStuckOrder ────────────────────────────────────────────────

func TestCancelStuckOrder_Success(t *testing.T) {
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	// Put an order into dispatched status via CreateDirectOrder, then stall it.
	res, err := eng.CreateDirectOrder(DirectOrderRequest{
		FromNodeID: storageNode.ID,
		ToNodeID:   lineNode.ID,
		StationID:  "stuck-test",
		Desc:       "stuck-test-order",
	})
	if err != nil {
		t.Fatalf("seed CreateDirectOrder: %v", err)
	}

	if err := eng.CancelStuckOrder(res.OrderID, "recovery-op"); err != nil {
		t.Fatalf("CancelStuckOrder: %v", err)
	}

	got, err := db.GetOrder(res.OrderID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.Status != "cancelled" {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}

	acts, _ := db.ListRecoveryActions(10)
	found := false
	for _, a := range acts {
		if a.Action == "cancel_stuck_order" && a.TargetID == res.OrderID {
			found = true
		}
	}
	if !found {
		t.Errorf("no cancel_stuck_order recovery action: %+v", acts)
	}
}

func TestCancelStuckOrder_RejectsTerminal(t *testing.T) {
	db := testDB(t)
	setupTestData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	order := &store.Order{
		EdgeUUID:  "stuck-terminal",
		StationID: "line-1",
		OrderType: "retrieve",
		Status:    "confirmed",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}

	err := eng.Recovery().CancelStuckOrder(order.ID, "op")
	if err == nil {
		t.Fatal("expected error for terminal order")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("err = %v", err)
	}
}

func TestCancelStuckOrder_MissingOrder(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	err := eng.CancelStuckOrder(9999999, "op")
	if err == nil {
		t.Fatal("expected not-found error")
	}
}
