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
	if err := nodes.Create(db.DB, node); err != nil { t.Fatalf("create node: %v", err) }
	binType := &bins.BinType{Code: "TOTE", Description: "Tote"}
	if err := bins.CreateType(db.DB, binType); err != nil { t.Fatalf("create bin type: %v", err) }
	bin := &bins.Bin{BinTypeID: binType.ID, Label: "BIN-1", Description: "Test bin", NodeID: &node.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil { t.Fatalf("create bin: %v", err) }

	claimed := &orders.Order{EdgeUUID: "claimed-terminal", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, claimed); err != nil { t.Fatalf("create claimed order: %v", err) }
	if err := bins.Claim(db.DB, bin.ID, claimed.ID); err != nil { t.Fatalf("claim bin: %v", err) }
	if err := orders.UpdateStatus(db.DB, claimed.ID, "failed", "test failure"); err != nil { t.Fatalf("mark failed: %v", err) }

	missingBin := &orders.Order{EdgeUUID: "missing-bin", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: node.Name}
	if err := orders.Create(db.DB, missingBin); err != nil { t.Fatalf("create missing-bin order: %v", err) }
	if err := orders.Complete(db.DB, missingBin.ID); err != nil { t.Fatalf("complete order: %v", err) }

	confirmedNoComplete := &orders.Order{EdgeUUID: "confirmed-no-complete", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, confirmedNoComplete); err != nil { t.Fatalf("create confirmed-no-complete order: %v", err) }
	if err := orders.UpdateStatus(db.DB, confirmedNoComplete.ID, "confirmed", "receipt accepted"); err != nil { t.Fatalf("mark confirmed: %v", err) }

	anomalies, err := reconciliation.ListOrderCompletionAnomalies(db.DB)
	if err != nil { t.Fatalf("list anomalies: %v", err) }
	if len(anomalies) != 3 { t.Fatalf("expected 3 anomalies, got %d", len(anomalies)) }
	issues := map[string]bool{}
	for _, a := range anomalies { issues[a.Issue] = true }
	if !issues["terminal_order_still_claims_bin"] { t.Fatalf("expected terminal_order_still_claims_bin anomaly") }
	if !issues["completed_order_missing_bin"] { t.Fatalf("expected completed_order_missing_bin anomaly") }
	if !issues["confirmed_without_completed_at"] { t.Fatalf("expected confirmed_without_completed_at anomaly") }
}

func TestCoverage_GetReconciliationSummary(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	order := &orders.Order{EdgeUUID: "summary-order", StationID: "edge.1", OrderType: "retrieve", Status: "pending", Quantity: 1, PayloadDesc: "payload"}
	if err := orders.Create(db.DB, order); err != nil { t.Fatalf("create order: %v", err) }
	if err := orders.UpdateStatus(db.DB, order.ID, "confirmed", "partial completion"); err != nil { t.Fatalf("mark confirmed: %v", err) }
	if err := messaging.EnqueueOutbox(db.DB, "dispatch", []byte(`{}`), "order.update", "edge.1"); err != nil { t.Fatalf("enqueue outbox: %v", err) }

	summary, err := reconciliation.GetSummary(db.DB)
	if err != nil { t.Fatalf("get summary: %v", err) }
	if summary.CompletionAnomalies != 1 { t.Fatalf("expected 1 completion anomaly, got %d", summary.CompletionAnomalies) }
	if summary.OutboxPending != 1 { t.Fatalf("expected 1 pending outbox message, got %d", summary.OutboxPending) }
	if summary.DeadLetters != 0 { t.Fatalf("expected 0 dead letters, got %d", summary.DeadLetters) }
	if summary.OldestOutboxAt == nil { t.Fatalf("expected oldest outbox timestamp") }
	if summary.Status != "critical" { t.Fatalf("expected critical status, got %q", summary.Status) }
}
