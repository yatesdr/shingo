//go:build docker

package reconciliation_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// TestReleaseOrphanedClaims_ReleasesSlotClaims: a slot still claimed by a
// terminal order is released by the defense-in-depth sweep, mirroring the bin
// behavior. Simulates a slot claim that leaked past the atomic terminal
// transition (e.g. crash mid-transaction).
func TestReleaseOrphanedClaims_ReleasesSlotClaims(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t).DB

	slot := &nodes.Node{Name: "SMN_RECON", Enabled: true}
	if err := nodes.Create(sdb, slot); err != nil {
		t.Fatalf("create node: %v", err)
	}
	order := &orders.Order{EdgeUUID: "recon-terminal", StationID: "edge.1", OrderType: "complex", Status: "queued", Quantity: 1}
	if err := orders.Create(sdb, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := nodes.ClaimSlot(sdb, slot.ID, order.ID); err != nil {
		t.Fatalf("claim slot: %v", err)
	}
	// Negative: while the owning order is still non-terminal (queued), the sweep must NOT
	// touch its claim — only terminal orders' claims are orphaned. A legitimately-queued
	// order that holds a claim mid-dispatch must not lose it. (This pins the leg the retired
	// complex_sourcing_window_test used to cover.)
	if n, err := reconciliation.ReleaseOrphanedClaims(sdb); err != nil {
		t.Fatalf("ReleaseOrphanedClaims (pre-terminal): %v", err)
	} else if n != 0 {
		t.Fatalf("pre-terminal sweep released %d claim(s), want 0 (a queued order is not orphaned)", n)
	}
	// Mark terminal WITHOUT releasing the slot (the leak the sweep heals) — a raw
	// write is the point here: it simulates a terminal status that slipped past
	// the atomic TerminalizeOrder (which orders.UpdateStatus now refuses outright).
	if _, err := sdb.Exec(`UPDATE orders SET status='failed', error_detail='leaked-claim test', updated_at=NOW() WHERE id=$1`, order.ID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	released, err := reconciliation.ReleaseOrphanedClaims(sdb)
	if err != nil {
		t.Fatalf("ReleaseOrphanedClaims: %v", err)
	}
	if released < 1 {
		t.Fatalf("expected >=1 released claim, got %d", released)
	}

	// Slot is claimable again by a live order.
	other := &orders.Order{EdgeUUID: "recon-other", StationID: "edge.1", OrderType: "complex", Status: "queued", Quantity: 1}
	if err := orders.Create(sdb, other); err != nil {
		t.Fatalf("create other order: %v", err)
	}
	if err := nodes.ClaimSlot(sdb, slot.ID, other.ID); err != nil {
		t.Fatalf("claim slot after sweep: %v", err)
	}
}
