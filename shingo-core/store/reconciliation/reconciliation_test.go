//go:build docker

package reconciliation_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/messaging"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

func TestCoverage_ListOrderCompletionAnomalies(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "LINE-1", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	binType := &bins.BinType{Code: "TOTE", Description: "Tote"}
	if err := bins.CreateType(db.DB, binType); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: binType.ID, Label: "BIN-1", Description: "Test bin", NodeID: &node.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}

	claimed := &orders.Order{EdgeUUID: "claimed-terminal", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, claimed); err != nil {
		t.Fatalf("create claimed order: %v", err)
	}
	testdb.ClaimBinForTest(t, db, bin.ID, claimed.ID)
	testdb.SeedOrderStatus(t, db, claimed.ID, "failed", "test failure")

	missingBin := &orders.Order{EdgeUUID: "missing-bin", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: node.Name}
	if err := orders.Create(db.DB, missingBin); err != nil {
		t.Fatalf("create missing-bin order: %v", err)
	}
	if err := orders.Complete(db.DB, missingBin.ID); err != nil {
		t.Fatalf("complete order: %v", err)
	}

	confirmedNoComplete := &orders.Order{EdgeUUID: "confirmed-no-complete", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, confirmedNoComplete); err != nil {
		t.Fatalf("create confirmed-no-complete order: %v", err)
	}
	testdb.SeedOrderStatus(t, db, confirmedNoComplete.ID, "confirmed", "receipt accepted")

	anomalies, err := reconciliation.ListOrderCompletionAnomalies(db.DB)
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	if len(anomalies) != 3 {
		t.Fatalf("expected 3 anomalies, got %d", len(anomalies))
	}
	issues := map[string]bool{}
	for _, a := range anomalies {
		issues[a.Issue] = true
	}
	if !issues["terminal_order_still_claims_bin"] {
		t.Fatalf("expected terminal_order_still_claims_bin anomaly")
	}
	if !issues["completed_order_missing_bin"] {
		t.Fatalf("expected completed_order_missing_bin anomaly")
	}
	if !issues["confirmed_without_completed_at"] {
		t.Fatalf("expected confirmed_without_completed_at anomaly")
	}
}

func TestCoverage_GetReconciliationSummary(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	order := &orders.Order{EdgeUUID: "summary-order", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, PayloadDesc: "payload"}
	if err := orders.Create(db.DB, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	testdb.SeedOrderStatus(t, db, order.ID, "confirmed", "partial completion")
	if err := messaging.EnqueueOutbox(db.DB, "dispatch", []byte(`{}`), "order.update", "edge.1"); err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}

	summary, err := reconciliation.GetSummary(db.DB)
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if summary.CompletionAnomalies != 1 {
		t.Fatalf("expected 1 completion anomaly, got %d", summary.CompletionAnomalies)
	}
	if summary.OutboxPending != 1 {
		t.Fatalf("expected 1 pending outbox message, got %d", summary.OutboxPending)
	}
	if summary.DeadLetters != 0 {
		t.Fatalf("expected 0 dead letters, got %d", summary.DeadLetters)
	}
	if summary.OldestOutboxAt == nil {
		t.Fatalf("expected oldest outbox timestamp")
	}
	if summary.Status != "critical" {
		t.Fatalf("expected critical status, got %q", summary.Status)
	}
}

