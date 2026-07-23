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
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, part_number, qty, payload_code)
		VALUES
		  ('STATION-B', $1, '', 1, 'PART-2', 22, 'PAY-X'),
		  ('STATION-A', $1, '', 1, 'PART-1', 11, 'PAY-X'),
		  ('STATION-A', $2, '', 2, 'PART-3', 33, 'PAY-Y')`,
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
		byKey[r.Station+"|"+r.PartNumber] = r
	}
	r1, ok := byKey["STATION-A|PART-1"]
	if !ok {
		t.Fatalf("missing STATION-A/PART-1: %+v", rows)
	}
	if r1.Qty != 11 {
		t.Errorf("PART-1 qty = %d, want 11", r1.Qty)
	}
	if r1.NodeName != "BKT-NODE-A" {
		t.Errorf("PART-1 node = %q, want BKT-NODE-A", r1.NodeName)
	}
	if r1.StyleID != 1 {
		t.Errorf("PART-1 style_id = %d, want 1", r1.StyleID)
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

// TestListLinesideBuckets_MarksStranded pins the derived State: a bucket whose node
// runs an active style that no longer covers the bucket's payload is "stranded" (the
// "Core showed active on a stranded one" bug — State used to be hardcoded "active");
// a bucket the active style does consume stays "active".
func TestListLinesideBuckets_MarksStranded(t *testing.T) {
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

	if _, err := db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, part_number, qty, payload_code)
		VALUES
		  ('ST', $1, '', 1, 'PART-A', 100, 'PAY-ACTIVE'),
		  ('ST', $1, '', 2, 'PART-O', 250, 'PAY-OLD')`,
		node.Name); err != nil {
		t.Fatalf("seed buckets: %v", err)
	}

	rows, err := inventory.ListLinesideBuckets(db.DB)
	if err != nil {
		t.Fatalf("ListLinesideBuckets: %v", err)
	}
	state := map[string]string{}
	for _, r := range rows {
		state[r.PayloadCode] = r.State
	}
	if state["PAY-ACTIVE"] != "active" {
		t.Errorf("PAY-ACTIVE state = %q, want active (node's active style consumes it)", state["PAY-ACTIVE"])
	}
	if state["PAY-OLD"] != "stranded" {
		t.Errorf("PAY-OLD state = %q, want stranded (prior style, active style no longer covers it)", state["PAY-OLD"])
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
