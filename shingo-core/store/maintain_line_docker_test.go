//go:build docker

package store_test

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// TestCountLevel_TopsAndBottomsIsNoStockOfTheLevel: the keeper's resident and
// coming leave tops & bottoms out (it is the tops & bottoms row's, never the
// stack level's), and a coming cart whose count is unknown is coming — it is
// unverified, not tops & bottoms. A level of 4 with two stacks standing and
// tops & bottoms standing and on its way, and two inbound (one two layers
// high, one unknown): resident 2, coming 2, short 0, so the keeper asks for
// nothing; the inbound rows stay whole, all three, for the card.
func TestCountLevel_TopsAndBottomsIsNoStockOfTheLevel(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	var grpID, btID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO nodes (name, is_synthetic, enabled) VALUES ('CL-GRP', true, true) RETURNING id`).Scan(&grpID), "group")
	testdb.MarkGroup(t, db, grpID)
	slots := make([]int64, 3)
	for i := range slots {
		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO nodes (name, is_synthetic, enabled, parent_id) VALUES ($1, false, true, $2) RETURNING id`,
			fmt.Sprintf("CL-SLOT-%d", i+1), grpID).Scan(&slots[i]), "slot")
	}
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code, totes_per_layer) VALUES ('CL-TOTE', 8) RETURNING id`).Scan(&btID), "type")
	bin := func(label string, node any, totes any) int64 {
		t.Helper()
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
			VALUES ($1,$2,$3,'available',$4) RETURNING id`, btID, label, node, totes).Scan(&id), "bin "+label)
		return id
	}
	coming := func(uuid string, binID int64) {
		t.Helper()
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.DeliveryNode, o.OrderType, o.BinID = uuid, "CL-SLOT-3", "move", &binID
		})
		testdb.ReserveBin(t, db, o.ID, binID)
	}
	bin("CL-STACK-1", slots[0], 16)
	bin("CL-STACK-2", slots[1], 40)
	bin("CL-STANDING-TB", slots[2], 0)
	coming("cl-tb", bin("CL-COMING-TB", nil, 0))
	coming("cl-two-high", bin("CL-COMING-16", nil, 16))
	coming("cl-unknown", bin("CL-COMING-NULL", nil, nil))

	group, err := db.GetNode(grpID)
	testutil.MustNoErr(t, err, "group")
	lv := store.MaintainLevel{GroupNodeID: grpID, BinTypeID: btID, BinTypeCode: "CL-TOTE", Want: 4}
	c, err := db.CountLevel(group, lv, store.DemandOrigin{})
	testutil.MustNoErr(t, err, "count")
	if c.Resident != 2 || c.Coming != 2 || c.Short() != 0 || len(c.Inbound) != 3 {
		t.Errorf("resident %d, coming %d, short %d, inbound rows %d; want 2, 2, 0 and the three rows whole",
			c.Resident, c.Coming, c.Short(), len(c.Inbound))
	}
}
