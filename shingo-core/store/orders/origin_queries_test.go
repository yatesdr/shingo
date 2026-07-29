//go:build docker

package orders_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// origin_queries_test.go — the one store query the origin-indexed forensics
// surface needed (5.12).

func seedOriginRow(t *testing.T, db *store.DB, originID string) {
	t.Helper()
	testutil.MustNoErr(t, db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID: originID,
		Revision: 1,
		// The episode key is derived from originID: demand_origins carries
		// idx_demand_origins_open_key, a unique index enforcing ONE OPEN EPISODE
		// PER KEY, so two open fixtures sharing a key is not a test setup detail —
		// it is a state the system refuses to hold.
		EpisodeKey:  "cell|devplant.line1|3|PANEL-" + originID[len(originID)-12:] + "|supply",
		Kind:        "cell",
		Direction:   "supply",
		StationID:   "devplant.line1",
		ProcessID:   3,
		PayloadCode: "PANEL-A",
		OpenedAt:    time.Now().UTC().Add(-time.Hour),
	}), "seed episode")
}

func seedOrderWithOrigin(t *testing.T, db *store.DB, uuid, originID string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID:    uuid,
		StationID:   "devplant.line1",
		OrderType:   "move",
		Status:      protocol.StatusPending,
		Quantity:    1,
		PayloadCode: "PANEL-A",
		OriginID:    originID,
		OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order "+uuid)
	return o
}

// setCreatedAt rewrites created_at directly. orders.Create stamps every row in
// a test from the same clock reading, so the ordering this query promises has to
// be set up explicitly rather than hoped for.
func setCreatedAt(t *testing.T, db *store.DB, id int64, at time.Time) {
	t.Helper()
	_, err := db.Exec(`UPDATE orders SET created_at = $1 WHERE id = $2`, at, id)
	testutil.MustNoErr(t, err, "set created_at")
}

// TestListByOrigin_ReadsForwardFromTheDemand pins the ascending order.
//
// Every other order listing in this package is newest-first, which is right for
// "what is happening now". This one answers "what did this demand cause" — a
// story with a beginning. Newest-first would put the FIRST order, the one whose
// lateness explains every order after it, at the bottom of the page.
func TestListByOrigin_ReadsForwardFromTheDemand(t *testing.T) {
	db := testdb.Open(t)
	const originID = "1a1a1a1a-0000-4000-8000-000000000001"
	seedOriginRow(t, db, originID)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	third := seedOrderWithOrigin(t, db, "fwd-3", originID)
	first := seedOrderWithOrigin(t, db, "fwd-1", originID)
	second := seedOrderWithOrigin(t, db, "fwd-2", originID)

	setCreatedAt(t, db, first.ID, base)
	setCreatedAt(t, db, second.ID, base.Add(2*time.Minute))
	setCreatedAt(t, db, third.ID, base.Add(9*time.Minute))

	got, truncated, err := orders.ListByOrigin(db.DB, originID, 100)
	testutil.MustNoErr(t, err, "list by origin")
	if truncated {
		t.Error("three rows under a cap of 100 must not report truncation")
	}
	if len(got) != 3 {
		t.Fatalf("want 3 orders, got %d", len(got))
	}
	wantOrder := []int64{first.ID, second.ID, third.ID}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Fatalf("position %d: got order %d, want %d — the list is not reading forward "+
				"from the demand", i, got[i].ID, want)
		}
	}
}

// TestListByOrigin_TiesBreakOnID characterises the case a bare ORDER BY
// created_at leaves undefined.
//
// created_at is written from clock.Now() rather than the DDL default (see
// orders.Create), so under the simulator's fast-forward clock several orders
// genuinely share a timestamp — and a burst minted together is exactly the stack
// someone reads this page for. Without the id tiebreak their order is whatever
// the plan returns, which SQL does not constrain at all.
//
// THIS IS A CHARACTERIZATION TEST, NOT A GUARD, AND THE DISTINCTION IS RECORDED
// BECAUSE VERIFY-RED FOUND IT. Deleting `, id ASC` from the query leaves this
// test GREEN on Postgres 16. Two attempts to force a divergence failed and the
// second one explains the first: HOT updates do not write new index entries, so
// rewriting the tuples in reverse id order moves them in the HEAP but leaves
// idx_orders_origin_id pointing at line pointers in insertion order, and the
// index scan hands the sort an already-ascending input that its presorted path
// returns unchanged. Postgres agrees with the tiebreak by accident here.
//
// The tiebreak stays: an ordering that holds only because of the plan the
// planner happens to pick is not an ordering, and a forensic page that reorders
// itself between two reads of the same episode is worse than one that is wrong
// in a fixed way. But nothing below proves it, and a test believed to be a guard
// when it is a description is how a rule quietly stops being enforced.
func TestListByOrigin_TiesBreakOnID(t *testing.T) {
	db := testdb.Open(t)
	const originID = "1a1a1a1a-0000-4000-8000-000000000002"
	seedOriginRow(t, db, originID)

	same := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Millisecond)
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, seedOrderWithOrigin(t, db, fmt.Sprintf("tie-%d", i), originID).ID)
	}
	// Stamped in DESCENDING id order so the heap order is the reverse of the id
	// order — the closest this substrate gets to an adversarial physical layout.
	// It is not enough; see the doc comment.
	for i := len(ids) - 1; i >= 0; i-- {
		setCreatedAt(t, db, ids[i], same)
	}

	got, _, err := orders.ListByOrigin(db.DB, originID, 100)
	testutil.MustNoErr(t, err, "list by origin")
	if len(got) != len(ids) {
		t.Fatalf("want %d orders, got %d", len(ids), len(got))
	}
	for i, want := range ids {
		if got[i].ID != want {
			t.Fatalf("position %d: got %d, want %d — identical timestamps are not being "+
				"broken deterministically", i, got[i].ID, want)
		}
	}
}

