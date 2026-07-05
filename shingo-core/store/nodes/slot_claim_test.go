//go:build docker

package nodes_test

import (
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
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

// (D47) TestClaimSlot_CASSemantics and TestClaimSlot_RefusesOccupiedSlot were
// deleted with nodes.ClaimSlot: the CAS/owner-idempotency/occupied-refusal behavior
// now lives on the seatbelted path and is covered by TestConfirmSlotClaim_* below
// (owner-idempotent heal, refuses-without-reservation, refuses-occupied, one-tx).

// TestUnclaimOrderSlots releases every slot an order holds — the terminal-cleanup
// path (mirrors UnclaimOrderBins). Setup uses the sanctioned testdb.ClaimSlotForTest
// raw claim; the assertion reads claimed_by directly (the deleted ClaimSlot's CAS is
// no longer available to prove re-claimability).
func TestUnclaimOrderSlots(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	s1 := mkSlotNode(t, db.DB, "SMN_10")
	s2 := mkSlotNode(t, db.DB, "SMN_11")
	order := mkSlotOrder(t, db.DB, "order-multi")
	testdb.ClaimSlotForTest(t, db, s1, order)
	testdb.ClaimSlotForTest(t, db, s2, order)

	if err := nodes.UnclaimOrderSlots(db.DB, order); err != nil {
		t.Fatalf("UnclaimOrderSlots: %v", err)
	}
	for _, id := range []int64{s1, s2} {
		n, _ := nodes.Get(db.DB, id)
		if n.ClaimedBy != nil {
			t.Errorf("slot %d claimed_by=%v after UnclaimOrderSlots, want nil", id, *n.ClaimedBy)
		}
	}
}

// TestRace_AcquireSlot_SingleWinner is the Hopkinsville #115/#117 characterization,
// now at the reservation layer (1d): N orders race to RESERVE the SAME slot
// concurrently — exactly one wins via uq_reservations_slot_active, the rest get a
// clean ErrReservationConflict (and re-resolve elsewhere). Run under -race; the slot
// reservation index is the single-winner guarantee (ClaimSlotTx then confirms it).
func TestRace_AcquireSlot_SingleWinner(t *testing.T) {
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
			if err := reservations.AcquireSlot(sdb, orderID, slot, "test"); err == nil {
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

// ── the slot seatbelt (ClaimSlotTx + db.ConfirmSlotClaim) ─────────────────────
// ClaimSlotTx carries EXISTS(pending slot reservation) on the owner-idempotent CAS +
// NOT EXISTS bins; ConfirmSlotClaim = claim+confirm in one tx. This is the sole
// slot-claim path (the un-seatbelted nodes.ClaimSlot was deleted in D47).

// TestConfirmSlotClaim_RefusesWithoutReservation: the demoted-CAS seatbelt refuses
// a slot claim with no pending slot reservation — the slot dual of the bin seatbelt.
func TestConfirmSlotClaim_RefusesWithoutReservation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	slot := mkSlotNode(t, db.DB, "CS-NORESV")
	order := mkSlotOrder(t, db.DB, "cs-noresv-o")

	if err := db.ConfirmSlotClaim(slot, order); err == nil {
		t.Fatal("ConfirmSlotClaim without a pending slot reservation must fail (seatbelt), got nil")
	}
	n, _ := nodes.Get(db.DB, slot)
	if n.ClaimedBy != nil {
		t.Errorf("claimed_by = %v, want nil — no slot claim without a reservation", *n.ClaimedBy)
	}
}

// TestConfirmSlotClaim_OwnerIdempotentHeal is the slot mirror of the D46 bin
// wedge-heal: a slot already claimed_by the order with its reservation still
// PENDING (claim committed, confirm didn't) heals on the next ConfirmSlotClaim —
// owner-idempotent CAS (claimed_by=$order) + the seatbelt satisfied by the pending
// row. Retains owner-idempotency the brief requires.
func TestConfirmSlotClaim_OwnerIdempotentHeal(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	slot := mkSlotNode(t, db.DB, "CS-HEAL")
	order := mkSlotOrder(t, db.DB, "cs-heal-o")

	if err := reservations.AcquireSlot(db.DB, order, slot, "test"); err != nil {
		t.Fatalf("AcquireSlot: %v", err)
	}
	// Seed the wedge: claim the slot (raw fixture claim) but leave the reservation
	// pending — claim committed, confirm never ran.
	testdb.ClaimSlotForTest(t, db, slot, order)
	if err := db.ConfirmSlotClaim(slot, order); err != nil {
		t.Fatalf("ConfirmSlotClaim of a claimed-but-pending slot must heal, got %v", err)
	}
	held, _ := reservations.ListByOrder(db.DB, order)
	if len(held) != 1 || held[0].State != reservations.StateConfirmed {
		t.Fatalf("slot reservation = %+v, want exactly one confirmed after heal", held)
	}
}

// TestConfirmSlotClaim_RefusesOccupiedSlot: occupancy IS read at confirm (the
// NOT EXISTS bins guard stays, D43 6/1) even though it is NOT read at reserve (D29).
// A slot physically holding a bin is refused at confirm, and neither half commits.
func TestConfirmSlotClaim_RefusesOccupiedSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	node := sd.StorageNode
	_ = testdb.CreateBinAtNode(t, db, "PART-A", node.ID, "CS-OCC-BIN") // occupy the node
	order := testdb.CreateOrder(t, db)

	if err := reservations.AcquireSlot(db.DB, order.ID, node.ID, "test"); err != nil {
		t.Fatalf("AcquireSlot (reserve succeeds on an occupied node — D29): %v", err)
	}
	if err := db.ConfirmSlotClaim(node.ID, order.ID); err == nil {
		t.Fatal("ConfirmSlotClaim on an OCCUPIED slot must be refused at confirm (NOT EXISTS bins), got nil")
	}
	n, _ := nodes.Get(db.DB, node.ID)
	if n.ClaimedBy != nil {
		t.Errorf("occupied slot claimed_by = %v, want nil", *n.ClaimedBy)
	}
}

// TestConfirmSlotClaim_OneTx: the hard claim and the reservation pending→confirmed
// flip commit together or not at all (mirrors D46's claimAndConfirm). Both on
// success; neither on the seatbelt-refused failure path.
func TestConfirmSlotClaim_OneTx(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	slot := mkSlotNode(t, db.DB, "CS-ONETX")
	order := mkSlotOrder(t, db.DB, "cs-onetx-o")

	// Failure path (no reservation → claim refused → neither half commits).
	if err := db.ConfirmSlotClaim(slot, order); err == nil {
		t.Fatal("ConfirmSlotClaim without reservation must fail")
	}
	if n, _ := nodes.Get(db.DB, slot); n.ClaimedBy != nil {
		t.Fatalf("failure path left claimed_by = %v, want nil (neither)", *n.ClaimedBy)
	}

	// Success path (both the claim and the confirmed reservation land).
	if err := reservations.AcquireSlot(db.DB, order, slot, "test"); err != nil {
		t.Fatalf("AcquireSlot: %v", err)
	}
	if err := db.ConfirmSlotClaim(slot, order); err != nil {
		t.Fatalf("ConfirmSlotClaim: %v", err)
	}
	n, _ := nodes.Get(db.DB, slot)
	if n.ClaimedBy == nil || *n.ClaimedBy != order {
		t.Fatalf("success path claimed_by = %v, want order %d (both)", n.ClaimedBy, order)
	}
	held, _ := reservations.ListByOrder(db.DB, order)
	if len(held) != 1 || held[0].State != reservations.StateConfirmed {
		t.Fatalf("success path reservation = %+v, want one confirmed (both)", held)
	}
}
