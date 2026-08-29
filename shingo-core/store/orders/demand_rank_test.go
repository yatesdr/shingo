//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestDemandRank_PriorityFirstThenAge pins §9's seam: the plant's ranking, in
// the one place it is spelled.
//
// Priority first, then oldest. Both halves matter and for different reasons:
// priority is what a person at a Core door can raise, and age is what makes
// progress a GUARANTEE within a class — orders.created_at is stamped once by the
// INSERT and no writer anywhere restamps it, so an order that keeps losing
// strictly ages toward the front rather than being re-shuffled forever.
func TestDemandRank_PriorityFirstThenAge(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	old := orders.DemandRank{Priority: 0, CreatedAt: base}
	young := orders.DemandRank{Priority: 0, CreatedAt: base.Add(time.Hour)}
	urgent := orders.DemandRank{Priority: 5, CreatedAt: base.Add(24 * time.Hour)}

	if !old.Outranks(young) {
		t.Error("within one priority class the older order must win — that is what makes a repeated " +
			"loser age toward the front instead of losing forever")
	}
	if young.Outranks(old) {
		t.Error("the younger order won its own class")
	}
	if !urgent.Outranks(old) {
		t.Error("priority comes first: a day-younger order at priority 5 must beat a priority-0 one")
	}
	if old.Outranks(urgent) {
		t.Error("age beat priority — the classes are strict, and the instrument is meant to SHOW " +
			"starvation rather than prevent it by softening the order")
	}
	if old.Outranks(old) {
		t.Error("a rank outranks itself; a tie must be a tie, or the steal gate takes on equality")
	}
}

// TestDemandRank_ALegPresentsItsParentsDemand is trap T2, pinned.
//
// A dig leg is an ordinary order row: it inherits its parent's origin but not
// its priority (never set, so 0) and its created_at is NOW — the youngest row in
// the plant. Ranked on its own row a leg loses every contest forever, and the
// only non-zero priorities today belong to the hand-placed class, which would
// hand exactly them a permanent veto over every excavation.
//
// So the rank READ resolves the parent. It is resolved rather than copied onto
// the child because a fact has one writer: priority lives on the parent's row,
// and a copy is a second place for it to be wrong.
func TestDemandRank_ALegPresentsItsParentsDemand(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	parent := newPendingOrder("rank-parent")
	parent.Priority = 5
	testutil.MustNoErr(t, orders.Create(db, parent), "create parent")
	// Backdated so the leg's own created_at cannot be mistaken for the parent's.
	_, err := db.Exec(`UPDATE orders SET created_at = NOW() - INTERVAL '2 hours' WHERE id=$1`, parent.ID)
	testutil.MustNoErr(t, err, "age the parent")

	leg := newPendingOrder("rank-leg")
	leg.ParentOrderID = &parent.ID
	testutil.MustNoErr(t, orders.Create(db, leg), "create leg")

	got, err := orders.LoadDemandRank(db, leg.ID)
	testutil.MustNoErr(t, err, "load the leg's demand rank")
	if got.Priority != 5 {
		t.Errorf("the leg's demand priority = %d, want 5 (its PARENT's).\n"+
			"A leg's own row is priority 0 and the plant's youngest timestamp. Ranked on itself it "+
			"loses every contest forever, and every non-zero priority in the plant today belongs to "+
			"the hand-placed class — which would then hold a permanent veto over every excavation.",
			got.Priority)
	}
	own, err := orders.LoadDemandRank(db, parent.ID)
	testutil.MustNoErr(t, err, "load the parent's own rank")
	if !got.CreatedAt.Equal(own.CreatedAt) {
		t.Errorf("the leg's demand age = %v, want the parent's %v — the child exists only as the "+
			"cost of the parent's demand, so it presents the parent's whole rank, not half of it",
			got.CreatedAt, own.CreatedAt)
	}

	// A plain order is its own demand.
	plain := newPendingOrder("rank-plain")
	plain.Priority = 2
	testutil.MustNoErr(t, orders.Create(db, plain), "create plain")
	plainRank, err := orders.LoadDemandRank(db, plain.ID)
	testutil.MustNoErr(t, err, "load the plain order's rank")
	if plainRank.Priority != 2 {
		t.Errorf("a parentless order's demand priority = %d, want its own 2", plainRank.Priority)
	}
}

// TestDemandRank_TheScanOrderIsTheComparator pins the seam's two callers to each
// other: the SQL the line is served in must be the comparator's own twin.
//
// The record's §9 is one sentence — "the ranking lives in exactly one place;
// nothing else may re-implement it" — because the day it becomes time-to-empty
// it has to change in ONE place. Two callers spelling it separately is how that
// lands in one site and silently not the other.
func TestDemandRank_TheScanOrderIsTheComparator(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	base := time.Now().UTC().Add(-24 * time.Hour)
	mk := func(uuid string, priority int, age time.Duration) *orders.Order {
		o := newPendingOrder(uuid)
		o.Priority = priority
		o.Status = protocol.StatusQueued
		testutil.MustNoErr(t, orders.Create(db, o), "create "+uuid)
		_, err := db.Exec(`UPDATE orders SET created_at=$2 WHERE id=$1`, o.ID, base.Add(age))
		testutil.MustNoErr(t, err, "age "+uuid)
		return o
	}
	// Deliberately created in an order that is NOT the ranked order.
	youngPlain := mk("scan-young-plain", 0, 3*time.Hour)
	urgent := mk("scan-urgent", 5, 4*time.Hour)
	oldPlain := mk("scan-old-plain", 0, 1*time.Hour)

	scanned, err := orders.ListAcquiring(db)
	testutil.MustNoErr(t, err, "ListAcquiring")

	var ranked []int64
	for _, o := range scanned {
		ranked = append(ranked, o.ID)
	}
	want := []int64{urgent.ID, oldPlain.ID, youngPlain.ID}
	if len(ranked) != len(want) {
		t.Fatalf("scan returned %v, want the three seeded orders %v", ranked, want)
	}
	for i := range want {
		if ranked[i] != want[i] {
			t.Fatalf("the line's order = %v, want %v — the scan's ORDER BY and the comparator are "+
				"the same ranking said twice, and they disagree", ranked, want)
		}
	}

	// And the comparator agrees with the order the SQL produced, pairwise. This is
	// the half that catches a change to one and not the other.
	for i := 0; i+1 < len(scanned); i++ {
		a := orders.DemandRank{Priority: scanned[i].Priority, CreatedAt: scanned[i].CreatedAt}
		b := orders.DemandRank{Priority: scanned[i+1].Priority, CreatedAt: scanned[i+1].CreatedAt}
		if !a.Outranks(b) {
			t.Errorf("the scan put order %d ahead of %d, and the comparator disagrees. One ranking, "+
				"two spellings — the day this becomes time-to-empty it changes in one and not the other",
				scanned[i].ID, scanned[i+1].ID)
		}
	}
}
