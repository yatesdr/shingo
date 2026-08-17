//go:build docker

package recovery_test

import (
	"testing"

	"shingo/protocol"
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
	if err := nodes.Create(db.DB, origin); err != nil {
		t.Fatalf("create origin: %v", err)
	}
	if err := nodes.Create(db.DB, dest); err != nil {
		t.Fatalf("create dest: %v", err)
	}
	bt := &bins.BinType{Code: "TOTE"}
	if err := bins.CreateType(db.DB, bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-1", NodeID: &origin.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := &orders.Order{EdgeUUID: "repair-order-1", StationID: "edge.1", OrderType: "retrieve", Status: "confirmed", SourceNode: origin.Name, DeliveryNode: dest.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	testdb.SeedOrderStatus(t, db, order.ID, "confirmed", "simulated")
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
	if err := recovery.RepairConfirmedOrderCompletion(db.DB, order.ID, bin.ID, dest.ID, true, nil); err != nil {
		t.Fatalf("repair: %v", err)
	}
	gotOrder, err := orders.Get(db.DB, order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if gotOrder.CompletedAt == nil {
		t.Fatalf("expected completed_at to be set")
	}
	gotBin, err := bins.Get(db.DB, bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if gotBin.NodeID == nil || *gotBin.NodeID != dest.ID {
		t.Fatalf("expected bin at node %d, got %+v", dest.ID, gotBin.NodeID)
	}
	if gotBin.ClaimedBy != nil {
		t.Fatalf("expected bin claim to be released")
	}
	if gotBin.Status != "staged" {
		t.Fatalf("expected staged status, got %q", gotBin.Status)
	}
}

// TestRepair_ScopesItsUnclaimAndCouplesTheReservation covers the two things the
// repair path was missing: the owner-scoped unclaim that 445f79eb added to
// applyArrival and never carried here, and the reservation release that this
// path never had at all.
//
// WHY THE REPAIR PATH IS THE WORSE PLACE FOR IT. A repair runs precisely when
// the ordinary arrival already failed — the bin has been sitting at a
// destination long enough for an operator to open the recovery page and press
// the button. That is ample time for another order to have claimed it, so the
// foreign-claim case is not a corner here; it is the expected shape.
//
// TWO ARMS. The own-claim arm is the coupling assertion (claim cleared ⇒
// reservation released); the foreign arm is the scoping one (claim spared ⇒
// reservation spared, because a reservation whose claim we just correctly left
// standing must not be stripped — the same defect one layer down).
//
// MUTATION (verified): drop the claimed_by predicate and the foreign arm fires
// on the claim; drop the RowsAffected guard around ReleaseByBin and it fires on
// the reservation instead.
func TestRepair_ScopesItsUnclaimAndCouplesTheReservation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	mkNode := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		if err := nodes.Create(db.DB, n); err != nil {
			t.Fatalf("create node %s: %v", name, err)
		}
		return n
	}
	origin := mkNode("SCOPE-ORIGIN")
	destOwn := mkNode("SCOPE-DEST-OWN")
	destForeign := mkNode("SCOPE-DEST-FOREIGN")

	bt := &bins.BinType{Code: "SCOPE-TOTE"}
	if err := bins.CreateType(db.DB, bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	mkBin := func(label string) *bins.Bin {
		b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &origin.ID, Status: "available"}
		if err := bins.Create(db.DB, b); err != nil {
			t.Fatalf("create bin %s: %v", label, err)
		}
		return b
	}
	mkOrder := func(uuid string, status protocol.Status, binID *int64, dest string) *orders.Order {
		o := &orders.Order{EdgeUUID: uuid, StationID: "edge.1", OrderType: "retrieve",
			Status: status, SourceNode: origin.Name, DeliveryNode: dest, BinID: binID}
		if err := orders.Create(db.DB, o); err != nil {
			t.Fatalf("create order %s: %v", uuid, err)
		}
		return o
	}
	countRes := func(binID int64) int {
		var n int
		if err := db.DB.QueryRow(`SELECT count(*) FROM reservations WHERE bin_id=$1`, binID).Scan(&n); err != nil {
			t.Fatalf("count reservations for bin %d: %v", binID, err)
		}
		return n
	}

	// ── ARM 1: the repairing order owns the claim. Cleared, reservation with it.
	ownBin := mkBin("SCOPE-BIN-OWN")
	owner := mkOrder("scope-repair-own", "confirmed", &ownBin.ID, destOwn.Name)
	testdb.SeedOrderStatus(t, db, owner.ID, "confirmed", "simulated")
	testdb.ClaimBinForTest(t, db, ownBin.ID, owner.ID)
	if countRes(ownBin.ID) == 0 {
		t.Fatalf("fixture: expected a reservation on the own-claim bin before repair")
	}
	if err := recovery.RepairConfirmedOrderCompletion(db.DB, owner.ID, ownBin.ID, destOwn.ID, false, nil); err != nil {
		t.Fatalf("repair (own claim): %v", err)
	}
	gotOwn, err := bins.Get(db.DB, ownBin.ID)
	if err != nil {
		t.Fatalf("get own bin: %v", err)
	}
	if gotOwn.ClaimedBy != nil {
		t.Errorf("own-claim bin still claimed by %d — a repair is a handoff and gives up its own claim",
			*gotOwn.ClaimedBy)
	}
	if n := countRes(ownBin.ID); n != 0 {
		t.Errorf("reservations on own-claim bin = %d, want 0 — a bin's reservation lives exactly as "+
			"long as its claim, and this path had no ReleaseByBin at all", n)
	}

	// ── ARM 2: somebody else holds the claim. Placed anyway, claim spared.
	foreignBin := mkBin("SCOPE-BIN-FOREIGN")
	repairer := mkOrder("scope-repair-foreign", "confirmed", &foreignBin.ID, destForeign.Name)
	testdb.SeedOrderStatus(t, db, repairer.ID, "confirmed", "simulated")
	// The rightful owner stays out of the acquiring set so the fulfillment
	// scanner cannot structurally fail it and release the claim under test.
	holder := mkOrder("scope-repair-holder", "in_transit", nil, destForeign.Name)
	testdb.ClaimBinForTest(t, db, foreignBin.ID, holder.ID)

	if err := recovery.RepairConfirmedOrderCompletion(db.DB, repairer.ID, foreignBin.ID, destForeign.ID, false, nil); err != nil {
		t.Fatalf("repair (foreign claim): %v", err)
	}
	gotForeign, err := bins.Get(db.DB, foreignBin.ID)
	if err != nil {
		t.Fatalf("get foreign bin: %v", err)
	}
	// The placement is never refused — attribution and ownership do not block
	// the physical record catching up.
	if gotForeign.NodeID == nil || *gotForeign.NodeID != destForeign.ID {
		t.Errorf("foreign-claimed bin is at %v, want dest %d — the repair still places the bin",
			gotForeign.NodeID, destForeign.ID)
	}
	if gotForeign.ClaimedBy == nil {
		t.Errorf("foreign-claimed bin: the repair cleared a claim it does not own. This is the " +
			"445f79eb defect on the repair path — the holder then picks up its OWN bin with no " +
			"claim on it and final delivery refuses a robot carrying a bin nobody owns.")
	} else if *gotForeign.ClaimedBy != holder.ID {
		t.Errorf("foreign-claimed bin claim = %d, want holder %d", *gotForeign.ClaimedBy, holder.ID)
	}
	if n := countRes(foreignBin.ID); n == 0 {
		t.Errorf("reservations on foreign-claimed bin = 0 — releasing unconditionally strips the " +
			"reservation off a bin whose claim was correctly left standing, which is the same " +
			"defect one layer down")
	}
}

func TestCoverage_ReleaseTerminalBinClaimRejectsActiveOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "NODE-A", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bt := &bins.BinType{Code: "TOTE-A"}
	if err := bins.CreateType(db.DB, bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-A", NodeID: &node.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := &orders.Order{EdgeUUID: "active-order", StationID: "edge.1", OrderType: "retrieve", Status: "dispatched", SourceNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
	if _, err := recovery.ReleaseTerminalBinClaim(db.DB, bin.ID); err == nil {
		t.Fatalf("expected active claim release to fail")
	}
}

func TestCoverage_ReleaseTerminalBinClaimAllowsCancelledOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "NODE-B", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	bt := &bins.BinType{Code: "TOTE-B"}
	if err := bins.CreateType(db.DB, bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-B", NodeID: &node.ID, Status: "available"}
	if err := bins.Create(db.DB, bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	order := &orders.Order{EdgeUUID: "cancelled-order", StationID: "edge.1", OrderType: "retrieve", Status: "cancelled", SourceNode: node.Name, BinID: &bin.ID}
	if err := orders.Create(db.DB, order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	testdb.SeedOrderStatus(t, db, order.ID, "cancelled", "simulated")
	testdb.ClaimBinForTest(t, db, bin.ID, order.ID)
	gotOrderID, err := recovery.ReleaseTerminalBinClaim(db.DB, bin.ID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if gotOrderID != order.ID {
		t.Fatalf("expected order id %d, got %d", order.ID, gotOrderID)
	}
	gotBin, err := bins.Get(db.DB, bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if gotBin.ClaimedBy != nil {
		t.Fatalf("expected claim to be cleared")
	}
}

func TestCoverage_RecordRecoveryAction_AndList(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	if err := recovery.RecordAction(db.DB, "unstuck_order", "order", 42, "manual unblock", "alice"); err != nil {
		t.Fatalf("RecordAction: %v", err)
	}
	got, err := recovery.ListActions(db.DB, 10)
	if err != nil {
		t.Fatalf("ListActions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Action != "unstuck_order" {
		t.Errorf("Action = %q, want unstuck_order", got[0].Action)
	}
	if got[0].TargetType != "order" {
		t.Errorf("TargetType = %q, want order", got[0].TargetType)
	}
	if got[0].TargetID != 42 {
		t.Errorf("TargetID = %d, want 42", got[0].TargetID)
	}
	if got[0].Detail != "manual unblock" {
		t.Errorf("Detail = %q, want manual unblock", got[0].Detail)
	}
	if got[0].Actor != "alice" {
		t.Errorf("Actor = %q, want alice", got[0].Actor)
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("CreatedAt should be populated")
	}
}

func TestCoverage_ListRecoveryActions_OrderAndLimit(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	recovery.RecordAction(db.DB, "a1", "order", 1, "first", "sys")
	recovery.RecordAction(db.DB, "a2", "order", 2, "second", "sys")
	recovery.RecordAction(db.DB, "a3", "order", 3, "third", "sys")
	recovery.RecordAction(db.DB, "a4", "order", 4, "fourth", "sys")
	all, err := recovery.ListActions(db.DB, 10)
	if err != nil {
		t.Fatalf("ListActions(10): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("all len = %d, want 4", len(all))
	}
	if all[0].Action != "a4" {
		t.Errorf("newest.Action = %q, want a4", all[0].Action)
	}
	if all[3].Action != "a1" {
		t.Errorf("oldest.Action = %q, want a1", all[3].Action)
	}
	limited, err := recovery.ListActions(db.DB, 2)
	if err != nil {
		t.Fatalf("ListActions(2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited len = %d, want 2", len(limited))
	}
	if limited[0].Action != "a4" {
		t.Errorf("limited[0].Action = %q, want a4", limited[0].Action)
	}
	if limited[1].Action != "a3" {
		t.Errorf("limited[1].Action = %q, want a3", limited[1].Action)
	}
}
