//go:build docker

package store_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// THE ASK'S SQL READERS, AT THE STORE TIER. Every reader of "Core has claimed
// this order" answers for a `pending` or `queued` row with an ask open exactly
// what it answers for the old in-create row (`dispatched`, no vendor id, no
// ask), and answers a `queued` row at rest the other way, so each assertion is
// about the ask and not about the status. The rows are written directly: the
// hand writes `dispatched` until the status moves on the fleet's answer, so no
// production path builds a resting status with an ask yet.

// askRow is one order in one of the four states, holding its bin by claim (the
// hand's hard hold).
type askRow struct {
	state  string
	status protocol.Status
	asked  string
	order  *orders.Order
	child  *orders.Order // a compound leg in the same state, under its own parent
	binID  int64
}

// askFixture is the four rows and the source node they draw from.
type askFixture struct {
	rows []*askRow
	src  string
}

func seedAskRows(t *testing.T, db *store.DB) askFixture {
	t.Helper()
	srcNode, line, bp := testdb.SetupTestData(t, db)
	rows := []*askRow{
		{state: "as built before, inside the create", status: protocol.StatusDispatched},
		{state: "pending with an ask open", status: protocol.StatusPending, asked: "sg-ask-p"},
		{state: "queued with an ask open", status: protocol.StatusQueued, asked: "sg-ask-q"},
		{state: "queued at rest", status: protocol.StatusQueued},
	}
	for i, r := range rows {
		bin := testdb.CreateBinAtNode(t, db, bp.Code, srcNode.ID, fmt.Sprintf("ASK-PIN-%d", i))
		r.binID = bin.ID
		r.order = testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("ask-pin-%d", i)
			o.OrderType = protocol.OrderTypeRetrieveEmpty
			o.SourceNode = srcNode.Name
			o.DeliveryNode = line.Name
		})
		parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("ask-pin-parent-%d", i)
		})
		r.child = testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = fmt.Sprintf("ask-pin-leg-%d", i)
			o.ParentOrderID = &parent.ID
			o.Sequence = 1
		})
		for _, id := range []int64{r.order.ID, r.child.ID} {
			_, err := db.Exec(`UPDATE orders SET status=$2, asked_vendor_order_id=$3 WHERE id=$1`, id, string(r.status), r.asked)
			testutil.MustNoErr(t, err, "seed "+r.state)
		}
		_, err := db.Exec(`UPDATE bins SET claimed_by=$2 WHERE id=$1`, bin.ID, r.order.ID)
		testutil.MustNoErr(t, err, "claim the bin for "+r.state)
	}
	return askFixture{rows: rows, src: srcNode.Name}
}