// TestListAnomalies_QueuedGetsTheLongerBound exercises the stuck-order query
// THROUGH THE DRIVER, against real Postgres.
//
// That is the entire point of it, and it exists because its absence shipped a
// live 500. The query splits its staleness bound with
// `CASE WHEN status = $3 THEN $2::int ELSE $1::int END * INTERVAL '1 second'`.
// The driver sends those parameters untyped, so without the ::int casts
// Postgres infers the CASE result as `text` and the query fails at RUNTIME with
// "operator does not exist: text * interval" — while building clean, vetting
// clean, and passing a hand-run `PREPARE stuckq(int, int, text)` in psql, since
// declaring the types is precisely what the driver does not do.
//
// So this test is not really about the thresholds. It is about the query being
// executed at all, by the thing that executes it in production.
func TestListAnomalies_QueuedGetsTheLongerBound(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "this fixture BACKDATES a `dispatched` order with no vendor id on purpose — that state is what the sweep under test is for, so the crash-sliver clause is correctly reporting the thing being arranged")
	db := testdb.Open(t)

	node := &nodes.Node{Name: "STUCK-LINE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	mk := func(uuid, status string, ageSeconds int) int64 {
		o := &orders.Order{EdgeUUID: uuid, StationID: "edge.1", OrderType: "retrieve_empty",
			Status: "pending", Quantity: 1, DeliveryNode: node.Name}
		if err := orders.Create(db.DB, o); err != nil {
			t.Fatalf("create %s: %v", uuid, err)
		}
		// AGE IS AGE SINCE THE LAST TRANSITION, not since the last row touch —
		// the detector's clock is an order_history row, so a fixture that
		// backdates only updated_at describes an order that HAS progressed
		// recently and correctly raises nothing.
		//
		// THE BIRTH ROW IS BACKDATED TOO. Every order now gets a history row from
		// the INSERT (orders.Create), so the COALESCE fallback to orders.created_at
		// no longer reaches — leaving the birth row at NOW would describe an order
		// created a second ago, which is not the order this fixture is about. In
		// production the two instants are the same value, which is why this changes
		// no reading of a real plant.
		if _, err := db.DB.Exec(`UPDATE orders
			SET status=$1,
			    updated_at = NOW() - ($2 * INTERVAL '1 second'),
			    created_at = NOW() - ($2 * INTERVAL '1 second')
			WHERE id=$3`, status, ageSeconds, o.ID); err != nil {
			t.Fatalf("backdate %s: %v", uuid, err)
		}
		if _, err := db.DB.Exec(
			`UPDATE order_history SET created_at = NOW() - ($1 * INTERVAL '1 second') WHERE order_id=$2`,
			ageSeconds, o.ID); err != nil {
			t.Fatalf("backdate %s birth row: %v", uuid, err)
		}
		return o.ID
	}

	youngQueued := mk("q-young", "queued", 3600)       // 1h — under the 2h queued bound
	oldQueued := mk("q-old", "queued", 10800)          // 3h — over it
	staleDispatched := mk("d-old", "dispatched", 3600) // 1h — 30m bound still applies

	anomalies, err := reconciliation.ListAnomalies(db.DB)
	if err != nil {
		// The cast regression lands exactly here.
		t.Fatalf("ListAnomalies through the driver: %v", err)
	}
	flagged := map[int64]bool{}
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" && a.OrderID != nil {
			flagged[*a.OrderID] = true
		}
	}

	if flagged[youngQueued] {
		t.Error("a queued order waiting 1h was flagged; waiting is what queued is FOR, and a board that fires on ordinary material churn gets ignored")
	}
	if !flagged[oldQueued] {
		t.Error("a queued order wedged for 3h raised nothing — no sweep covers queued, so this anomaly is the only thing that reports it")
	}
	if !flagged[staleDispatched] {
		t.Error("a dispatched order stale for 1h was not flagged; the longer bound must apply to queued ONLY, not widen to every status")
	}
}

