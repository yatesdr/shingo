//go:build docker

package inventory_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/inventory"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// TestListLinesideBuckets_ReturnsAllRowsOrdered pins the Issue 2
// listing query: every row in lineside_buckets surfaces with the
// joined node/cell/lane context, and the result set is ordered by
// cell → station → node → part so the operator-facing inventory page
// can render the rows without re-sorting on the client.
//
// See lineside-buckets-investigation-2026-05-18.md.
func TestListLinesideBuckets_ReturnsAllRowsOrdered(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	nodeA := &nodes.Node{Name: "BKT-NODE-A", Zone: "Z1", Enabled: true}
	if err := nodes.Create(db.DB, nodeA); err != nil {
		t.Fatalf("create node A: %v", err)
	}
	nodeB := &nodes.Node{Name: "BKT-NODE-B", Zone: "Z1", Enabled: true}
	if err := nodes.Create(db.DB, nodeB); err != nil {
		t.Fatalf("create node B: %v", err)
	}

	// Seed three buckets across two stations.
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES
		  ('STATION-B', $1, 'PAY-2', 'active', 22),
		  ('STATION-A', $1, 'PAY-1', 'active', 11),
		  ('STATION-A', $2, 'PAY-3', 'active', 33)`,
		nodeA.Name, nodeB.Name); err != nil {
		t.Fatalf("seed buckets: %v", err)
	}

	rows, err := inventory.ListLinesideBuckets(db.DB)
	if err != nil {
		t.Fatalf("ListLinesideBuckets: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("row count = %d, want 3; rows=%+v", len(rows), rows)
	}

	// Pin field mapping on the first row by station name.
	byKey := map[string]inventory.BucketRow{}
	for _, r := range rows {
		byKey[r.Station+"|"+r.PayloadCode] = r
	}
	r1, ok := byKey["STATION-A|PAY-1"]
	if !ok {
		t.Fatalf("missing STATION-A/PAY-1: %+v", rows)
	}
	if r1.Qty != 11 {
		t.Errorf("PART-1 qty = %d, want 11", r1.Qty)
	}
	if r1.NodeName != "BKT-NODE-A" {
		t.Errorf("PART-1 node = %q, want BKT-NODE-A", r1.NodeName)
	}
	if r1.State != "active" {
		t.Errorf("PART-1 state = %q, want active", r1.State)
	}

	// Ordering: cell → station → node → part. With nodes that have
	// no parent hierarchy the cell is empty, so the sort falls
	// through to station first. STATION-A rows come before STATION-B.
	if rows[0].Station != "STATION-A" {
		t.Errorf("rows[0].Station = %q, want STATION-A (cell→station→node sort)", rows[0].Station)
	}
	if rows[2].Station != "STATION-B" {
		t.Errorf("rows[2].Station = %q, want STATION-B (cell→station→node sort)", rows[2].Station)
	}
}

// TestListLinesideBuckets_StateIsTheStoredColumn pins the listing's State:
// the pile's stored state, as the Edge's level set it.
//
// This was TestListLinesideBuckets_MarksStranded, which pinned State derived
// from the plant-claims mirror (a pile whose node's active style no longer
// covered its payload read "stranded"). FLIPPED BY BRIEF v7 EXPECTED CHANGE
// #3: stranded is the state set at cutover, so the claims seeded below no
// longer enter into it: an active row of a part the style does not claim lists
// as active, and the stranded row lists as stranded beside it.
func TestListLinesideBuckets_StateIsTheStoredColumn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "BKT-STRAND-NODE", Zone: "Z1", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	// Mirror: the node runs an active style consuming PAY-ACTIVE, not PAY-OLD.
	if err := plantclaims.ReplaceProcess(db.DB, "PROC-B",
		[]plantclaims.StyleRow{{ProcessID: "PROC-B", StyleID: "STYLE-ACTIVE", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{
			ProcessID:           "PROC-B",
			StyleID:             "STYLE-ACTIVE",
			CoreNodeName:        node.Name,
			Role:                protocol.ClaimRoleConsume,
			PayloadCode:         "PAY-ACTIVE",
			AllowedPayloadCodes: []string{"PAY-ACTIVE"},
		}}, 0,
	); err != nil {
		t.Fatalf("seed plant claims: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES
		  ('ST', $1, 'PAY-ACTIVE', 'active', 100),
		  ('ST', $1, 'PAY-OLD', 'active', 250),
		  ('ST', $1, 'PAY-OLD', 'stranded', 40)`,
		node.Name); err != nil {
		t.Fatalf("seed buckets: %v", err)
	}

	rows, err := inventory.ListLinesideBuckets(db.DB)
	if err != nil {
		t.Fatalf("ListLinesideBuckets: %v", err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.PayloadCode+"|"+r.State] = r.Qty
	}
	want := map[string]int{"PAY-ACTIVE|active": 100, "PAY-OLD|active": 250, "PAY-OLD|stranded": 40}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for k, q := range want {
		if got[k] != q {
			t.Errorf("%s = %d, want %d", k, got[k], q)
		}
	}
}

// TestListLinesideBuckets_Empty pins the empty-table response: zero
// rows, no error.
func TestListLinesideBuckets_Empty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	rows, err := inventory.ListLinesideBuckets(db.DB)
	if err != nil {
		t.Fatalf("ListLinesideBuckets: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty DB len = %d, want 0", len(rows))
	}
}
