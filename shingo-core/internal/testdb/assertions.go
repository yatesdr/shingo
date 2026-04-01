package testdb

import (
	"testing"

	"shingocore/store"
)

// RequireOrder fetches an order by UUID and fails the test if not found.
func RequireOrder(t *testing.T, db *store.DB, uuid string) *store.Order {
	t.Helper()
	order, err := db.GetOrderByUUID(uuid)
	if err != nil {
		t.Fatalf("get order %s: %v", uuid, err)
	}
	return order
}

// AssertOrderStatus fetches an order by UUID and asserts its status matches want.
// Returns the order for further inspection.
func AssertOrderStatus(t *testing.T, db *store.DB, uuid, wantStatus string) *store.Order {
	t.Helper()
	order := RequireOrder(t, db, uuid)
	if order.Status != wantStatus {
		t.Fatalf("order %s: status = %q, want %q", uuid, order.Status, wantStatus)
	}
	return order
}

// RequireBin fetches a bin by ID and fails the test if not found.
func RequireBin(t *testing.T, db *store.DB, binID int64) *store.Bin {
	t.Helper()
	bin, err := db.GetBin(binID)
	if err != nil {
		t.Fatalf("get bin %d: %v", binID, err)
	}
	return bin
}

// AssertBinAtNode checks that a bin is located at the expected node.
func AssertBinAtNode(t *testing.T, db *store.DB, binID, wantNodeID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.NodeID == nil {
		t.Errorf("bin %d: node is nil, want %d", binID, wantNodeID)
	} else if *bin.NodeID != wantNodeID {
		t.Errorf("bin %d: node = %d, want %d", binID, *bin.NodeID, wantNodeID)
	}
}

// AssertBinUnclaimed checks that a bin has no active claim.
func AssertBinUnclaimed(t *testing.T, db *store.DB, binID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.ClaimedBy != nil {
		t.Errorf("bin %d: still claimed by order %d, want unclaimed", binID, *bin.ClaimedBy)
	}
}

// AssertBinClaimedBy checks that a bin is claimed by the expected order.
func AssertBinClaimedBy(t *testing.T, db *store.DB, binID, wantOrderID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.ClaimedBy == nil {
		t.Errorf("bin %d: not claimed, want claimed by order %d", binID, wantOrderID)
	} else if *bin.ClaimedBy != wantOrderID {
		t.Errorf("bin %d: claimed by %d, want %d", binID, *bin.ClaimedBy, wantOrderID)
	}
}
