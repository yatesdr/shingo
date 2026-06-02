//go:build docker

package nodes_test

import (
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func mkSlotOrder(t *testing.T, sdb *sql.DB, uuid string) int64 {
	t.Helper()
	o := &orders.Order{EdgeUUID: uuid, StationID: "edge.1", OrderType: "complex", Status: "queued", Quantity: 1}
	if err := orders.Create(sdb, o); err != nil {
		t.Fatalf("create order %s: %v", uuid, err)
	}
	return o.ID
}

func mkSlotNode(t *testing.T, sdb *sql.DB, name string) int64 {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	if err := nodes.Create(sdb, n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n.ID
}

// TestClaimSlot_CASSemantics covers the store dual of the bin claim: a slot is
// claimable by exactly one order, re-claims by the owner are idempotent (so a
// requeue/replay doesn't livelock), and a different order is rejected until the
// slot is unclaimed.
func TestClaimSlot_CASSemantics(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t).DB

	slot := mkSlotNode(t, sdb, "SMN_02")
	orderA := mkSlotOrder(t, sdb, "order-a")
	orderB := mkSlotOrder(t, sdb, "order-b")

	if err := nodes.ClaimSlot(sdb, slot, orderA); err != nil {
		t.Fatalf("A claim free slot: %v", err)
	}
	if err := nodes.ClaimSlot(sdb, slot, orderB); err == nil {
		t.Fatal("B claimed a slot already held by A; want error")
	}
	// Owner re-claim is idempotent.
	if err := nodes.ClaimSlot(sdb, slot, orderA); err != nil {
		t.Fatalf("A re-claim own slot (idempotent): %v", err)
	}
	if err := nodes.UnclaimSlot(sdb, slot); err != nil {
		t.Fatalf("unclaim slot: %v", err)
	}
	if err := nodes.ClaimSlot(sdb, slot, orderB); err != nil {
		t.Fatalf("B claim after unclaim: %v", err)
	}
}

// TestClaimSlot_RefusesOccupiedSlot: a slot physically holding a bin cannot be
// claimed (the NOT EXISTS bins guard in the CAS).
func TestClaimSlot_RefusesOccupiedSlot(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t).DB

	slot := mkSlotNode(t, sdb, "SMN_07")
	bt := &bins.BinType{Code: "TOTE", Description: "Tote"}
	if err := bins.CreateType(sdb, bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	b := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-OCC", NodeID: &slot, Status: "available"}
	if err := bins.Create(sdb, b); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := mkSlotOrder(t, sdb, "order-occ")
	if err := nodes.ClaimSlot(sdb, slot, order); err == nil {
		t.Fatal("claimed an occupied slot; want error")
	}
}

// TestClaimSlot_UnclaimOrderSlots releases every slot an order holds — the
// terminal-cleanup path (mirrors UnclaimOrderBins).
func TestClaimSlot_UnclaimOrderSlots(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t).DB

	s1 := mkSlotNode(t, sdb, "SMN_10")
	s2 := mkSlotNode(t, sdb, "SMN_11")
	order := mkSlotOrder(t, sdb, "order-multi")
	if err := nodes.ClaimSlot(sdb, s1, order); err != nil {
		t.Fatalf("claim s1: %v", err)
	}
	if err := nodes.ClaimSlot(sdb, s2, order); err != nil {
		t.Fatalf("claim s2: %v", err)
	}
	nodes.UnclaimOrderSlots(sdb, order)

	other := mkSlotOrder(t, sdb, "order-other")
	if err := nodes.ClaimSlot(sdb, s1, other); err != nil {
		t.Fatalf("claim s1 after UnclaimOrderSlots: %v", err)
	}
	if err := nodes.ClaimSlot(sdb, s2, other); err != nil {
		t.Fatalf("claim s2 after UnclaimOrderSlots: %v", err)
	}
}

// TestRace_ClaimSlot_SingleWinner is the Hopkinsville #115/#117 characterization:
// N orders race to claim the SAME slot concurrently — exactly one wins, the rest
// get a clean error (and re-resolve elsewhere). Run under -race; the DB-level
// CAS is the single-claimant guarantee.
func TestRace_ClaimSlot_SingleWinner(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t).DB

	slot := mkSlotNode(t, sdb, "SMN_CONTENDED")
	const n = 8
	orderIDs := make([]int64, n)
	for i := 0; i < n; i++ {
		orderIDs[i] = mkSlotOrder(t, sdb, fmt.Sprintf("racer-%d", i))
	}

	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(orderID int64) {
			defer wg.Done()
			<-start
			if err := nodes.ClaimSlot(sdb, slot, orderID); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}(orderIDs[i])
	}
	close(start) // release all racers at once
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly 1 winner for contended slot, got %d", wins)
	}
}