// TestListByOrigin_IsScopedToOneEpisode pins the join.
//
// The whole page attributes cost to one demand. An order from a neighbouring
// episode appearing here would put another demand's cost on this one's bill.
func TestListByOrigin_IsScopedToOneEpisode(t *testing.T) {
	db := testdb.Open(t)
	const mine = "1a1a1a1a-0000-4000-8000-000000000003"
	const theirs = "1a1a1a1a-0000-4000-8000-000000000004"
	seedOriginRow(t, db, mine)
	seedOriginRow(t, db, theirs)

	ours := seedOrderWithOrigin(t, db, "scope-mine", mine)
	seedOrderWithOrigin(t, db, "scope-theirs", theirs)

	// And an order with no origin at all — the consume-side / admin-action case,
	// which is the majority of rows on a real plant. It must not be swept in.
	noOrigin := &orders.Order{
		EdgeUUID: "scope-none", StationID: "devplant.line1", OrderType: "move",
		Status: protocol.StatusPending, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(noOrigin), "create origin-less order")

	got, _, err := orders.ListByOrigin(db.DB, mine, 100)
	testutil.MustNoErr(t, err, "list by origin")
	if len(got) != 1 || got[0].ID != ours.ID {
		t.Fatalf("want exactly order %d, got %v", ours.ID, idsOf(got))
	}
}

// TestListByOrigin_CapIsReportedNotSilent pins that a truncated list says so.
//
// Children-per-episode is unmeasured at a plant. A page that showed the first N
// as though they were all of them would understate what a demand cost, silently,
// on exactly the episodes worth reading.
func TestListByOrigin_CapIsReportedNotSilent(t *testing.T) {
	db := testdb.Open(t)
	const originID = "1a1a1a1a-0000-4000-8000-000000000005"
	seedOriginRow(t, db, originID)

	for i := 0; i < 4; i++ {
		seedOrderWithOrigin(t, db, fmt.Sprintf("cap-%d", i), originID)
	}

	got, truncated, err := orders.ListByOrigin(db.DB, originID, 2)
	testutil.MustNoErr(t, err, "list by origin")
	if len(got) != 2 {
		t.Fatalf("the cap must bound the result: got %d rows, want 2", len(got))
	}
	if !truncated {
		t.Error("four rows under a cap of two must REPORT truncation — a silently short " +
			"list is a list that lies about what a demand cost")
	}

	// Exactly at the cap is NOT truncated. The limit+1 fetch exists so this case
	// is answerable at all; a query for exactly `limit` rows cannot tell a full
	// page from a clipped one.
	exact, truncated, err := orders.ListByOrigin(db.DB, originID, 4)
	testutil.MustNoErr(t, err, "list by origin at the cap")
	if len(exact) != 4 || truncated {
		t.Errorf("exactly at the cap: got %d rows truncated=%v, want 4 rows and no truncation",
			len(exact), truncated)
	}
}

// TestListByOrigin_EmptyOriginIsRejected pins the guard on the degenerate call.
//
// origin_id is NULL for every order nothing asked for, and idx_orders_origin_id
// is partial on exactly that predicate. An empty string is not an episode, has
// no orders by definition, and issuing it would be a query off the index for a
// question with no answer.
func TestListByOrigin_EmptyOriginIsRejected(t *testing.T) {
	db := testdb.Open(t)
	_, _, err := orders.ListByOrigin(db.DB, "", 100)
	if err == nil {
		t.Fatal("an empty origin id must be rejected, not issued as a query")
	}
	// THE ERROR MUST BE OURS, NOT THE DRIVER'S. Found by verify-red: deleting
	// the guard entirely STILL errors, because Postgres rejects '' for a uuid
	// column — so "an error came back" proves nothing about the guard. What the
	// guard buys is the message: `invalid input syntax for type uuid` reads as a
	// database fault, when the truth is the caller asked a meaningless question.
	if !strings.Contains(err.Error(), "empty origin id") {
		t.Errorf("the rejection must name the caller's mistake, got %q — a driver-level "+
			"type error here reads as a database fault", err)
	}
}

func idsOf(os []*orders.Order) []int64 {
	out := make([]int64, 0, len(os))
	for _, o := range os {
		out = append(out, o.ID)
	}
	return out
}
