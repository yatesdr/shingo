//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_hand_placed_docker_test.go — what happens to a person's order when a dig
// takes the bin they named.
//
// This is the path where the dig WON the blocker — since §7 that is a rank
// contest rather than a property of digs, and the fixture below says so. What
// was wrong was everything after that. The steal cleared the
// holder's bin_id, which for an ordinary demand is a recalculation and for a
// HAND-PLACED one is Core quietly re-aiming somebody's instruction at whatever
// bin happens to be standing at that node next. And because the bin-move door
// stamped no source intent, the re-aimed order did not even get that far: the
// scanner's move exemption could not see it and killed it as a construction
// bug, with a sentence about a payload code the door is not supposed to set.
//
// So it ends here instead, out loud, under a code of its own.

// TestDigTakesAHandPlacedBin_FailsLoudlyWithItsOwnCode is the disposition.
//
// MUTATION (verified): drop the failDisplacedByHand call. The bin move sits
// queued, pointing at a bin the dig owns and holding no reservation on it —
// invisible, permanent, and exactly the shape contract-v2 clause (iii) detects.
func TestDigTakesAHandPlacedBin_FailsLoudlyWithItsOwnCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-HAND-BLOCKER")
	_ = createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-HAND-TARGET")

	// A person at a Core door: a bin move naming THIS bin, holding it softly,
	// stamped the way the door stamps it.
	byHand := &orders.Order{
		EdgeUUID: "uuid-hand-placed-move", StationID: "line-1",
		OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1,
		SourceNode:   slots[0].Name,
		BinID:        &blocker.ID,
		OriginClass:  protocol.OriginClassNoDemand,
		SourceIntent: SourceIntentForType(OrderTypeMove),
	}
	testutil.MustNoErr(t, db.CreateOrder(byHand), "create hand-placed move")
	testutil.MustNoErr(t, reservations.Acquire(db.DB, byHand.ID, byHand.ID, blocker.ID, "test"), "reserve blocker")

	// THE DIG OUTRANKS, a stated precondition since §7 rather than a property of
	// digs. Without it the hand-placed move — seeded first, so older at equal
	// priority — keeps its bin and this test never reaches the disposition it is
	// about. What happens to a hand-placed order whose bin a dig CAN take is the
	// question here; whether it can take it is the ranked take's
	// (ranked_take_docker_test.go).
	parent := &orders.Order{
		EdgeUUID: "uuid-hand-placed-dig", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusReshuffling,
		Priority: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create dig parent")

	plan, err := PlanLaneMouthClear(db, slots[1], lane, grp.ID, reservations.Anyone)
	testutil.MustNoErr(t, err, "plan unbury")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.writeCompoundChildren(parent, plan), "writeCompoundChildren")

	after, err := db.GetOrder(byHand.ID)
	testutil.MustNoErr(t, err, "reload hand-placed order")

	if !protocol.IsTerminal(after.Status) {
		t.Fatalf("hand-placed order is %q, want a terminal — it points at a bin the dig now owns "+
			"and holds no reservation on it, and nothing in the tree re-acquires. Left live it is "+
			"queued forever with claim-failed and no releaser", after.Status)
	}
	if strings.Contains(strings.ToLower(after.ErrorDetail), "payload_code") {
		t.Errorf("failed with %q — that is the structural arm, which is a sentence about a "+
			"construction bug. A move door does not set a payload code and is not supposed to",
			after.ErrorDetail)
	}
	if !strings.Contains(after.ErrorDetail, "dig") {
		t.Errorf("error detail = %q; it must say a dig took the bin, and where it went — the "+
			"person who asked for this move is the reader", after.ErrorDetail)
	}

	code := terminalCodeFor(t, db, byHand.ID)
	if code != string(protocol.TermBinDugAway) {
		t.Errorf("terminal code = %q, want %q — not claim_failed (nothing raced) and not "+
			"structural (nothing is malformed)", code, protocol.TermBinDugAway)
	}
}

// terminalCodeFor reads the code off the order's terminal history row.
func terminalCodeFor(t *testing.T, db interface {
	ListOrderHistory(int64) ([]*orders.History, error)
}, orderID int64) string {
	t.Helper()
	rows, err := db.ListOrderHistory(orderID)
	testutil.MustNoErr(t, err, "read order history")
	for i := len(rows) - 1; i >= 0; i-- {
		if protocol.IsTerminal(rows[i].Status) {
			return rows[i].Code
		}
	}
	t.Fatalf("order %d has no terminal history row", orderID)
	return ""
}
