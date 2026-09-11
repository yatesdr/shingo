//go:build docker

// Black-box (package orders_test) per the cycle note in orders_test.go.
package orders_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// newLeg builds a compound child: an order whose parent link is what makes it a
// leg. The parent is a real row because parent_order_id is a foreign key.
func newLeg(t *testing.T, db *store.DB, uuid string) *orders.Order {
	t.Helper()
	parent := newPendingOrder(uuid + "-parent")
	parent.Status = protocol.StatusReshuffling
	testutil.MustNoErr(t, orders.Create(db.DB, parent), "create parent")

	leg := newPendingOrder(uuid)
	leg.ParentOrderID = &parent.ID
	testutil.MustNoErr(t, orders.Create(db.DB, leg), "create leg")
	return leg
}

// TestSetQueueDetail_EveryLegWaitLandsInHistory is the leg half of "a wait's
// cause lands on that wait's own row".
//
// A dig leg is the one population that writes a cause and then STAYS: seven sites
// in dispatch/compound.go park it with a reason and deliberately leave the
// status at `pending`, because being unsent is what the re-drive selects. The
// stamp refused every one of them — `pending` is a birth certificate, not a
// wait — so the cause lived only in orders.queue_cause, a live column
// overwritten in place. A leg that parked three times left a record of ONE.
//
// Three causes in sequence, three dated rows. This is not the UPDATE the other
// statuses get: a leg has exactly one `pending` episode for its whole life, so
// updating "the row that opened the current episode" would collapse every wait
// it ever has onto that single row — the same disease at a different address.
func TestSetQueueDetail_EveryLegWaitLandsInHistory(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	leg := newLeg(t, d, "uuid-leg-three-waits")

	// The sequence a real leg walks: the lane is busy, then the read fails, then
	// somebody is digging in front of it. Three different waits, three releasers.
	for _, w := range []struct{ reason, code, cause string }{
		{"Storage is being rearranged", string(protocol.QueueStorageRearranging), "lane-occupied"},
		{"Waiting for a slot", string(protocol.QueueWaitingForSlot), "read-failed"},
		{"Storage is being rearranged", string(protocol.QueueStorageRearranging), "lane-dig-active"},
	} {
		testutil.MustNoErr(t, orders.SetQueueDetail(d.DB, leg.ID, w.reason, w.code, w.cause), "park under "+w.cause)
	}

	got := episodeCodes(t, d.DB, leg.ID)
	// The birth row, then one row per wait.
	want := [][2]string{
		{string(protocol.StatusPending), ""},
		{string(protocol.StatusPending), string(protocol.QueueStorageRearranging)},
		{string(protocol.StatusPending), string(protocol.QueueWaitingForSlot)},
		{string(protocol.StatusPending), string(protocol.QueueStorageRearranging)},
	}
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v — three waits must leave three dated records. "+
			"One row for three waits is the live column's disease copied into the table that "+
			"exists to escape it.", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %v, want %v. Full history: %v", i, got[i], want[i], got)
		}
	}

	// AND THE ROWS ARE DATED, which is the whole point of moving the cause off the
	// live column: queue_cause answers "why now", history answers "why last
	// Tuesday, and for how long".
	rows, err := orders.ListHistory(d.DB, leg.ID)
	testutil.MustNoErr(t, err, "list history")
	for _, r := range rows {
		if r.CreatedAt.IsZero() {
			t.Errorf("history row %q carries no timestamp — an undated wait is not a time series", r.Status)
		}
	}
}

// TestSetQueueDetail_AWedgedLegShowsItsWait is the second half, and the one that
// cost a diagnosis: a leg that parks ONCE and never moves again.
//
// Under the plain guard it left no dated record of any wait at all, so the only
// trace of a leg wedged for an hour was a live column and a debug log that is
// nil unless DebugLog is wired.
func TestSetQueueDetail_AWedgedLegShowsItsWait(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	leg := newLeg(t, d, "uuid-leg-wedged")

	testutil.MustNoErr(t, orders.SetQueueDetail(d.DB, leg.ID,
		"Storage is being rearranged", string(protocol.QueueStorageRearranging), "lane-occupied"), "park")

	got := episodeCodes(t, d.DB, leg.ID)
	want := [][2]string{
		{string(protocol.StatusPending), ""},
		{string(protocol.StatusPending), string(protocol.QueueStorageRearranging)},
	}
	if len(got) != len(want) || got[1] != want[1] {
		t.Errorf("history = %v, want %v — a wedged leg's wait must be findable in history, "+
			"not only in a column that the next park overwrites", got, want)
	}
}

// TestSetQueueDetail_APendingNonLegStillWritesNoRow pins the guard that stays.
//
// The reason `pending` was never stamped is real and is not repealed here: the
// bin-move door and planning's reserve path write a cause on a pending order and
// then immediately QUEUE it, and lifecycle.historyReason carries the code off the
// order struct onto that fresh row. Stamping the birth row too would put one wait
// on two rows and read every intake-side park twice.
//
// The leg exemption is scoped to the population that writes a cause and does NOT
// move. A pending order with no parent is still a door about to walk through.
func TestSetQueueDetail_APendingNonLegStillWritesNoRow(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB

	o := newPendingOrder("uuid-pending-not-a-leg")
	testutil.MustNoErr(t, orders.Create(db, o), "create")

	testutil.MustNoErr(t, orders.SetQueueDetail(db, o.ID,
		"Waiting for a slot", string(protocol.QueueWaitingForSlot), "lane-occupied"), "stamp")

	got := episodeCodes(t, db, o.ID)
	want := [][2]string{{string(protocol.StatusPending), ""}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("history = %v, want %v — this order's door is about to queue it, and the code "+
			"rides that transition onto the new row. A row here would count the wait twice.", got, want)
	}
}
