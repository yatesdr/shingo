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

	// A wait for a resource has a releaser only when something live will put
	// that resource there (the orc's ruling on the D2 after, §2.3): work moving
	// at the waiter's node is not enough. Line 2 of that run stopped with three
	// orders waiting for an empty in its market, beside orders shuttling partial
	// bins through the same market, and the old rule read them as pending.
	// Converted from "work progressing at its node": RED at 9a34f135 on the
	// first read.
	t.Run("a wait for an empty: only an order bringing an empty releases it", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		simNow := time.Now().UTC().Add(48 * time.Hour)
		waiter := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusQueued })
		_, err := db.Exec(`UPDATE orders SET order_type = 'retrieve_empty', source_node = 'STR-MKT', delivery_node = 'STR-LOADER', payload_code = '' WHERE id = $1`, waiter.ID)
		testutil.MustNoErr(t, err, "a retrieve_empty from the market")
		withCause(t, db, waiter.ID, "finder-no-empty-of-type")
		historyAt(t, db, waiter.ID, protocol.StatusQueued, simNow.Add(-30*time.Minute))

		shuttle := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusInTransit })
		_, err = db.Exec(`UPDATE orders SET source_node = 'STR-MKT', delivery_node = 'STR-PRESS', payload_code = 'STR-PART' WHERE id = $1`, shuttle.ID)
		testutil.MustNoErr(t, err, "a full leaving the market")
		historyAt(t, db, shuttle.ID, protocol.StatusInTransit, simNow.Add(-2*time.Minute))
		returning := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusInTransit })
		_, err = db.Exec(`UPDATE orders SET source_node = 'STR-PRESS', delivery_node = 'STR-MKT', payload_code = 'STR-PART' WHERE id = $1`, returning.ID)
		testutil.MustNoErr(t, err, "a partial coming back to the market")
		historyAt(t, db, returning.ID, protocol.StatusInTransit, simNow.Add(-1*time.Minute))

		r, err := readStranded(db.DB, &simNow)
		testutil.MustNoErr(t, err, "read the run end")
		if len(r.stranded) != 1 || r.noRel != 1 {
			t.Fatalf("stranded %d, with no releaser %d; want the waiter with none: a full leaving and a part "+
				"coming back are work at its market, not an empty", len(r.stranded), r.noRel)
		}
		if !strings.Contains(r.clock, "live sim clock") {
			t.Errorf("clock %q, want the live sim clock", r.clock)
		}

		empty := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusInTransit })
		_, err = db.Exec(`UPDATE orders SET order_type = 'move', source_node = 'STR-DRAIN', delivery_node = 'STR-MKT', payload_code = '' WHERE id = $1`, empty.ID)
		testutil.MustNoErr(t, err, "an empty on its way to the market")
		historyAt(t, db, empty.ID, protocol.StatusInTransit, simNow.Add(-1*time.Minute))
		r, err = readStranded(db.DB, &simNow)
		testutil.MustNoErr(t, err, "read the run end again")
		if len(r.stranded) != 1 || r.noRel != 0 {
			t.Fatalf("stranded %d, with no releaser %d; want the waiter's releaser pending: an empty is on its "+
				"way to its market", len(r.stranded), r.noRel)
		}
	})

	// A swap pair waits as one: the evac leg's wait is its supply leg's empty.
	// Line 2's 552 (an empty from the market) and 553 (the weld's full, waiting
	// on it). RED at 9a34f135: both read as pending beside the market's work.
	t.Run("a swap pair waits for its supply leg's empty", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		simNow := time.Now().UTC().Add(48 * time.Hour)
		supply := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusQueued })
		evac := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusQueued })
		_, err := db.Exec(`UPDATE orders SET order_type = 'complex', source_node = 'STR-MKT', delivery_node = 'STR-WELD', payload_code = 'STR-Y',
			sibling_order_uuid = $2, steps_json = '[{"action":"pickup","node":"STR-MKT","empty":true},{"action":"dropoff","node":"STR-WELD"}]' WHERE id = $1`,
			supply.ID, evac.EdgeUUID)
		testutil.MustNoErr(t, err, "the supply leg")
		_, err = db.Exec(`UPDATE orders SET order_type = 'complex', source_node = 'STR-WELD', delivery_node = 'STR-MKT-9', payload_code = 'STR-Y',
			sibling_order_uuid = $2, steps_json = '[{"action":"wait","node":"STR-WELD","wait_kind":"station"},{"action":"pickup","node":"STR-WELD"},{"action":"dropoff","node":"STR-MKT-9","group":"STR-MKT"}]' WHERE id = $1`,
			evac.ID, supply.EdgeUUID)
		testutil.MustNoErr(t, err, "the evac leg")
		for _, id := range []int64{supply.ID, evac.ID} {
			withCause(t, db, id, "ngrp-resolve")
			historyAt(t, db, id, protocol.StatusQueued, simNow.Add(-30*time.Minute))
		}
		busy := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusInTransit })
		_, err = db.Exec(`UPDATE orders SET source_node = 'STR-MKT', delivery_node = 'STR-PRESS', payload_code = 'STR-W' WHERE id = $1`, busy.ID)
		testutil.MustNoErr(t, err, "work at the market")
		historyAt(t, db, busy.ID, protocol.StatusInTransit, simNow.Add(-1*time.Minute))

		r, err := readStranded(db.DB, &simNow)
		testutil.MustNoErr(t, err, "read the run end")
		if len(r.stranded) != 2 || r.noRel != 2 {
			t.Fatalf("stranded %d, with no releaser %d; want both legs with none: nothing brings the supply leg an empty",
				len(r.stranded), r.noRel)
		}
	})
}
