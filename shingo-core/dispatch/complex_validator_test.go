//go:build docker

package dispatch

// Pins the race-aware classifier the extended dispatch validator uses to decide
// whether a plan-vs-authority bin difference is benign (a concurrent claim or a
// same-node double-pick) or a real divergence worth surfacing.

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func TestBinDivergenceKind(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	node := &nodes.Node{Name: "BDK", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	free := testdb.CreateBinAtNode(t, db, "PART-A", node.ID, "BDK-FREE")
	mine := testdb.CreateBinAtNode(t, db, "PART-A", node.ID, "BDK-MINE")
	other := testdb.CreateBinAtNode(t, db, "PART-A", node.ID, "BDK-OTHER")

	myOrder := &orders.Order{EdgeUUID: "bdk-mine", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(myOrder), "create my order")
	otherOrder := &orders.Order{EdgeUUID: "bdk-other", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued, Quantity: 1}
	testutil.MustNoErr(t, db.CreateOrder(otherOrder), "create other order")

	testutil.MustNoErr(t, db.ClaimBin(mine.ID, myOrder.ID), "claim mine")
	testutil.MustNoErr(t, db.ClaimBin(other.ID, otherOrder.ID), "claim other")

	cases := []struct {
		name string
		bin  int64
		want string
	}{
		{"free_bin_is_real_divergence", free.ID, "real"},
		{"my_claim_is_double_pick", mine.ID, "self"},
		{"other_claim_is_race", other.ID, "race"},
		{"zero_bin_is_real", 0, "real"},
		{"missing_bin_is_real", 9_999_999, "real"},
	}
	for _, c := range cases {
		if got := d.binDivergenceKind(c.bin, myOrder.ID); got != c.want {
			t.Errorf("%s: binDivergenceKind(%d) = %q, want %q", c.name, c.bin, got, c.want)
		}
	}
}
