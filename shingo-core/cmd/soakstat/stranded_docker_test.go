//go:build docker

package main

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// historyAt replaces an order's history with one row at the given sim time.
func historyAt(t *testing.T, db *store.DB, id int64, status protocol.Status, at time.Time) {
	t.Helper()
	_, err := db.Exec(`DELETE FROM order_history WHERE order_id = $1`, id)
	testutil.MustNoErr(t, err, "clear history")
	_, err = db.Exec(`INSERT INTO order_history (order_id, status, created_at) VALUES ($1, $2, $3)`, id, string(status), at)
	testutil.MustNoErr(t, err, "seed history")
}

// withCause writes a queue cause on the row (CreateOrder does not carry one).
func withCause(t *testing.T, db *store.DB, id int64, cause string) {
	t.Helper()
	_, err := db.Exec(`UPDATE orders SET queue_cause = $1 WHERE id = $2`, cause, id)
	testutil.MustNoErr(t, err, "write the cause")
}

// TestStranded_AFrozenPlantFails: main's 2026-10-05 jam in small, read from a
// dump (no live clock, so "now" is the newest history row). A dig parent waits
// on its leg, the leg dwells with no slot, and a material wait nobody holds,
// all quiet for 40 sim-minutes, and no work progressed at their nodes in the
// last 10: stranded, with no releaser, so the run fails. The order that wrote
// the newest row is live and not in the table.
//
// Then: a stranded waiter whose chain reaches a live order has a releaser
// pending. A material wait nobody holds has one only while work progresses at
// its own nodes or groups (read on the live sim clock): a shuttle elsewhere is
// not its releaser, as main's market cluster stood a day beside one.
func TestStranded_AFrozenPlantFails(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "seeds history rows at chosen times")

	t.Run("a dig with nowhere to park, and an unarmed wait", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		simNow := time.Now().UTC().Add(48 * time.Hour)
		parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusStaged })
		leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status, o.ParentOrderID = protocol.StatusStaged, &parent.ID
		})
		unarmed := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusQueued })
		withCause(t, db, leg.ID, "no-shuffle-slot")
		withCause(t, db, unarmed.ID, "finder-no-empty-of-type")
		moving := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusInTransit })
		for _, id := range []int64{parent.ID, leg.ID, unarmed.ID} {
			historyAt(t, db, id, protocol.StatusStaged, simNow.Add(-40*time.Minute))
		}
		historyAt(t, db, moving.ID, protocol.StatusInTransit, simNow)

		r, err := readStranded(db.DB, nil)
		testutil.MustNoErr(t, err, "read the run end")
		if len(r.stranded) != 3 || r.noRel != 3 {
			t.Fatalf("stranded %d, with no releaser %d; want 3 and 3 (parent, leg, unarmed)", len(r.stranded), r.noRel)
		}
		if !strings.HasPrefix(r.verdict(), "FAIL") {
			t.Fatalf("verdict %q, want FAIL", r.verdict())
		}
		for _, s := range r.stranded {
			if s.o.id == moving.ID {
				t.Fatal("the moving order is in the stranded table")
			}
			if s.o.id == parent.ID && !strings.Contains(s.waitsOn, "its legs") {
				t.Errorf("the parent waits on %q, want its legs", s.waitsOn)
			}
		}
		if len(r.active) != 4 {
			t.Errorf("Active Orders lists %d, want all 4 non-terminal", len(r.active))
		}
	})

	t.Run("a chain that reaches a live order", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		simNow := time.Now().UTC().Add(48 * time.Hour)
		parent := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusStaged })
		leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status, o.ParentOrderID = protocol.StatusInTransit, &parent.ID
		})
		historyAt(t, db, parent.ID, protocol.StatusStaged, simNow.Add(-20*time.Minute))
		historyAt(t, db, leg.ID, protocol.StatusInTransit, simNow)

		r, err := readStranded(db.DB, nil)
		testutil.MustNoErr(t, err, "read the run end")
		if len(r.stranded) != 1 || r.noRel != 0 {
			t.Fatalf("stranded %d, with no releaser %d; want the parent, its releaser its live leg", len(r.stranded), r.noRel)
		}
		if !strings.HasPrefix(r.verdict(), "PASS") {
			t.Fatalf("verdict %q, want PASS", r.verdict())
		}
	})

	t.Run("a material wait: work progressing at its node, not elsewhere", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		simNow := time.Now().UTC().Add(48 * time.Hour)
		waiter := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status, o.DeliveryNode = protocol.StatusQueued, "STR-MKT"
		})
		withCause(t, db, waiter.ID, "finder-no-empty-of-type")
		shuttle := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status, o.SourceNode, o.DeliveryNode = protocol.StatusInTransit, "STR-H1", "STR-ALN"
		})
		historyAt(t, db, waiter.ID, protocol.StatusQueued, simNow.Add(-30*time.Minute))
		historyAt(t, db, shuttle.ID, protocol.StatusInTransit, simNow.Add(-2*time.Minute))

		r, err := readStranded(db.DB, &simNow)
		testutil.MustNoErr(t, err, "read the run end")
		if len(r.stranded) != 1 || r.noRel != 1 {
			t.Fatalf("stranded %d, with no releaser %d; want the waiter with none: a shuttle elsewhere is "+
				"not its releaser", len(r.stranded), r.noRel)
		}
		if !strings.Contains(r.clock, "live sim clock") {
			t.Errorf("clock %q, want the live sim clock", r.clock)
		}

		supply := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.Status, o.SourceNode = protocol.StatusInTransit, "STR-MKT"
		})
		historyAt(t, db, supply.ID, protocol.StatusDispatched, simNow.Add(-1*time.Minute))
		r, err = readStranded(db.DB, &simNow)
		testutil.MustNoErr(t, err, "read the run end again")
		if len(r.stranded) != 1 || r.noRel != 0 {
			t.Fatalf("stranded %d, with no releaser %d; want the waiter's releaser pending, work progressing "+
				"at its node", len(r.stranded), r.noRel)
		}
	})
}
