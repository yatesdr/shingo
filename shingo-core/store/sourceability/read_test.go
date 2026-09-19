//go:build docker

package sourceability_test

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// This proves BuildInputs' SQL against a real DB: the available-bin pool follows
// the FindSourceFIFO predicate (claimed and reserved bins drop out), and
// Compute's verdict flows from it. Pure-computation edge cases live in the
// no-DB fixtures (sourceability_test.go).

func stateFor(t *testing.T, states []sourceability.StyleState, process, style string) sourceability.StyleState {
	t.Helper()
	for _, s := range states {
		if s.ProcessID == process && s.StyleID == style {
			return s
		}
	}
	t.Fatalf("no state for %s/%s in %+v", process, style, states)
	return sourceability.StyleState{}
}

func TestBuildInputsAndCompute_PoolPredicate(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB

	std := testdb.SetupStandardData(t, sdb)
	bin := testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "src-a")

	// Mirror: SNF2 with style A (needs BIN-A, available) and style Z (needs
	// BIN-Z, which no bin holds).
	styles := []plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A"}, {ProcessID: "SNF2", StyleID: "Z"}}
	claims := []plantclaims.ClaimRow{
		{ProcessID: "SNF2", StyleID: "A", CoreNodeName: std.StorageNode.Name, PayloadCode: "BIN-A"},
		{ProcessID: "SNF2", StyleID: "Z", CoreNodeName: std.StorageNode.Name, PayloadCode: "BIN-Z"},
	}
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db, "SNF2", styles, claims, 0), "seed mirror")

	compute := func() []sourceability.StyleState {
		in, err := sourceability.BuildInputs(db, time.Hour)
		testutil.MustNoErr(t, err, "build inputs")
		return sourceability.Compute(in, sourceability.Config{}, time.Now())
	}

	// Available bin present → A green, Z red (missing BIN-Z).
	states := compute()
	if got := stateFor(t, states, "SNF2", "A"); got.Status != sourceability.StatusGreen {
		t.Errorf("A status = %q, want green", got.Status)
	}
	z := stateFor(t, states, "SNF2", "Z")
	if z.Status != sourceability.StatusRed || len(z.Missing) != 1 || z.Missing[0] != "BIN-Z" {
		t.Errorf("Z = %+v, want red missing [BIN-Z]", z)
	}

	// Claim the bin → it leaves the pool → A goes red missing BIN-A.
	if _, err := db.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	if got := stateFor(t, compute(), "SNF2", "A"); got.Status != sourceability.StatusRed {
		t.Errorf("A after claim = %q, want red (claimed bin not sourceable)", got.Status)
	}

	// Unclaim, then place a PENDING reservation → still out of the pool.
	if _, err := db.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("unclaim bin: %v", err)
	}
	if got := stateFor(t, compute(), "SNF2", "A"); got.Status != sourceability.StatusGreen {
		t.Fatalf("A after unclaim = %q, want green again", got.Status)
	}
	var orderID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity)
		 VALUES ('res-1','edge.line1','retrieve','pending',1) RETURNING id`).Scan(&orderID), "create order")
	if _, err := db.Exec(
		`INSERT INTO reservations (order_id, bin_id, state, expires_at)
		 VALUES ($1, $2, 'pending', NOW() + INTERVAL '1 hour')`, orderID, bin.ID); err != nil {
		t.Fatalf("reserve bin: %v", err)
	}
	if got := stateFor(t, compute(), "SNF2", "A"); got.Status != sourceability.StatusRed {
		t.Errorf("A after pending reservation = %q, want red (reserved bin not sourceable)", got.Status)
	}
}

// TestPoolBreakdownByPayload_ByLocation proves the page's "Free at" column data:
// FREE bins are broken down by their real physical node, most-first, and Free is
// the sum. Before this the column rendered the CLAIM node (where material feeds),
// which is not where an operator goes to find a bin.
func TestPoolBreakdownByPayload_ByLocation(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	std := testdb.SetupStandardData(t, sdb)

	// Three free BIN-A: two at the storage node, one at the line node. Plus one
	// held (claimed) at storage, which must count toward Held and never appear
	// as a free LOCATION.
	testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "free-1")
	testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "free-2")
	testdb.CreateBinAtNode(t, sdb, "BIN-A", std.LineNode.ID, "free-3")
	held := testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "held-1")
	if _, err := sdb.DB.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, held.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	pool, err := sourceability.PoolBreakdownByPayload(sdb.DB)
	testutil.MustNoErr(t, err, "pool breakdown")

	pb := pool["BIN-A"]
	if pb.Free != 3 {
		t.Errorf("Free = %d, want 3", pb.Free)
	}
	if pb.Held != 1 {
		t.Errorf("Held = %d, want 1 (the claimed bin)", pb.Held)
	}

	// Most-first: the storage node (2) before the line node (1). The held-only
	// nothing-free case would not appear at all; here every node has a free bin.
	if len(pb.FreeByNode) != 2 {
		t.Fatalf("FreeByNode = %+v, want two nodes", pb.FreeByNode)
	}
	if pb.FreeByNode[0].Node != std.StorageNode.Name || pb.FreeByNode[0].Count != 2 {
		t.Errorf("first location = %+v, want %s ×2", pb.FreeByNode[0], std.StorageNode.Name)
	}
	if pb.FreeByNode[1].Node != std.LineNode.Name || pb.FreeByNode[1].Count != 1 {
		t.Errorf("second location = %+v, want %s ×1", pb.FreeByNode[1], std.LineNode.Name)
	}

	// The sum invariant the page relies on: Free equals the location counts.
	sum := 0
	for _, nc := range pb.FreeByNode {
		sum += nc.Count
	}
	if sum != pb.Free {
		t.Errorf("location counts sum to %d, but Free = %d", sum, pb.Free)
	}
}

// TestBuildInputs_CarriesTheCarrierRuleFindingBesideThePool.
//
// A bin in a carrier its payload is not declared to travel in is refused by
// BinSourceableSQL, so it contributes nothing to Pool and the style goes RED —
// beside an inventory page saying the parts are there. "No stock" and "stock in
// the wrong carrier" have different actions, so BuildInputs carries the second
// count and Compute hands it to the operator sentence.
//
// IT RIDES THE POOL QUERY. This runs on every recompute; a second round trip
// for a number that is zero on a good day would be a per-tick cost. The pool
// term became a FILTER over the same rows and must answer identically, which is
// what the green half of this test is for.
//
// THE SPARSE ARM IS PINNED QUIET. payload_bin_types has rows for one payload of
// 128 at Springfield; a payload with none constrains nothing and is never
// counted.
func TestBuildInputs_CarriesTheCarrierRuleFindingBesideThePool(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB
	std := testdb.SetupStandardData(t, sdb)

	// BIN-A is sourceable and unflagged; BIN-F is flagged (so out of the pool);
	// BIN-S carries a payload nobody has described and is neither.
	testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "src-a")
	flagged := testdb.CreateBinAtNode(t, sdb, "BIN-F", std.StorageNode.ID, "src-f")
	testdb.CreateBinAtNode(t, sdb, "BIN-S", std.StorageNode.ID, "src-s")
	if _, err := db.Exec(
		`UPDATE bins SET undeclared_carrier_at = NOW() WHERE id = $1`, flagged.ID); err != nil {
		t.Fatalf("stamp the finding: %v", err)
	}

	in, err := sourceability.BuildInputs(db, time.Hour)
	testutil.MustNoErr(t, err, "build inputs")

	// THE POOL IS UNCHANGED BY THE SECOND AGGREGATE. Every one of these bins is
	// ordinarily sourceable, so each contributes 1 — including the flagged one,
	// because the STAMP gates nothing: the bin-type arm in BinSourceableSQL is
	// what refuses it, and this fixture stamps the column without breaking the
	// rule. That separation is deliberate and is the point of the assertion.
	for _, p := range []string{"BIN-A", "BIN-F", "BIN-S"} {
		if in.Pool[p] != 1 {
			t.Errorf("Pool[%s] = %d, want 1 — the pool term moved from a WHERE to a "+
				"FILTER over the same rows and must answer identically", p, in.Pool[p])
		}
	}

	if in.UndeclaredCarrier["BIN-F"] != 1 {
		t.Errorf("UndeclaredCarrier[BIN-F] = %d, want 1", in.UndeclaredCarrier["BIN-F"])
	}
	// ABSENT, NOT ZERO. Compute uses presence to decide whether the operator
	// sentence has a second half at all, and "0 bins of BIN-A in undeclared
	// carriers" is a sentence that would be printed to somebody.
	for _, p := range []string{"BIN-A", "BIN-S"} {
		if _, ok := in.UndeclaredCarrier[p]; ok {
			t.Errorf("UndeclaredCarrier holds %s with no findings; a payload with none "+
				"must be absent from the map, not present with a zero", p)
		}
	}
}
