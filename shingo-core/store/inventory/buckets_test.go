//go:build docker

package inventory_test

import (
	"strings"
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
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station_uid, core_node_name, payload_code, state, qty)
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
		byKey[r.StationUID+"|"+r.PayloadCode] = r
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
	if rows[0].StationUID != "STATION-A" {
		t.Errorf("rows[0].Station = %q, want STATION-A (cell→station→node sort)", rows[0].StationUID)
	}
	if rows[2].StationUID != "STATION-B" {
		t.Errorf("rows[2].Station = %q, want STATION-B (cell→station→node sort)", rows[2].StationUID)
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

	if _, err := db.Exec(`INSERT INTO lineside_buckets (station_uid, core_node_name, payload_code, state, qty)
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

// TestLinesideBuckets_ThePileIsKeyedByItsSeatAndOneOfTwoStates: a pile row is
// active or stranded and nothing else (lineside_buckets_state_check), and one
// seat holds at most one row per state (lineside_buckets_node_payload_state_key,
// the conflict target of the level wire's upsert in uop/applier.go). An active
// and a stranded row at one seat both land, so each refusal is its own
// constraint's. It is the runtime half of store's deleted TestV131_PileMirrorShape;
// the columns that test found gone are the committed snapshot's to show.
func TestLinesideBuckets_ThePileIsKeyedByItsSeatAndOneOfTwoStates(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	insert := func(state string) error {
		_, err := db.Exec(`INSERT INTO lineside_buckets (station_uid, core_node_name, payload_code, state, qty)
			VALUES ('ST', 'BKT-KEY-NODE', 'PAY-KEY', $1, 1)`, state)
		return err
	}
	if err := insert("inactive"); err == nil || !strings.Contains(err.Error(), "lineside_buckets_state_check") {
		t.Errorf("state inactive: %v, want the state CHECK's refusal", err)
	}
	for _, state := range []string{"active", "stranded"} {
		if err := insert(state); err != nil {
			t.Fatalf("state %s at a fresh seat: %v", state, err)
		}
	}
	if err := insert("active"); err == nil || !strings.Contains(err.Error(), "lineside_buckets_node_payload_state_key") {
		t.Errorf("a second active row at one seat: %v, want the seat key's refusal", err)
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

// TestCountHolders: the bins and active piles of a payload whose count is not
// 0, bins first, with the totals; a bin at 0, a stranded pile and another
// payload's rows are not counted. The payload guards read it.
func TestCountHolders(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	for _, q := range []string{
		`INSERT INTO payloads (code, uop_capacity) VALUES ('HOLD-A', 800), ('HOLD-B', 800)`,
		`INSERT INTO bin_types (code) VALUES ('HOLD-BT')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	for _, b := range []struct {
		label, code string
		uop         int
	}{{"HB-2", "HOLD-A", -5}, {"HB-1", "HOLD-A", 800}, {"HB-3", "HOLD-A", 0}, {"HB-4", "HOLD-B", 9}} {
		if _, err := db.Exec(`INSERT INTO bins (bin_type_id, label, payload_code, uop_remaining, status)
			SELECT id, $1, $2, $3, 'available' FROM bin_types WHERE code = 'HOLD-BT'`, b.label, b.code, b.uop); err != nil {
			t.Fatalf("seed bin %s: %v", b.label, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO lineside_buckets (station_uid, core_node_name, payload_code, state, qty) VALUES
		('STN', 'ALN_1', 'HOLD-A', 'active', 24), ('STN', 'ALN_2', 'HOLD-A', 'stranded', 7), ('STN', 'ALN_3', 'HOLD-B', 'active', 3)`); err != nil {
		t.Fatalf("seed piles: %v", err)
	}
	holders, bins, piles, err := inventory.CountHolders(db.DB, "HOLD-A", 10)
	if err != nil {
		t.Fatalf("CountHolders: %v", err)
	}
	want := []inventory.Holder{{What: "HB-1", Count: 800}, {What: "HB-2", Count: -5}, {What: "STN · ALN_1", Count: 24, Pile: true}}
	if bins != 2 || piles != 1 || len(holders) != len(want) {
		t.Fatalf("CountHolders(HOLD-A) = %+v, %d bins, %d piles; want %+v, 2, 1", holders, bins, piles, want)
	}
	for i := range want {
		if holders[i] != want[i] {
			t.Errorf("holder %d = %+v, want %+v", i, holders[i], want[i])
		}
	}
	if h, b, p, err := inventory.CountHolders(db.DB, "HOLD-A", 1); err != nil || b != 2 || p != 1 || len(h) != 2 {
		t.Errorf("limit 1: %+v, %d, %d, %v; want one bin, one pile, totals 2 and 1", h, b, p, err)
	}
}
