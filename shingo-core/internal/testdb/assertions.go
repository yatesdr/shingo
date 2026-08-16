package testdb

import (
	"testing"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// --- Fetch helpers (always fatal on miss) ---

// RequireOrder fetches an order by UUID and fatals if not found.
func RequireOrder(t *testing.T, db *store.DB, uuid string) *orders.Order {
	t.Helper()
	order, err := db.GetOrderByUUID(uuid)
	if err != nil {
		t.Fatalf("get order %s: %v", uuid, err)
	}
	return order
}

// RequireBin fetches a bin by ID and fatals if not found.
func RequireBin(t *testing.T, db *store.DB, binID int64) *bins.Bin {
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
func RequireOrderStatus(t *testing.T, db *store.DB, uuid string, wantStatus protocol.Status) *orders.Order {
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
func AssertOrderStatus(t *testing.T, db *store.DB, uuid string, wantStatus protocol.Status) *orders.Order {
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

// --- Ledger sweep ---

// AssertNoOrphanedHolds sweeps the WHOLE hold ledger for a hold whose owner is
// dead: every reservation kind (bin, slot, mouth, occupancy — the last two
// carrying the lane seam, and a mouth row with mode='dig' IS the lane lock),
// plus the two hard-claim columns, bins.claimed_by and nodes.claimed_by.
//
// ── WHY A SWEEP AND NOT A COUNT ───────────────────────────────────────────
//
// The per-resource assertions above answer "is THIS bin free". They are the
// right shape when a test knows which resource it is about, and the wrong shape
// for the question the ledger's promise actually makes, which is over the whole
// table: NOTHING is held by an order that is finished. A test that counts rows
// for one bin id passes while a mouth row, an occupancy row and a dig lock from
// the same order sit behind it — and those are exactly the kinds the lane work
// added, on the paths where an order holds several resources at once.
//
// ── THE PREDICATE IS THE REAPER'S ─────────────────────────────────────────
//
// Orphaned means the OWNER is terminal or gone, which is the same test
// reservations.ReapOrphaned applies (store/reservations). Deliberately not "the
// table is empty": a hold under a live order is sacred no matter how long it has
// been held — an order in sourcing legitimately waits hours for its source — so
// a sweep that demanded an empty ledger would fail every scenario that ends with
// work still in flight, and would therefore be wired into none of them.
//
// Sharing the reaper's predicate also makes the assertion say something
// operationally exact: the reaper would find nothing to do. On the normal path
// TerminalizeOrder has already released everything in the same transaction as
// the status write, so the backstop should always be looking at an empty set. A
// failure here is either a release the chokepoint missed or a hold written
// outside it.
//
// Safe to call from any package's docker tests: testdb.Open gives every test its
// own database, so the sweep sees this test's rows and nothing else.
func AssertNoOrphanedHolds(t *testing.T, db *store.DB) {
	t.Helper()
	dead := "(o.id IS NULL OR o.status IN (" + protocol.TerminalStatusSQLList() + "))"

	rows, err := db.DB.Query(`
		SELECT r.resource_kind, COALESCE(r.mode, ''), r.order_id,
		       COALESCE(r.bin_id, r.node_id, 0), COALESCE(o.status, '(gone)')
		  FROM reservations r
		  LEFT JOIN orders o ON o.id = r.order_id
		 WHERE ` + dead + `
		 ORDER BY r.id`)
	if err != nil {
		t.Fatalf("AssertNoOrphanedHolds: scan reservations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, mode, status string
		var orderID, resourceID int64
		if err := rows.Scan(&kind, &mode, &orderID, &resourceID, &status); err != nil {
			t.Fatalf("AssertNoOrphanedHolds: scan reservation row: %v", err)
		}
		what := kind
		if mode != "" {
			what += "/" + mode
		}
		t.Errorf("orphaned %s reservation on resource %d, held by order %d (%s) — a hold under a "+
			"dead owner is invisible to every live order and is only ever reclaimed by the "+
			"owner-liveness reaper, which is the backstop and not the mechanism",
			what, resourceID, orderID, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertNoOrphanedHolds: reservations: %v", err)
	}

	assertNoOrphanedClaims(t, db, "bins", dead)
	assertNoOrphanedClaims(t, db, "nodes", dead)
}

// assertNoOrphanedClaims is the hard-claim half, over one of the two tables that
// carry a claimed_by column. Both are swept because they leak differently: a
// stranded bins.claimed_by hides a bin from every finder, and a stranded
// nodes.claimed_by makes a slot look taken to every placer.
func assertNoOrphanedClaims(t *testing.T, db *store.DB, table, dead string) {
	t.Helper()
	rows, err := db.DB.Query(`
		SELECT x.id, x.claimed_by, COALESCE(o.status, '(gone)')
		  FROM ` + table + ` x
		  LEFT JOIN orders o ON o.id = x.claimed_by
		 WHERE x.claimed_by IS NOT NULL AND ` + dead + `
		 ORDER BY x.id`)
	if err != nil {
		t.Fatalf("AssertNoOrphanedHolds: scan %s claims: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner int64
		var status string
		if err := rows.Scan(&id, &owner, &status); err != nil {
			t.Fatalf("AssertNoOrphanedHolds: scan %s row: %v", table, err)
		}
		t.Errorf("orphaned hard claim: %s %d is still claimed_by order %d (%s)", table, id, owner, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("AssertNoOrphanedHolds: %s: %v", table, err)
	}
}
