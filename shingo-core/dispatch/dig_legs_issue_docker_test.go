//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_legs_issue_docker_test.go — a dig's legs are issued holding no bin row:
// each stamps the bin it moves, which its parent's lane holds, and the dig's
// ranked steal and parking pencil are its own acts after the row. A leg that
// declared its bin as a cart to hold would meet both of these at its issue.

// TestDigLegs_OneBinTwoLegs_BothIssued: one plan moves one bin twice (an
// unbury followed by a retrieve of the same bin). The bin index admits one
// active row, so legs holding their bin by a row could not both be issued.
func TestDigLegs_OneBinTwoLegs_BothIssued(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, line, bp := setupTestData(t, db)
	_, _, slots, shuffles, _ := setupNodeGroupWithShuffle(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "DL2-BIN")
	parent := &orders.Order{EdgeUUID: "dl2-dig", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusReshuffling, Priority: 1}
	testutil.MustNoErr(t, db.CreateOrder(parent), "the dig's parent")
	plan := &ReshufflePlan{Steps: []ReshuffleStep{
		{Sequence: 1, StepType: protocol.StepUnbury, BinID: bin.ID, FromNode: slots[0], ToNode: shuffles[0]},
		{Sequence: 2, StepType: protocol.StepRetrieve, BinID: bin.ID, FromNode: shuffles[0], ToNode: line},
	}}

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.writeCompoundChildren(parent, plan, nil), "issue the dig's legs")

	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "the legs")
	if len(legs) != 2 {
		t.Fatalf("%d legs issued, want 2: one bin may be two legs'", len(legs))
	}
	for _, l := range legs {
		if l.BinID == nil || *l.BinID != bin.ID {
			t.Errorf("leg %d (seq %d) stamps bin %v, want %d", l.ID, l.Sequence, l.BinID, bin.ID)
		}
	}
	var rows int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE bin_id = $1`, bin.ID).Scan(&rows), "rows")
	if rows != 0 {
		t.Errorf("the bin carries %d reservation row(s), want none: a leg holds its bin by its parent's lane", rows)
	}
}

// TestDigLegs_OutrankedPendingHoldIsStolenAtIssue: a stranger has promised
// itself a blocker (a pending hold) and the dig outranks it. The dig's ranked
// steal takes the promise as the leg is issued, and the stranger, un-pointed,
// re-finds. A leg acquiring the blocker as Core's errand would be refused by it.
func TestDigLegs_OutrankedPendingHoldIsStolenAtIssue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "DLS-BLOCKER")
	_ = createTestBinAtNode(t, db, bp.Code, slots[1].ID, "DLS-TARGET")
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.BinID, o.PayloadCode = "dls-stranger", &blocker.ID, bp.Code
	})
	testutil.MustNoErr(t, reservations.Acquire(db.DB, stranger.ID, stranger.ID, blocker.ID, "test"), "its promise")
	// The dig outranks the stranger: a higher priority.
	parent := &orders.Order{EdgeUUID: "dls-dig", StationID: "line-1", OrderType: OrderTypeComplex,
		Status: StatusReshuffling, Priority: 5}
	testutil.MustNoErr(t, db.CreateOrder(parent), "the dig's parent")
	plan, err := PlanLaneMouthClear(db, slots[1], lane, grp.ID, reservations.Anyone)
	testutil.MustNoErr(t, err, "plan the unbury")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.writeCompoundChildren(parent, plan, nil), "issue the dig's legs")

	var holders int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE bin_id = $1 AND order_id = $2`,
		blocker.ID, stranger.ID).Scan(&holders), "the stranger's rows")
	if holders != 0 {
		t.Errorf("the stranger still holds the blocker: an outranked promise is stolen at the leg's issue")
	}
	got, err := db.GetOrder(stranger.ID)
	testutil.MustNoErr(t, err, "the stranger")
	if got.BinID != nil {
		t.Errorf("the stranger points at bin %d, want un-pointed to re-find", *got.BinID)
	}
	legs, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "the legs")
	if len(legs) == 0 {
		t.Fatal("no leg issued")
	}
}
