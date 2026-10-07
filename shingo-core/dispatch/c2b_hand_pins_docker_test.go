//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// c2b_hand_pins_docker_test.go — C2b A.1: a dig leg is born holding nothing.
//
// Under v4 §2.6 (SYNTH Fork 1) a leg is born `pending` holding nothing and takes
// its bin in its own hand, at its handover (store.ClaimCompoundLegBinTx through
// acquireHand). Before C2b, store.CreateCompoundChildren claimed each leg's bin
// and wrote its confirmed ledger row at birth, so a leg waiting its turn behind
// the first held its blocker hard for the whole excavation. The blockers stay the
// dig's because the parent holds the lane: the finders and the bin acquire keep
// everyone but the compound out of a lane a dig holds.
func TestC2bPin_ADigLegIsBornHoldingNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	// Two blockers in front of the target: two unbury legs, so one of them is
	// still waiting its turn when the first is sent.
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-C2BL-BLK1")
	createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-C2BL-BLK2")
	target := createTestBinAtNode(t, db, bp.Code, slots[2].ID, "BIN-C2BL-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkQueuedComplexParent(t, db, "uuid-c2b-leg-born", bp.Code)
	d.handleComplexBuriedOnReplay(demand, &BuriedError{Bin: target, Slot: slots[2], LaneID: lane.ID})

	legs, err := db.ListChildOrders(demand.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(legs) < 2 {
		t.Fatalf("fixture: the dig has %d leg(s), want at least 2 (two blockers) — the pin needs a leg "+
			"that is still waiting its turn", len(legs))
	}
	var held []string
	waiting := 0
	for _, leg := range legs {
		if leg.VendorOrderID != "" {
			continue // sent: its hand has run, and it holds what it carries
		}
		waiting++
		var claimed int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM bins WHERE claimed_by = $1`, leg.ID).Scan(&claimed),
			"count the leg's bin claims")
		var rows int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations
			WHERE order_id = $1 AND state = 'confirmed'`, leg.ID).Scan(&rows), "count the leg's confirmed rows")
		if claimed+rows > 0 {
			held = append(held, fmt.Sprintf("leg %d (%s, not sent) holds %d bin claim(s) and %d confirmed "+
				"reservation(s)", leg.ID, leg.Status, claimed, rows))
		}
	}
	if waiting == 0 {
		t.Fatalf("fixture: every leg was sent at once — the pin needs a leg waiting its turn")
	}
	if len(held) != 0 {
		t.Fatalf("a dig leg waits holding: %v", held)
	}
	// AND THE SENT LEG HOLDS WHAT IT CARRIES: its hand took the bin claim and the
	// confirmed row at its handover.
	for _, leg := range legs {
		if leg.VendorOrderID == "" || leg.BinID == nil {
			continue
		}
		var claimed int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM bins WHERE id = $1 AND claimed_by = $2`,
			*leg.BinID, leg.ID).Scan(&claimed), "read the sent leg's claim")
		if claimed != 1 {
			t.Errorf("sent leg %d does not hold bin %d — its hand should have claimed it at the handover",
				leg.ID, *leg.BinID)
		}
	}
	testdb.AssertNoHoldsAtRest(t, db, "a dig with a leg waiting its turn")
}
