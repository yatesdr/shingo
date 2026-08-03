//go:build docker

package reconciliation_test

import (
	"testing"

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
		if _, err := db.DB.Exec(`UPDATE orders SET status=$1, updated_at = NOW() - ($2 * INTERVAL '1 second') WHERE id=$3`,
			status, ageSeconds, o.ID); err != nil {
			t.Fatalf("backdate %s: %v", uuid, err)
		}
		return o.ID
	}

	youngQueued := mk("q-young", "queued", 3600)     // 1h — under the 2h queued bound
	oldQueued := mk("q-old", "queued", 10800)        // 3h — over it
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