func idsHave(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func orderIDs(os []*orders.Order) []int64 {
	ids := make([]int64, 0, len(os))
	for _, o := range os {
		ids = append(ids, o.ID)
	}
	return ids
}

func queryIDs(t *testing.T, db *sql.DB, q string, args ...any) []int64 {
	t.Helper()
	rows, err := db.Query(q, args...)
	testutil.MustNoErr(t, err, q)
	defer rows.Close()
	cols, err := rows.Columns()
	testutil.MustNoErr(t, err, "columns")
	var ids []int64
	for rows.Next() {
		dest := make([]any, len(cols))
		var id int64
		dest[0] = &id
		for i := 1; i < len(cols); i++ {
			dest[i] = new(any)
		}
		testutil.MustNoErr(t, rows.Scan(dest...), "scan")
		ids = append(ids, id)
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return ids
}

// TestAskReaders_SQLAnswersAsTheOldCreateWindow: the SQL readers, each asked of
// all four rows. The orphan sweep writes, so it is read last.
func TestAskReaders_SQLAnswersAsTheOldCreateWindow(t *testing.T) {
	testdb.DisableWedgeSweep(t, "the rows pair states on purpose (a row at rest holding a claimed bin beside asked ones) to ask the readers about each, not a plant state")
	db := testdb.Open(t)
	fx := seedAskRows(t, db)
	rows, src := fx.rows, fx.src

	type reader struct {
		name string
		read func(r *askRow) bool
	}
	holdsAtRest := queryIDs(t, db.DB, orders.HoldsAtRestSQL())
	awaiting := queryIDs(t, db.DB, fmt.Sprintf(`SELECT id FROM orders WHERE %s`, orders.AwaitingFleetSQL("")))
	acq, err := db.ListAcquiringOrders()
	testutil.MustNoErr(t, err, "the scanner's population")
	refs, err := db.ListActiveOrdersBySourceRef([]string{src})
	testutil.MustNoErr(t, err, "the re-parent guard")
	pulls, err := orders.ClearPullCandidates(db.DB, 0, "no-home", []string{src})
	testutil.MustNoErr(t, err, "the home clear's pull candidates")
	readers := []reader{
		{"HoldsAtRestSQL lists it", func(r *askRow) bool { return idsHave(holdsAtRest, r.order.ID) }},
		{"AwaitingFleetSQL", func(r *askRow) bool { return idsHave(awaiting, r.order.ID) }},
		{"ListAcquiring (the scanner's population)", func(r *askRow) bool { return idsHave(orderIDs(acq), r.order.ID) }},
		{"ListActiveBySourceRef (the re-parent guard)", func(r *askRow) bool { return idsHave(orderIDs(refs), r.order.ID) }},
		{"ClearPullCandidates", func(r *askRow) bool { return idsHave(pulls, r.order.ID) }},
		{"GetNextChild (the leg the compound sends next)", func(r *askRow) bool {
			next, err := db.GetNextChildOrder(*r.child.ParentOrderID)
			return err == nil && next != nil && next.ID == r.child.ID
		}},
	}
	swept, err := reconciliation.ReleaseAcquiringOrphanClaims(db.DB)
	testutil.MustNoErr(t, err, "the acquiring-orphan sweep")
	if swept != 1 {
		t.Errorf("the acquiring-orphan sweep took %d claims, want 1 (the row at rest's)", swept)
	}
	readers = append(readers, reader{"ReleaseAcquiringOrphanClaims takes its claim", func(r *askRow) bool {
		var claimed sql.NullInt64
		testutil.MustNoErr(t, db.QueryRow(`SELECT claimed_by FROM bins WHERE id=$1`, r.binID).Scan(&claimed), "read the bin")
		return !claimed.Valid
	}})

	before, atRest := rows[0], rows[3]
	for _, rd := range readers {
		want := rd.read(before)
		for _, r := range rows[1:3] {
			if got := rd.read(r); got != want {
				t.Errorf("%s: %s reads %v, want %v (%s)", rd.name, r.state, got, want, before.state)
			}
		}
		if got := rd.read(atRest); got == want {
			t.Errorf("%s: %s reads %v, the same as an asked row: the pin would not see the ask", rd.name, atRest.state, got)
		}
	}
}

// TestAskClause_OnlyTheAnswerOrAnEndingMovesAnAskedRow: the status CAS refuses
// a row with an ask open (no row, no history, no version); the terminal CAS
// lands on it; the fleet's yes (FleetYes) lands only on a row asking for that
// job, writing `dispatched` and the job and closing the ask in one statement.
func TestAskClause_OnlyTheAnswerOrAnEndingMovesAnAskedRow(t *testing.T) {
	testdb.DisableWedgeSweep(t, "the rows pair states on purpose (a row at rest holding a claimed bin beside asked ones) to ask the readers about each, not a plant state")
	db := testdb.Open(t)
	rows := seedAskRows(t, db).rows
	asked := rows[2] // queued with an ask open
	history := func(id int64) int {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM order_history WHERE order_id=$1`, id).Scan(&n), "history")
		return n
	}
	before, err := db.GetOrder(asked.order.ID)
	testutil.MustNoErr(t, err, "read")
	h := history(asked.order.ID)
	_, moved, err := orders.UpdateStatusFromWithReason(db.DB, asked.order.ID, string(protocol.StatusQueued),
		string(protocol.StatusPending), "pin", "", "", nil)
	testutil.MustNoErr(t, err, "the status CAS")
	after := mustOrder(t, db, asked.order.ID)
	if moved || after.Status != before.Status || after.Version != before.Version || history(asked.order.ID) != h {
		t.Errorf("the status CAS moved an asked row: moved %v, %s v%d -> %s v%d, history %d -> %d",
			moved, before.Status, before.Version, after.Status, after.Version, h, history(asked.order.ID))
	}
	atRest := rows[3]
	if _, moved, err := orders.UpdateStatusFromWithReason(db.DB, atRest.order.ID, string(protocol.StatusQueued),
		string(protocol.StatusPending), "pin", "", "", nil); err != nil || !moved {
		t.Errorf("the status CAS refused a row at rest (moved %v, err %v): the clause is not about the ask", moved, err)
	}

	yes := func(id int64, from protocol.Status, job string) bool {
		_, moved, err := db.FleetYes(id, from, job, "", "vendor order "+job+" created", store.HistoryReason{}, protocol.OrderUpdate{})
		testutil.MustNoErr(t, err, "the yes write")
		return moved
	}
	if yes(rows[1].order.ID, protocol.StatusPending, "sg-some-other-job") {
		t.Error("the yes write naming another job landed on a row asking for a different one")
	}
	if o := mustOrder(t, db, rows[1].order.ID); o.AskedVendorOrderID != rows[1].asked || o.Status != protocol.StatusPending {
		t.Errorf("a refused yes changed the row: %s, ask %q", o.Status, o.AskedVendorOrderID)
	}
	if !yes(rows[1].order.ID, protocol.StatusPending, rows[1].asked) {
		t.Fatal("the yes write naming the asked job found no row")
	}
	if o := mustOrder(t, db, rows[1].order.ID); o.Status != protocol.StatusDispatched || o.AskedVendorOrderID != "" ||
		o.VendorOrderID != rows[1].asked || o.VendorState != "CREATED" {
		t.Errorf("the yes left %s, ask %q, vendor %q (%s); want dispatched naming the job with the ask closed",
			o.Status, o.AskedVendorOrderID, o.VendorOrderID, o.VendorState)
	}

	won, err := db.TerminalizeOrder(asked.order.ID, protocol.StatusCancelled, "pin: an ending lands")
	testutil.MustNoErr(t, err, "terminalize")
	if o := mustOrder(t, db, asked.order.ID); !won || o.Status != protocol.StatusCancelled || o.AskedVendorOrderID != asked.asked {
		t.Errorf("the ending: won %v, status %s, ask %q; want cancelled with the ask kept as the record",
			won, o.Status, o.AskedVendorOrderID)
	}
	if strings.TrimSpace(asked.asked) == "" {
		t.Fatal("fixture: the asked row has no ask")
	}
}

func mustOrder(t *testing.T, db *store.DB, id int64) *orders.Order {
	t.Helper()
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "read the order")
	return o
}
