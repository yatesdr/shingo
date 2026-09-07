//go:build docker

package main

import (
	"fmt"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// ── THE POOL THIS ASSERTS ON IS ITS OWN ─────────────────────────────────────
//
// These tests share a database and run in parallel, and the check is a
// PLANT-WIDE question: "does any carrier type have nothing left to source". A
// fixture built on the DEFAULT type would be answering that question about every
// other test's bins as well, and would pass or fail on what else happened to be
// running. Each case therefore mints its own bin type and asserts only on the
// line naming it — the check groups by type, so the isolation is exact.
func poolFixture(t *testing.T, db *store.DB, label string) (typeCode string, nodeID int64) {
	t.Helper()

	typeCode = fmt.Sprintf("SOAK-%s", label)
	bt := &bins.BinType{Code: typeCode, Description: "carrier-pool soak fixture"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type %s: %v", typeCode, err)
	}

	// An ENABLED, NON-SYNTHETIC node with no style_claims row against it.
	// EmptyCarrierWhere excludes all three, and a carrier standing on a cell's
	// own position is deliberately not in the empty population — so a fixture
	// node that happened to carry a claim would seed carriers this check is
	// right to ignore, and the test would be asserting the wrong zero.
	node := &nodes.Node{Name: fmt.Sprintf("SOAKPOOL-%s", label), Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node for %s: %v", label, err)
	}
	return typeCode, node.ID
}

// seedCarrier puts one carrier of the fixture's type at its node. A payload code
// makes it FULL; empty means an empty carrier.
func seedCarrier(t *testing.T, db *store.DB, typeCode string, nodeID int64, label, payload string) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode(typeCode)
	if err != nil {
		t.Fatalf("read bin type %s: %v", typeCode, err)
	}
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(b); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	if payload != "" {
		if err := db.SetBinManifest(b.ID, `{"items":[]}`, payload, 30); err != nil {
			t.Fatalf("set manifest %s: %v", label, err)
		}
		if err := db.ConfirmBinManifest(b.ID, ""); err != nil {
			t.Fatalf("confirm manifest %s: %v", label, err)
		}
	}
	got, err := db.GetBin(b.ID)
	if err != nil {
		t.Fatalf("get bin %s: %v", label, err)
	}
	return got
}

// TestExhaustedCarrierPool_EveryCarrierFull is the deadlock the check exists for,
// and it is the state demo.yaml's STANDARD-SM pool ended a run in: 13 carriers,
// zero empty, PRESS-2 stopped for want of one it could never be given.
//
// It is worth asserting because NOTHING ELSE IN THE SOAK NOTICES. The rate checks
// pass — the plant is in balance, it just cannot move — and the orders that pile
// up wear causes naming the finder rather than the pool, so the report reads as a
// sourcing fault at a station instead of an exhausted carrier type.
func TestExhaustedCarrierPool_EveryCarrierFull(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	typeCode, nodeID := poolFixture(t, db, "FULL")

	for i := 1; i <= 3; i++ {
		seedCarrier(t, db, typeCode, nodeID, fmt.Sprintf("%s-%d", typeCode, i), "PART-A")
	}

	got := checkExhaustedCarrierPool(db)
	if !hasLine(got, typeCode+" is exhausted") {
		t.Errorf("a carrier type with three carriers and none empty did not trip the check.\n"+
			"This is the state a produce station cannot leave on its own: it needs an EMPTY to "+
			"start the next carrier, and it is holding a full one it cannot put down.\ngot: %v", got)
	}
	if !hasLine(got, "all 3 carriers are FULL") {
		t.Errorf("the violation must say how big the pool is — 3 full of 3 is a seeding decision, "+
			"and the reader's next question is always how many carriers the type has.\ngot: %v", got)
	}
}

// TestExhaustedCarrierPool_QuietWhileAnEmptyRemains keeps the check from firing
// on a working plant. One empty carrier is all a produce station needs to start
// another, so a pool holding one is not deadlocked — it is running low, which is
// the stock floor's question and not this one.
func TestExhaustedCarrierPool_QuietWhileAnEmptyRemains(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	typeCode, nodeID := poolFixture(t, db, "ONELEFT")

	seedCarrier(t, db, typeCode, nodeID, typeCode+"-full1", "PART-A")
	seedCarrier(t, db, typeCode, nodeID, typeCode+"-full2", "PART-A")
	seedCarrier(t, db, typeCode, nodeID, typeCode+"-empty", "")

	if got := checkExhaustedCarrierPool(db); hasLine(got, typeCode) {
		t.Errorf("a pool with one sourceable empty was reported as exhausted. A soak that fails on "+
			"a plant which is merely running low teaches the reader to skip its violations.\n"+
			"got: %v", got)
	}
}

// TestExhaustedCarrierPool_EmptiesThatNobodyCanHave is the second zero, and it
// must not be reported as the first.
//
// The carriers here ARE empty. They are also LOCKED, which means a produce
// station asking for one gets nothing — the pool reads as stocked and sources as
// bare. That is a DIFFERENT fault with a different fix (find out what is holding
// them) than "every carrier is full" (make fewer, or make them slower), and
// collapsing the two sends whoever reads the soak to the wrong place.
//
// Locked rather than claimed because a lock is a plant state on its own: a hard
// claim implies a committed robot, so faking one on a queued order builds a
// half-confirmed park that the shared teardown assertion rightly refuses. The
// branch under test is the same either way — sourceable is zero while the pool
// is not full — and a non-expiring lock hiding an available bin is a shape this
// plant has actually produced.
//
// This is the population question the check inherits from bins.EmptyCarrierWhere,
// whose own comment requires that "any count over the same population must
// agree". A check counting `payload_code IS NULL` would call this pool healthy.
func TestExhaustedCarrierPool_EmptiesThatNobodyCanHave(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	typeCode, nodeID := poolFixture(t, db, "LOCKED")

	for i := 1; i <= 2; i++ {
		b := seedCarrier(t, db, typeCode, nodeID, fmt.Sprintf("%s-%d", typeCode, i), "")
		if _, err := db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, b.ID); err != nil {
			t.Fatalf("lock bin: %v", err)
		}
	}

	got := checkExhaustedCarrierPool(db)
	if !hasLine(got, "NONE sourceable") {
		t.Errorf("two empty-but-claimed carriers did not trip the check. They are invisible to "+
			"sourcing, so the station waiting on one waits forever while the pool looks stocked.\n"+
			"got: %v", got)
	}
	if hasLine(got, "is exhausted") {
		t.Errorf("empties that are merely spoken for were reported as an exhausted pool. The two "+
			"zeros have different causes and different fixes, and the message decides where the "+
			"reader looks first.\ngot: %v", got)
	}
}
