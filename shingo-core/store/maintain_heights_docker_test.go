//go:build docker

package store_test

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// maintain_heights_docker_test.go — what an unloader's group line reads for a
// wanted height is the keeper's two populations: the residents by totes add up
// (with the carts that have no number) to the keeper's resident count, and the
// coming rows, each cart's number carried, are the keeper's coming counted.

func TestCountsByTotes_AgreeWithTheKeepersCounts(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		testutil.MustNoErr(t, err, q)
	}
	var grpID, slotID, btID, otherID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO nodes (name, is_synthetic, enabled) VALUES ('HGT-GRP', true, true) RETURNING id`).Scan(&grpID), "group")
	testdb.MarkGroup(t, db, grpID)
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO nodes (name, is_synthetic, enabled, parent_id) VALUES ('HGT-SLOT', false, true, $1) RETURNING id`,
		grpID).Scan(&slotID), "slot")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code, totes_per_layer) VALUES ('HGT-TOTE', 8) RETURNING id`).Scan(&btID), "type")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('HGT-OTHER') RETURNING id`).Scan(&otherID), "other type")
	bin := func(bt int64, label string, node any, totes any) int64 {
		t.Helper()
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
			VALUES ($1,$2,$3,'available',$4) RETURNING id`, bt, label, node, totes).Scan(&id), "bin "+label)
		return id
	}

	// Residents: 24, 24, 26, unknown; a loaded 24 and another type's 24 are
	// not in the level.
	bin(btID, "HGT-R1", slotID, 24)
	bin(btID, "HGT-R2", slotID, 24)
	bin(btID, "HGT-R3", slotID, 26)
	bin(btID, "HGT-R4", slotID, nil)
	loaded := bin(btID, "HGT-R5", slotID, 24)
	exec(`UPDATE bins SET payload_code = 'PART-A' WHERE id = $1`, loaded)
	bin(otherID, "HGT-R6", slotID, 24)

	resident, err := db.CountEmptyBinsOfTypeInGroup("HGT-TOTE", grpID)
	testutil.MustNoErr(t, err, "keeper resident")
	byTotes, err := db.CountEmptyBinsOfTypeInGroupByTotes("HGT-TOTE", grpID)
	testutil.MustNoErr(t, err, "resident by totes")
	if want := map[int]int{24: 2, 26: 1}; !reflect.DeepEqual(byTotes, want) {
		t.Errorf("residents by totes = %v, want %v", byTotes, want)
	}
	if sum := byTotes[24] + byTotes[26] + 1; sum != resident {
		t.Errorf("residents by totes + the unknown = %d, keeper's resident = %d", sum, resident)
	}

	// Comings, one per shape the keeper counts: a plain order's own carrier,
	// an order still naming the group, a junction row's carrier, and an empty-
	// out's named carrier before it is sourced. The keeper's own ask and a
	// loaded carrier are not coming.
	order := func(uuid, delivery string, setup func(o *orders.Order)) *orders.Order {
		t.Helper()
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.DeliveryNode, o.OrderType = uuid, delivery, "move"
			if setup != nil {
				setup(o)
			}
		})
		if o.BinID != nil {
			// A live order pointing at a bin holds its reservation, or the
			// teardown reads a pointer wedge.
			testdb.ReserveBin(t, db, o.ID, *o.BinID)
		}
		return o
	}
	a := bin(btID, "HGT-C1", nil, 40)
	order("hgt-plain", "HGT-SLOT", func(o *orders.Order) { o.BinID = &a })
	b := bin(btID, "HGT-C2", nil, 40)
	order("hgt-group", "HGT-GRP", func(o *orders.Order) { o.BinID = &b })
	c := bin(btID, "HGT-C3", nil, 16)
	cplx := order("hgt-complex", "ELSEWHERE", nil)
	testutil.MustNoErr(t, orders.InsertOrderBin(db.DB, cplx.ID, c, 0, "store", "PICKUP", "HGT-SLOT"), "junction")
	d := bin(btID, "HGT-C4", nil, nil)
	order("hgt-named", "HGT-GRP", func(o *orders.Order) { o.NamedBinID = &d })
	const mnt = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeee01"
	exec(`INSERT INTO demand_origins (origin_id, revision, episode_key, kind, opened_at) VALUES ($1, 1, $2, $3, NOW())`,
		mnt, protocol.MaintainEpisodeKey("HGT-GRP", "HGT-TOTE"), protocol.EpisodeKindMaintain)
	e := bin(btID, "HGT-C5", nil, 40)
	order("hgt-own", "HGT-SLOT", func(o *orders.Order) { o.BinID = &e; o.OriginID = mnt })
	f := bin(btID, "HGT-C6", nil, 40)
	exec(`UPDATE bins SET payload_code = 'PART-A' WHERE id = $1`, f)
	order("hgt-loaded", "HGT-SLOT", func(o *orders.Order) { o.BinID = &f })

	coming, err := db.CountTypedInboundToGroup(grpID, "HGT-GRP", "HGT-TOTE")
	testutil.MustNoErr(t, err, "keeper coming")
	rows, err := db.ListTypedInboundToGroup(grpID, "HGT-GRP", "HGT-TOTE")
	testutil.MustNoErr(t, err, "inbound rows")
	got := map[int]int{}
	unknown := 0
	for _, r := range rows {
		if r.TotesOnCart == nil {
			unknown++
			continue
		}
		got[*r.TotesOnCart]++
	}
	if want := map[int]int{40: 2, 16: 1}; !reflect.DeepEqual(got, want) || unknown != 1 {
		t.Errorf("inbound rows by totes = %v and %d unknown, want %v and 1", got, unknown, want)
	}
	if len(rows) != coming {
		t.Errorf("inbound rows = %d, keeper's coming = %d: the count is the rows, counted", len(rows), coming)
	}
}
