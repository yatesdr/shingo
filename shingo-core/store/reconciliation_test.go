package store

import "testing"

func TestListOrderCompletionAnomalies(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "LINE-1", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	binType := &BinType{Code: "TOTE", Description: "Tote"}
	if err := db.CreateBinType(binType); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	bin := &Bin{BinTypeID: binType.ID, Label: "BIN-1", Description: "Test bin", NodeID: &node.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}

	claimed := &Order{
		EdgeUUID:     "claimed-terminal",
		StationID:    "edge.1",
		OrderType:    "retrieve",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: node.Name,
		BinID:        &bin.ID,
	}
	if err := db.CreateOrder(claimed); err != nil {
		t.Fatalf("create claimed order: %v", err)
	}
	if err := db.ClaimBin(bin.ID, claimed.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	if err := db.UpdateOrderStatus(claimed.ID, "failed", "test failure"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	missingBin := &Order{
		EdgeUUID:     "missing-bin",
		StationID:    "edge.1",
		OrderType:    "retrieve",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: node.Name,
	}
	if err := db.CreateOrder(missingBin); err != nil {
		t.Fatalf("create missing-bin order: %v", err)
	}
	if err := db.CompleteOrder(missingBin.ID); err != nil {
		t.Fatalf("complete order: %v", err)
	}

	confirmedNoComplete := &Order{
		EdgeUUID:     "confirmed-no-complete",
		StationID:    "edge.1",
		OrderType:    "retrieve",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: node.Name,
		BinID:        &bin.ID,
	}
	if err := db.CreateOrder(confirmedNoComplete); err != nil {
		t.Fatalf("create confirmed-no-complete order: %v", err)
	}
	if err := db.UpdateOrderStatus(confirmedNoComplete.ID, "confirmed", "receipt accepted"); err != nil {
		t.Fatalf("mark confirmed: %v", err)
	}

	anomalies, err := db.ListOrderCompletionAnomalies()
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

func TestGetReconciliationSummary(t *testing.T) {
	db := testDB(t)

	order := &Order{
		EdgeUUID:    "summary-order",
		StationID:   "edge.1",
		OrderType:   "retrieve",
		Status:      "pending",
		Quantity:    1,
		PayloadDesc: "payload",
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(order.ID, "confirmed", "partial completion"); err != nil {
		t.Fatalf("mark confirmed: %v", err)
	}
	if err := db.EnqueueOutbox("dispatch", []byte(`{}`), "order.update", "edge.1"); err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}

	summary, err := db.GetReconciliationSummary()
	if err != nil {
		t.Fatalf("get reconciliation summary: %v", err)
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