// TestCompletionAnomalies_ACompoundParentIsNotAMissingBin pins the exemption
// that turned a permanently-red health strip back into an instrument.
//
// ── WHAT WAS BEING REPORTED ───────────────────────────────────────────────
//
// completed_order_missing_bin was written for a SINGLE-BIN order whose
// UpdateOrderBinID never persisted: it reaches FINISHED with its bin still
// sitting at source, and nothing else notices. A real defect, worth a row.
//
// A COMPOUND PARENT matched it too, and matched it forever. A parent carries no
// bin by construction — a service dig's parent is a container with no cargo and
// no robot, and a plain buried retrieve re-parents the demand so its own fetch
// becomes a leg. The condition is permanent for that shape, so every dig the
// plant ever ran added one more permanent anomaly.
//
// Measured on the lane-stress rig 2026-08-13: twelve anomalies, ten service digs
// and two buried retrieves, every one a parent whose legs had delivered
// correctly — including the final retrieve legs to the line. ZERO completed
// orders with no bin and no legs, so the predicate had no true positives at all,
// and the strip read "Core degraded" for the whole run on the strength of them.
//
// ── AND THE TRUE POSITIVE STILL FIRES ─────────────────────────────────────
//
// The second half is the one that matters: a childless order that completed
// without a bin is still the defect this was built for, and exempting parents
// must not quietly exempt it too.
//
// MUTATION: drop the child-rows clause. The parent is reported again, and the
// count in the first half goes to 2.
func TestCompletionAnomalies_ACompoundParentIsNotAMissingBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "CP-DEST", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	// A COMPOUND PARENT: no bin of its own, one leg that carries the bin.
	parent := &orders.Order{EdgeUUID: "cp-parent", StationID: "edge.1", OrderType: "move",
		Status: "pending", Quantity: 1, DeliveryNode: node.Name}
	if err := orders.Create(db.DB, parent); err != nil {
		t.Fatalf("create the compound parent: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, "PART-A", node.ID, "CP-BIN")
	leg := &orders.Order{EdgeUUID: "cp-leg", StationID: "edge.1", OrderType: "move",
		Status: "pending", Quantity: 1, DeliveryNode: node.Name,
		ParentOrderID: &parent.ID, BinID: &bin.ID}
	if err := orders.Create(db.DB, leg); err != nil {
		t.Fatalf("create the leg: %v", err)
	}
	if err := orders.Complete(db.DB, parent.ID); err != nil {
		t.Fatalf("complete the parent: %v", err)
	}

	anomalies, err := reconciliation.ListOrderCompletionAnomalies(db.DB)
	if err != nil {
		t.Fatalf("list anomalies: %v", err)
	}
	for _, a := range anomalies {
		if a.OrderID == parent.ID {
			t.Fatalf("compound parent %d was reported as %q. It carries no bin because its LEGS do — "+
				"the condition is permanent for this shape, so every dig the plant runs would add one "+
				"more anomaly that can never be cleared", parent.ID, a.Issue)
		}
	}

	// ── THE TRUE POSITIVE IS UNTOUCHED ───────────────────────────────────────
	childless := &orders.Order{EdgeUUID: "cp-childless", StationID: "edge.1", OrderType: "retrieve",
		Status: "pending", Quantity: 1, DeliveryNode: node.Name}
	if err := orders.Create(db.DB, childless); err != nil {
		t.Fatalf("create the childless order: %v", err)
	}
	if err := orders.Complete(db.DB, childless.ID); err != nil {
		t.Fatalf("complete the childless order: %v", err)
	}

	anomalies, err = reconciliation.ListOrderCompletionAnomalies(db.DB)
	if err != nil {
		t.Fatalf("re-list anomalies: %v", err)
	}
	var found bool
	for _, a := range anomalies {
		if a.OrderID == childless.ID && a.Issue == "completed_order_missing_bin" {
			found = true
		}
	}
	if !found {
		t.Errorf("order %d completed with no bin and no legs and was NOT reported. That is the defect "+
			"this anomaly exists for — an order that reached FINISHED with its bin still at source",
			childless.ID)
	}
}

// TestListAnomalies_ARetryingOrderCannotRefreshItsOwnStalenessTimer is the pin
// on the progress signal.
//
// orders.updated_at means TOUCHED, not PROGRESSED. Roughly twenty writers stamp
// it, and several run on the dispatch retry loop — so an order that re-enters the
// scanner every tick, is refused, and parks again keeps moving its own updated_at
// forward. The one order this detector exists to catch is therefore the one it
// cannot see, and it fails SILENTLY: the board reads "no stuck orders" while a
// wedged order sits in `sourcing` indefinitely.
//
// MEASURED on the sim wedge of 2026-08-28 (main 1a6b6d23): order 143 sat in
// `sourcing` behind a leaked lane hold for over two hours of sim time. Its last
// real transition was 19:00:55; its updated_at read 19:16:55 and kept climbing,
// against a bound it would never reach.
//
// The fixture below is that order: a fresh updated_at over an old last
// transition. Under the updated_at clock it raises nothing.
func TestListAnomalies_ARetryingOrderCannotRefreshItsOwnStalenessTimer(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "RETRY-LINE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	o := &orders.Order{EdgeUUID: "retry-wedged", StationID: "edge.1", OrderType: "retrieve_empty",
		Status: "pending", Quantity: 1, DeliveryNode: node.Name}
	if err := orders.Create(db.DB, o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Born an hour and a bit ago. The INSERT writes a birth history row
	// (orders.Create), and leaving it at NOW would make the plant's newest
	// transition this order's own creation — which is not the order under test.
	if _, err := db.DB.Exec(
		`UPDATE order_history SET created_at = NOW() - INTERVAL '65 minutes' WHERE order_id=$1`,
		o.ID); err != nil {
		t.Fatalf("backdate the birth row: %v", err)
	}
	// Parked in `sourcing` an hour ago — that is the last thing that HAPPENED.
	if _, err := db.DB.Exec(
		`INSERT INTO order_history (order_id, status, detail, created_at)
		 VALUES ($1, 'sourcing', 'reserving source bins', NOW() - INTERVAL '1 hour')`, o.ID); err != nil {
		t.Fatalf("seed the last real transition: %v", err)
	}
	// ...and touched one second ago by the retry loop, over and over since.
	if _, err := db.DB.Exec(
		`UPDATE orders SET status='sourcing',
		        created_at = NOW() - INTERVAL '1 hour',
		        updated_at = NOW() - INTERVAL '1 second'
		 WHERE id=$1`, o.ID); err != nil {
		t.Fatalf("touch without progressing: %v", err)
	}

	anomalies, err := reconciliation.ListAnomalies(db.DB)
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" && a.OrderID != nil && *a.OrderID == o.ID {
			if a.ObservedAt == nil || time.Since(*a.ObservedAt) < 30*time.Minute {
				t.Fatalf("order %d was flagged, but ObservedAt is %v — the row must report the LAST "+
					"TRANSITION, not the last touch, or the operator reads a fresh timestamp beside a "+
					"stuck-order alarm and disbelieves the alarm.", o.ID, a.ObservedAt)
			}
			return // flagged, and dated honestly
		}
	}
	t.Fatalf("order %d has not changed status in an hour and was NOT flagged. Its updated_at is one "+
		"second old because the retry loop keeps touching it — which is precisely the order that "+
		"needs reporting, and precisely the one an updated_at clock cannot see.", o.ID)
}

