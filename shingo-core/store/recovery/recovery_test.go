//go:build docker

package recovery_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/recovery"
)

func TestCoverage_RepairConfirmedOrderCompletion(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	origin := &nodes.Node{Name: "ORIGIN", Enabled: true}
	dest := &nodes.Node{Name: "DEST", Enabled: true}
	if err := nodes.Create(db.DB, origin); err != nil { t.Fatalf("create origin: %v", err) }
	if err := nodes.Create(db.DB, dest); err != nil { t.Fatalf("create dest: %v", err) }
	bt := &bins.BinType{Code: "TOTE"}
	if err := bins.CreateType(db.DB, bt); err != nil { t.Fatalf("create bin type: %v", err) }
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-1", NodeID: &origin.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil { t.Fatalf("create bin: %v", err) }
	order := &orders.Order{EdgeUUID: "repair-order-1", StationID: "edge.1", OrderType: "retrieve", Status: "confirmed", SourceNode: origin.Name, DeliveryNode: dest.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, order); err != nil { t.Fatalf("create order: %v", err) }
	if err := orders.UpdateStatus(db.DB, order.ID, "confirmed", "simulated"); err != nil { t.Fatalf("confirm order: %v", err) }
	if err := bins.Claim(db.DB, bin.ID, order.ID); err != nil { t.Fatalf("claim bin: %v", err) }
	if err := recovery.RepairConfirmedOrderCompletion(db.DB, order.ID, bin.ID, dest.ID, true, nil); err != nil { t.Fatalf("repair: %v", err) }
	gotOrder, err := orders.Get(db.DB, order.ID)
	if err != nil { t.Fatalf("get order: %v", err) }
	if gotOrder.CompletedAt == nil { t.Fatalf("expected completed_at to be set") }
	gotBin, err := bins.Get(db.DB, bin.ID)
	if err != nil { t.Fatalf("get bin: %v", err) }
	if gotBin.NodeID == nil || *gotBin.NodeID != dest.ID { t.Fatalf("expected bin at node %d, got %+v", dest.ID, gotBin.NodeID) }
	if gotBin.ClaimedBy != nil { t.Fatalf("expected bin claim to be released") }
	if gotBin.Status != "staged" { t.Fatalf("expected staged status, got %q", gotBin.Status) }
}

func TestCoverage_ReleaseTerminalBinClaimRejectsActiveOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "NODE-A", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil { t.Fatalf("create node: %v", err) }
	bt := &bins.BinType{Code: "TOTE-A"}
	if err := bins.CreateType(db.DB, bt); err != nil { t.Fatalf("create bin type: %v", err) }
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-A", NodeID: &node.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil { t.Fatalf("create bin: %v", err) }
	order := &orders.Order{EdgeUUID: "active-order", StationID: "edge.1", OrderType: "retrieve", Status: "dispatched", SourceNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, order); err != nil { t.Fatalf("create order: %v", err) }
	if err := bins.Claim(db.DB, bin.ID, order.ID); err != nil { t.Fatalf("claim bin: %v", err) }
	if _, err := recovery.ReleaseTerminalBinClaim(db.DB, bin.ID); err == nil { t.Fatalf("expected active claim release to fail") }
}

func TestCoverage_ReleaseTerminalBinClaimAllowsCancelledOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "NODE-B", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil { t.Fatalf("create node: %v", err) }
	bt := &bins.BinType{Code: "TOTE-B"}
	if err := bins.CreateType(db.DB, bt); err != nil { t.Fatalf("create bin type: %v", err) }
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-B", NodeID: &node.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil { t.Fatalf("create bin: %v", err) }
	order := &orders.Order{EdgeUUID: "cancelled-order", StationID: "edge.1", OrderType: "retrieve", Status: "cancelled", SourceNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, order); err != nil { t.Fatalf("create order: %v", err) }
	if err := orders.UpdateStatus(db.DB, order.ID, "cancelled", "simulated"); err != nil { t.Fatalf("cancel order: %v", err) }
	if err := bins.Claim(db.DB, bin.ID, order.ID); err != nil { t.Fatalf("claim bin: %v", err) }
	gotOrderID, err := recovery.ReleaseTerminalBinClaim(db.DB, bin.ID)
	if err != nil { t.Fatalf("release: %v", err) }
	if gotOrderID != order.ID { t.Fatalf("expected order id %d, got %d", order.ID, gotOrderID) }
	gotBin, err := bins.Get(db.DB, bin.ID)
	if err != nil { t.Fatalf("get bin: %v", err) }
	if gotBin.ClaimedBy != nil { t.Fatalf("expected claim to be cleared") }
}

func TestCoverage_RecordRecoveryAction_AndList(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if err := recovery.RecordAction(db.DB, "unstuck_order", "order", 42, "manual unblock", "alice"); err != nil { t.Fatalf("RecordAction: %v", err) }
	got, err := recovery.ListActions(db.DB, 10)
	if err != nil { t.Fatalf("ListActions: %v", err) }
	if len(got) != 1 { t.Fatalf("len = %d, want 1", len(got)) }
	if got[0].Action != "unstuck_order" { t.Errorf("Action = %q, want unstuck_order", got[0].Action) }
	if got[0].TargetType != "order" { t.Errorf("TargetType = %q, want order", got[0].TargetType) }
	if got[0].TargetID != 42 { t.Errorf("TargetID = %d, want 42", got[0].TargetID) }
	if got[0].Detail != "manual unblock" { t.Errorf("Detail = %q, want manual unblock", got[0].Detail) }
	if got[0].Actor != "alice" { t.Errorf("Actor = %q, want alice", got[0].Actor) }
	if got[0].CreatedAt.IsZero() { t.Error("CreatedAt should be populated") }
}

func TestCoverage_ListRecoveryActions_OrderAndLimit(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	recovery.RecordAction(db.DB, "a1", "order", 1, "first", "sys")
	recovery.RecordAction(db.DB, "a2", "order", 2, "second", "sys")
	recovery.RecordAction(db.DB, "a3", "order", 3, "third", "sys")
	recovery.RecordAction(db.DB, "a4", "order", 4, "fourth", "sys")
	all, err := recovery.ListActions(db.DB, 10)
	if err != nil { t.Fatalf("ListActions(10): %v", err) }
	if len(all) != 4 { t.Fatalf("all len = %d, want 4", len(all)) }
	if all[0].Action != "a4" { t.Errorf("newest.Action = %q, want a4", all[0].Action) }
	if all[3].Action != "a1" { t.Errorf("oldest.Action = %q, want a1", all[3].Action) }
	limited, err := recovery.ListActions(db.DB, 2)
	if err != nil { t.Fatalf("ListActions(2): %v", err) }
	if len(limited) != 2 { t.Fatalf("limited len = %d, want 2", len(limited)) }
	if limited[0].Action != "a4" { t.Errorf("limited[0].Action = %q, want a4", limited[0].Action) }
	if limited[1].Action != "a3" { t.Errorf("limited[1].Action = %q, want a3", limited[1].Action) }
}
