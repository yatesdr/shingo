//go:build docker

package orders_test

import (
	"database/sql"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// The level keeper's two order-side tallies.
//
// Both exist because a keeper that miscounts what it has already asked for, or
// what is already coming, re-asks — and re-asking against a group is the
// Springfield 2026-08-03 shape (241 identical queued retrieve_empty orders in
// three and a half hours, because the only guard was a count that could not see
// the orders it had already created) at a new grain.

// A dig child inherits its parent's origin ON PURPOSE (compound.go:553) — the
// reshuffle is part of the cost of the demand that caused it. That makes the
// plain CountLiveByOrigin wrong for a keeper: one physical ask that trips a
// reshuffle would report as several, and the keeper would stop refilling a group
// that is still short.
func TestCountLiveRootsByOrigin_DigChildDoesNotCount(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB
	const origin = "11111111-2222-3333-4444-555555555555"

	root := newPendingOrder("mnt-root-1")
	root.OriginID = origin
	root.Status = "queued" // queued COUNTS — that is the whole 241-duplicates lesson
	testutil.MustNoErr(t, orders.Create(db, root), "create root")

	child := newPendingOrder("mnt-dig-child-1")
	child.OriginID = origin
	child.ParentOrderID = &root.ID
	testutil.MustNoErr(t, orders.Create(db, child), "create dig child")

	// The plain count sees both — correctly, for its own question.
	all, err := orders.CountLiveByOrigin(db, origin)
	testutil.MustNoErr(t, err, "CountLiveByOrigin")
	if all != 2 {
		t.Fatalf("CountLiveByOrigin = %d, want 2 (the fixture must actually have a child)", all)
	}

	// The roots count sees ONE. This is the assertion the keeper rests on.
	roots, err := orders.CountLiveRootsByOrigin(db, origin)
	testutil.MustNoErr(t, err, "CountLiveRootsByOrigin")
	if roots != 1 {
		t.Errorf("CountLiveRootsByOrigin = %d, want 1 — the legs of one physical ask are not additional demand", roots)
	}

	// A terminal root drops out; the episode has nothing outstanding again.
	// Written with raw SQL rather than through TerminalizeOrder: that lives on
	// *store.DB (this package's tests hold a *sql.DB) and the transition itself
	// is not what is under test — the count's NOT IN (terminal) is.
	_, err = db.Exec(`UPDATE orders SET status = $1 WHERE id = $2`,
		string(protocol.StatusConfirmed), root.ID)
	testutil.MustNoErr(t, err, "complete root")
	roots, err = orders.CountLiveRootsByOrigin(db, origin)
	testutil.MustNoErr(t, err, "CountLiveRootsByOrigin after completion")
	if roots != 0 {
		t.Errorf("CountLiveRootsByOrigin after the root completed = %d, want 0", roots)
	}

	// Blank origin never reaches Postgres: origin_id is a UUID column and ""
	// is a cast error, not an empty result.
	n, err := orders.CountLiveRootsByOrigin(db, "")
	testutil.MustNoErr(t, err, "CountLiveRootsByOrigin blank")
	if n != 0 {
		t.Errorf("blank origin = %d, want 0", n)
	}
}

// mkGroupWithChild builds a group with one physical child and returns both, plus
// a bin type id. The inbound count is descendant-aware, so the shape matters.
func mkGroupWithChild(t *testing.T, db *sql.DB, groupName, childName, typeCode string) (int64, int64, int64) {
	t.Helper()
	var grpID, childID, btID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO nodes (name, is_synthetic, enabled) VALUES ($1, true, true) RETURNING id`,
		groupName).Scan(&grpID), "create group")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO nodes (name, is_synthetic, enabled, parent_id) VALUES ($1, false, true, $2) RETURNING id`,
		childName, grpID).Scan(&childID), "create child")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, typeCode).Scan(&btID), "create bin type")
	return grpID, childID, btID
}

