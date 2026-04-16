package testdb

import (
	"testing"

	"shingocore/store"
)

// --- Fetch helpers (always fatal on miss) ---

// RequireOrder fetches an order by UUID and fatals if not found.
func RequireOrder(t *testing.T, db *store.DB, uuid string) *store.Order {
	t.Helper()
	order, err := db.GetOrderByUUID(uuid)
	if err != nil {
		t.Fatalf("get order %s: %v", uuid, err)
	}
	return order
}

// RequireBin fetches a bin by ID and fatals if not found.
func RequireBin(t *testing.T, db *store.DB, binID int64) *store.Bin {
	t.Helper()
	bin, err := db.GetBin(binID)
	if err != nil {
		t.Fatalf("get bin %d: %v", binID, err)
	}
	return bin
}

// --- Order status helpers ---

// RequireOrderStatus fetches an order and fatals if the status does not match.
// Use for preconditions where subsequent logic depends on the expected status.
func RequireOrderStatus(t *testing.T, db *store.DB, uuid, wantStatus string) *store.Order {
	t.Helper()
	order := RequireOrder(t, db, uuid)
	if order.Status != wantStatus {
		t.Fatalf("order %s: status = %q, want %q", uuid, order.Status, wantStatus)
	}
	return order
}

// AssertOrderStatus fetches an order and logs an error (non-fatal) if the status
// does not match. Use for end-of-test verification where you want to see all
// failures. Returns the order for further inspection (may be nil on fetch error).
func AssertOrderStatus(t *testing.T, db *store.DB, uuid, wantStatus string) *store.Order {
	t.Helper()
	order, err := db.GetOrderByUUID(uuid)
	if err != nil {
		t.Errorf("get order %s: %v", uuid, err)
		return nil
	}
	if order.Status != wantStatus {
		t.Errorf("order %s: status = %q, want %q", uuid, order.Status, wantStatus)
	}
	return order
}

// --- Bin location helpers ---

// RequireBinAtNode fetches a bin and fatals if it is not at the expected node.
func RequireBinAtNode(t *testing.T, db *store.DB, binID, wantNodeID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.NodeID == nil {
		t.Fatalf("bin %d: node is nil, want %d", binID, wantNodeID)
	} else if *bin.NodeID != wantNodeID {
		t.Fatalf("bin %d: node = %d, want %d", binID, *bin.NodeID, wantNodeID)
	}
}

// AssertBinAtNode fetches a bin and logs an error (non-fatal) if it is not at
// the expected node.
func AssertBinAtNode(t *testing.T, db *store.DB, binID, wantNodeID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.NodeID == nil {
		t.Errorf("bin %d: node is nil, want %d", binID, wantNodeID)
	} else if *bin.NodeID != wantNodeID {
		t.Errorf("bin %d: node = %d, want %d", binID, *bin.NodeID, wantNodeID)
	}
}

// --- Bin claim helpers ---

// RequireBinUnclaimed fetches a bin and fatals if it has an active claim.
func RequireBinUnclaimed(t *testing.T, db *store.DB, binID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.ClaimedBy != nil {
		t.Fatalf("bin %d: still claimed by order %d, want unclaimed", binID, *bin.ClaimedBy)
	}
}

// AssertBinUnclaimed fetches a bin and logs an error (non-fatal) if it has an
// active claim.
func AssertBinUnclaimed(t *testing.T, db *store.DB, binID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.ClaimedBy != nil {
		t.Errorf("bin %d: still claimed by order %d, want unclaimed", binID, *bin.ClaimedBy)
	}
}

// RequireBinClaimedBy fetches a bin and fatals if it is not claimed by the
// expected order.
func RequireBinClaimedBy(t *testing.T, db *store.DB, binID, wantOrderID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.ClaimedBy == nil {
		t.Fatalf("bin %d: not claimed, want claimed by order %d", binID, wantOrderID)
	} else if *bin.ClaimedBy != wantOrderID {
		t.Fatalf("bin %d: claimed by %d, want %d", binID, *bin.ClaimedBy, wantOrderID)
	}
}

// AssertBinClaimedBy fetches a bin and logs an error (non-fatal) if it is not
// claimed by the expected order.
func AssertBinClaimedBy(t *testing.T, db *store.DB, binID, wantOrderID int64) {
	t.Helper()
	bin := RequireBin(t, db, binID)
	if bin.ClaimedBy == nil {
		t.Errorf("bin %d: not claimed, want claimed by order %d", binID, wantOrderID)
	} else if *bin.ClaimedBy != wantOrderID {
		t.Errorf("bin %d: claimed by %d, want %d", binID, *bin.ClaimedBy, wantOrderID)
	}
}
