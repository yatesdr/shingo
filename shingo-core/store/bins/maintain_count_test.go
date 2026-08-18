//go:build docker

package bins_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// THE ONE-SPELLING PROPERTY, asserted rather than trusted.
//
// CountEmptyOfTypeInGroup and FindEmptyOfTypeInGroup interpolate the same
// EmptyOfTypeInGroupWhere, so they agree by construction — but "by construction"
// is a claim about today's code, and the failure it prevents is silent and
// expensive: a keeper that counts six carriers the press finder cannot see
// decides the group is stocked while every pull queues for want of one. So the
// equivalence is checked directly, across the states that actually diverge.
//
// find != nil ⟺ count > 0, over the same fixture, after each mutation.
func TestEmptyOfTypeInGroup_CountAndFindAgree(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, "AGREE-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")
	slot := &nodes.Node{Name: "AGREE-SLOT-1", Enabled: true, ParentID: &grpID}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot")

	var btID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('AGREE-45x58') RETURNING id`).Scan(&btID), "bin type")

	agree := func(t *testing.T, stage string) int {
		t.Helper()
		found, ferr := bins.FindEmptyOfTypeInGroup(sdb, "AGREE-45x58", grpID, 0)
		n, cerr := bins.CountEmptyOfTypeInGroup(sdb, "AGREE-45x58", grpID)
		testutil.MustNoErr(t, cerr, stage+": count")
		hasFound := ferr == nil && found != nil
		if hasFound != (n > 0) {
			t.Fatalf("%s: find says %v but count says %d — the two readers have parted, "+
				"which is the 'keeper counts six, press finds none' failure", stage, hasFound, n)
		}
		return n
	}

	if got := agree(t, "empty group"); got != 0 {
		t.Fatalf("empty group count = %d, want 0", got)
	}

	// A resident empty of the type: both see it.
	var binID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,'AGREE-BIN-1',$2,'available') RETURNING id`,
		btID, slot.ID).Scan(&binID), "insert bin")
	if got := agree(t, "one resident empty"); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}

	// Each of these is a state where a naive count would disagree with the
	// finder, and each is a real thing that happens on a floor.
	for _, tc := range []struct {
		stage string
		sql   string
	}{
		{"claimed", `UPDATE bins SET claimed_by = 12345 WHERE id = $1`},
		{"unclaimed again", `UPDATE bins SET claimed_by = NULL WHERE id = $1`},
		{"locked", `UPDATE bins SET locked = true WHERE id = $1`},
		{"unlocked", `UPDATE bins SET locked = false WHERE id = $1`},
		{"staged", `UPDATE bins SET status = 'staged' WHERE id = $1`},
		{"available again", `UPDATE bins SET status = 'available' WHERE id = $1`},
		{"carries a payload", `UPDATE bins SET payload_code = 'PANEL-A' WHERE id = $1`},
		{"empty again", `UPDATE bins SET payload_code = '' WHERE id = $1`},
	} {
		_, err := sdb.Exec(tc.sql, binID)
		testutil.MustNoErr(t, err, tc.stage)
		agree(t, tc.stage)
	}

	// A pending reservation hides the carrier from the finder; the count must
	// lose it too, or the keeper counts stock that is already spoken for.
	var orderID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		 VALUES ('agree-reserver','line-1','retrieve','pending',1) RETURNING id`).Scan(&orderID),
		"insert reserving order")
	_, err = sdb.Exec(
		`INSERT INTO reservations (order_id, bin_id, resource_kind, state) VALUES ($1, $2, 'bin', 'pending')`,
		orderID, binID)
	testutil.MustNoErr(t, err, "reserve")
	if got := agree(t, "pending reservation"); got != 0 {
		t.Errorf("count with a pending reservation = %d, want 0", got)
	}
	_, err = sdb.Exec(`DELETE FROM reservations WHERE bin_id = $1`, binID)
	testutil.MustNoErr(t, err, "unreserve")

	// A DISABLED node takes the carrier out of both. A carrier parked on a
	// disabled position is not on hand in any sense the keeper can act on.
	_, err = sdb.Exec(`UPDATE nodes SET enabled = false WHERE id = $1`, slot.ID)
	testutil.MustNoErr(t, err, "disable slot")
	if got := agree(t, "disabled node"); got != 0 {
		t.Errorf("count at a disabled node = %d, want 0", got)
	}
	_, err = sdb.Exec(`UPDATE nodes SET enabled = true WHERE id = $1`, slot.ID)
	testutil.MustNoErr(t, err, "re-enable slot")

	// A DIFFERENT type is not this level's business.
	var otherID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('AGREE-OTHER') RETURNING id`).Scan(&otherID), "other type")
	_, err = sdb.Exec(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,'AGREE-BIN-2',$2,'available')`,
		otherID, slot.ID)
	testutil.MustNoErr(t, err, "insert other-type bin")
	if got := agree(t, "a second type present"); got != 1 {
		t.Errorf("count = %d, want 1 — the other type is a different level", got)
	}

	// A blank code counts nothing rather than everything, matching the finder's
	// ErrNoRows rather than matching every carrier in the group.
	n, err := bins.CountEmptyOfTypeInGroup(sdb, "", grpID)
	testutil.MustNoErr(t, err, "count blank code")
	if n != 0 {
		t.Errorf("blank type code count = %d, want 0", n)
	}
}
