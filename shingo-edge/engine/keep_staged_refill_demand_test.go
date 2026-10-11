package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
)

// keep_staged_refill_demand_test.go — a keep-staged refill is open demand, not
// an earmark.
//
// The keeper tops a staging spot up with real retrieve orders. A forecaster on
// Core must read those orders as demand already open, not look for them among
// demand records: the keeper writes no demand_origins_open row, holds nothing,
// and its refill carries the live leg's episode or no_demand, nothing else.
// Characterisation: no source change; this pins what the keeper does today.

func countOpenEpisodes(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM demand_origins_open`).Scan(&n), "count open episodes")
	return n
}

func onlyRefill(t *testing.T, db *store.DB, nodeID int64) storeorders.Order {
	t.Helper()
	rows, err := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, err, "rows")
	var refills []storeorders.Order
	for _, o := range rows {
		if o.OrderType == orders.TypeRetrieve && o.DeliveryNode == ksSpot {
			refills = append(refills, o)
		}
	}
	if len(refills) != 1 {
		t.Fatalf("%d refills at the spot, want 1", len(refills))
	}
	return refills[0]
}

func TestKeepStaged_RefillIsAnOrderNotAHold(t *testing.T) {
	t.Parallel()

	t.Run("idle line: no_demand", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID := keeperCell(t)
		before := countOpenEpisodes(t, db)
		eng.sweepCellLevels()

		r := onlyRefill(t, db, nodeID)
		if r.Status != protocol.StatusSubmitted || !r.AutoConfirm || r.SourceNode != ksMarket {
			t.Errorf("refill status=%q autoConfirm=%v source=%q, want a submitted auto-confirm retrieve from %q",
				r.Status, r.AutoConfirm, r.SourceNode, ksMarket)
		}
		if r.OriginClass != protocol.OriginClassNoDemand || r.OriginID != "" {
			t.Errorf("refill origin = %q/%q, want no_demand with no id on an idle line", r.OriginClass, r.OriginID)
		}
		if after := countOpenEpisodes(t, db); after != before {
			t.Errorf("demand_origins_open rows %d -> %d: the keeper wrote a demand record", before, after)
		}
	})

	t.Run("live leg: attached to its episode", func(t *testing.T) {
		t.Parallel()
		eng, db, nodeID := keeperCell(t)
		holdingSwap(t, db, nodeID)
		_, err := db.Exec(`UPDATE orders SET origin_id = 'floor-episode', origin_class = ?
			WHERE process_node_id = ? AND order_type = ?`,
			protocol.OriginClassAttached, nodeID, orders.TypeComplex)
		testutil.MustNoErr(t, err, "the live leg carries its episode")
		before := countOpenEpisodes(t, db)
		eng.sweepCellLevels()

		r := onlyRefill(t, db, nodeID)
		if r.OriginClass != protocol.OriginClassAttached || r.OriginID != "floor-episode" {
			t.Errorf("refill origin = %q/%q, want attached to the live leg's floor-episode", r.OriginClass, r.OriginID)
		}
		if after := countOpenEpisodes(t, db); after != before {
			t.Errorf("demand_origins_open rows %d -> %d: the keeper wrote a demand record", before, after)
		}
	})
}