// mkEmptyBin creates an empty carrier with NO node — it is in transit, which is
// what "inbound" means. The count keys on the ORDER's destination, never on
// where the carrier currently sits.
func mkEmptyBin(t *testing.T, db *sql.DB, btID int64, label string) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,NULL,'available') RETURNING id`,
		btID, label).Scan(&id), "create bin "+label)
	return id
}

// CountTypedInboundToGroup over the four shapes an inbound carrier actually
// arrives in. Each arm exists because one of them would otherwise be missed, and
// a missed inbound makes the keeper over-ask and then overfill.
func TestCountTypedInboundToGroup_FourShapes(t *testing.T) {
	t.Parallel()
	d := testdb.Open(t)
	db := d.DB
	grpID, _, btID := mkGroupWithChild(t, db, "INB-GRP", "INB-SLOT", "INB-45x58")

	count := func(t *testing.T, stage string) int {
		t.Helper()
		n, err := orders.CountTypedInboundToGroup(db, grpID, "INB-GRP", "INB-45x58")
		testutil.MustNoErr(t, err, stage)
		return n
	}
	if got := count(t, "nothing inbound"); got != 0 {
		t.Fatalf("empty = %d, want 0", got)
	}

	// SHAPE 1 — a plain order whose own bin_id and delivery_node say it all.
	// This is the unloader pushing a drained empty back.
	binA := mkEmptyBin(t, db, btID, "INB-BIN-A")
	plain := newPendingOrder("inb-plain")
	plain.DeliveryNode = "INB-SLOT"
	plain.BinID = &binA
	testutil.MustNoErr(t, orders.Create(db, plain), "create plain")
	if got := count(t, "plain"); got != 1 {
		t.Fatalf("plain inbound = %d, want 1", got)
	}

	// SHAPE 2 — an order still naming the GROUP, not a child. A retrieve_empty
	// admitted while the group was momentarily full keeps the group name and is
	// never re-resolved, so matching only descendants would miss it entirely.
	binB := mkEmptyBin(t, db, btID, "INB-BIN-B")
	grpNamed := newPendingOrder("inb-groupnamed")
	grpNamed.DeliveryNode = "INB-GRP"
	grpNamed.BinID = &binB
	testutil.MustNoErr(t, orders.Create(db, grpNamed), "create group-named")
	if got := count(t, "group-named"); got != 2 {
		t.Fatalf("with a group-named order = %d, want 2", got)
	}

	// SHAPE 3 — a multi-pickup complex order, which records its carriers in
	// order_bins with a per-carrier dest_node. The order's OWN delivery_node is
	// its LAST step and may be somewhere else entirely.
	binC := mkEmptyBin(t, db, btID, "INB-BIN-C")
	cplx := newPendingOrder("inb-complex")
	cplx.DeliveryNode = "SOMEWHERE-ELSE"
	testutil.MustNoErr(t, orders.Create(db, cplx), "create complex")
	testutil.MustNoErr(t, orders.InsertOrderBin(db, cplx.ID, binC, 0, "store", "PICKUP", "INB-SLOT"),
		"junction row")
	if got := count(t, "complex junction"); got != 3 {
		t.Fatalf("with a junction-recorded carrier = %d, want 3", got)
	}

	// The same carrier named by BOTH arms counts ONCE — COUNT(DISTINCT bin_id).
	testutil.MustNoErr(t, orders.InsertOrderBin(db, plain.ID, binA, 0, "store", "PICKUP", "INB-SLOT"),
		"junction row for the plain order too")
	if got := count(t, "both arms name one carrier"); got != 3 {
		t.Errorf("double-named carrier = %d, want 3 (DISTINCT)", got)
	}

	// SHAPE 4 — the keeper's OWN ask is excluded. It is already counted as
	// "asked" by CountLiveRootsByOrigin, and counting it here would
	// double-subtract.
	const mntOrigin = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	_, err := db.Exec(`INSERT INTO demand_origins (origin_id, revision, episode_key, kind, opened_at)
		VALUES ($1, 1, $2, $3, NOW())`,
		mntOrigin, protocol.MaintainEpisodeKey("INB-GRP", "INB-45x58"), protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "mint maintain episode")

	binD := mkEmptyBin(t, db, btID, "INB-BIN-D")
	own := newPendingOrder("inb-own")
	own.DeliveryNode = "INB-SLOT"
	own.BinID = &binD
	own.OriginID = mntOrigin
	testutil.MustNoErr(t, orders.Create(db, own), "create own-origin order")
	if got := count(t, "own maintain origin"); got != 3 {
		t.Errorf("with the keeper's own ask inbound = %d, want 3 — it is already counted as asked", got)
	}

	// A carrier that GAINED A PAYLOAD is not joining an empty level.
	_, err = db.Exec(`UPDATE bins SET payload_code = 'PANEL-A' WHERE id = $1`, binA)
	testutil.MustNoErr(t, err, "load bin A")
	if got := count(t, "loaded carrier"); got != 2 {
		t.Errorf("with a loaded carrier inbound = %d, want 2", got)
	}

	// A TERMINAL order stops being inbound.
	_, err = db.Exec(`UPDATE orders SET status = $1 WHERE id = $2`,
		string(protocol.StatusConfirmed), grpNamed.ID)
	testutil.MustNoErr(t, err, "confirm the group-named order")
	if got := count(t, "terminal order"); got != 1 {
		t.Errorf("after one inbound went terminal = %d, want 1", got)
	}

	// A DIFFERENT type is a different level.
	n, err := orders.CountTypedInboundToGroup(db, grpID, "INB-GRP", "SOME-OTHER-TYPE")
	testutil.MustNoErr(t, err, "other type")
	if n != 0 {
		t.Errorf("other type inbound = %d, want 0", n)
	}
}