// TestListAnomalies_AnOrderThatJustTransitionedIsNotStuck is the pin that makes
// the LATERAL load-bearing, and without it the join is decoration.
//
// The detector's clock is MAX(order_history.created_at), with orders.created_at
// as the COALESCE fallback for a row that has never transitioned. Every other
// fixture in this file backdates BOTH — which is right for what those tests
// assert, and means the fallback alone satisfies all of them. Delete the LATERAL,
// compare o.created_at directly, and the suite stays green while the change this
// file exists to protect is gone.
//
// That mutation is not contrived. Inside the lateral, `MAX(created_at)` and
// `order_id` are unqualified while `orders` is also in scope with a created_at of
// its own; Postgres resolves them to the inner table, which is correct — but an
// editor "tidying" them to `o.` turns the aggregate into a correlated constant.
// The query still runs, still returns rows, and every long-lived order reports as
// stuck from the moment it was created.
//
// So this is the other direction: an OLD order that has JUST MOVED. Its birth is
// two hours behind the bound and its last transition is a second old. It is the
// busiest possible order, and it must raise nothing.
func TestListAnomalies_AnOrderThatJustTransitionedIsNotStuck(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "MOVING-LINE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	o := &orders.Order{EdgeUUID: "moving-along", StationID: "edge.1", OrderType: "retrieve_empty",
		Status: "pending", Quantity: 1, DeliveryNode: node.Name}
	if err := orders.Create(db.DB, o); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Born two hours ago — well past both bounds on the fallback clock.
	if _, err := db.DB.Exec(
		`UPDATE orders SET status='in_transit', created_at = NOW() - INTERVAL '2 hours'
		 WHERE id=$1`, o.ID); err != nil {
		t.Fatalf("age the order: %v", err)
	}
	// ...and progressing normally the whole time, most recently a second ago.
	for _, h := range []struct {
		status string
		ago    string
	}{
		{"sourcing", "119 minutes"},
		{"dispatched", "90 minutes"},
		{"in_transit", "1 second"},
	} {
		if _, err := db.DB.Exec(
			`INSERT INTO order_history (order_id, status, detail, created_at)
			 VALUES ($1, $2, 'progressing', NOW() - $3::interval)`, o.ID, h.status, h.ago); err != nil {
			t.Fatalf("seed transition %s: %v", h.status, err)
		}
	}

	anomalies, err := reconciliation.ListAnomalies(db.DB)
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" && a.OrderID != nil && *a.OrderID == o.ID {
			t.Fatalf("order %d changed status ONE SECOND ago and was flagged as stuck (observed_at "+
				"%v). Only its created_at is old. Reporting a moving order teaches operators to "+
				"ignore the board, and it means the detector is reading the order's birth rather "+
				"than the order_history row the LATERAL exists to find.", o.ID, a.ObservedAt)
		}
	}
}
