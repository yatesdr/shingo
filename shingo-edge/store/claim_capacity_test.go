package store

import (
	"sort"
	"strings"
	"testing"

	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// claim_capacity_test.go — a claim's UOP capacity is Core's number, read from
// the payload catalog, not a copy kept on the claim row.
//
// THE DRIFT THIS EXISTS FOR. `style_node_claims.uop_capacity` was filled from
// the catalog once, by the admin editor, at save time, and nothing re-copied
// it. When Core's capacity moved, the claim kept the old number until somebody
// happened to re-save the style. At Hopkinsville that had already happened to
// the style that was RUNNING: its two produce positions carried 10560 where
// the catalog said 2500 — a bin that physically holds 2500 rendered as 24%
// full forever, never stamped the "bin is full" demand edge, and recorded
// every operator swap as discretionary.
//
// THE DRIFT IS IN THE FIXTURE, which is why the pins below are a census of a
// whole plant rather than a hand-made stale pair: plant A carries that pull's
// two drifted rows and its twenty-three current ones, so resolution has to
// answer 2500 on exactly those two and leave every other one alone. The codes
// are the fixture's invented ones — the plant's are not in the repo.

// capacityCensus is one plant's claims as capacity now resolves them, against
// the number the claim row still stores.
type capacityCensus struct {
	agree  int // resolved == the stored copy — the copy was not stale
	differ []string
}

func capacityCensusOf(t *testing.T, plant string) capacityCensus {
	t.Helper()
	db := testDB(t)
	f := readPlantFixture(t, plant)
	loadPlantFixture(t, db, f)

	// The stored copy, straight off the row, before anything resolves it.
	stored := map[int64]int{}
	rows, err := db.Query(`SELECT id, uop_capacity FROM style_node_claims`)
	if err != nil {
		t.Fatalf("read stored capacities: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, cap int64
		if err := rows.Scan(&id, &cap); err != nil {
			t.Fatalf("scan stored capacity: %v", err)
		}
		stored[id] = int(cap)
	}

	var c capacityCensus
	for _, claim := range liveFixtureClaims(t, db) {
		want, ok := stored[claim.ID]
		if !ok {
			t.Fatalf("%s claim %d: no stored row", plant, claim.ID)
		}
		if claim.UOPCapacity == want {
			c.agree++
			continue
		}
		c.differ = append(c.differ, claim.CoreNodeName+" style "+itoa(int(claim.StyleID))+
			" payload "+claim.PayloadCode+": stored "+itoa(want)+", resolved "+itoa(claim.UOPCapacity))
	}
	sort.Strings(c.differ)
	return c
}

// TestClaimCapacity_ResolvesFromCatalogNotTheStoredCopy is the finding, pinned.
//
// Plant A: exactly two live claims resolve to something other than what they
// store, they are style 10's two produce positions, and the number they
// resolve to is the catalog's 2500. Plant B: none — every copy there was
// current, and resolution must not move a single one of them.
func TestClaimCapacity_ResolvesFromCatalogNotTheStoredCopy(t *testing.T) {
	a := capacityCensusOf(t, "a")
	want := []string{
		"PLN_01 style 10 payload SYN-A-P008: stored 10560, resolved 2500",
		"PLN_04 style 10 payload SYN-A-P009: stored 10560, resolved 2500",
	}
	if strings.Join(a.differ, "\n") != strings.Join(want, "\n") {
		t.Errorf("a: claims whose resolved capacity differs from the stored copy:\n got %v\nwant %v", a.differ, want)
	}
	if a.agree != 23 {
		t.Errorf("a: %d live claims resolve to the number they store, want 23", a.agree)
	}

	b := capacityCensusOf(t, "b")
	if len(b.differ) != 0 {
		t.Errorf("b: no copy there was stale, so nothing may move; got %v", b.differ)
	}
	if b.agree != 30 {
		t.Errorf("b: %d live claims resolve to the number they store, want 30", b.agree)
	}
}

// TestClaimCapacity_FreshCellResolvesWithoutAPriorClaim — a cell composed with
// no prior claim to copy from still gets its payload's capacity. Under the old
// scheme the number came off the prior row, so a first save wrote 0 unless the
// editor happened to fill it in.
func TestClaimCapacity_FreshCellResolvesWithoutAPriorClaim(t *testing.T) {
	db := testDB(t)
	seedCapacityProcess(t, db)
	if err := catalog.UpsertCatalog(db.DB, &catalog.CatalogEntry{
		ID: 1, Name: "Part A", Code: "PART-A", UOPCapacity: 1320,
	}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	id, err := processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID:             1,
		CoreNodeName:        "PLN_01",
		Role:                "produce",
		SwapMode:            "two_robot",
		PayloadCode:         "PART-A",
		InboundSource:       "SMN_001",
		InboundStaging:      "STG_001",
		OutboundDestination: "SMN_001",
		PairedCoreNode:      "PLN_02",
		Source:              "hmi",
	})
	if err != nil {
		t.Fatalf("upsert fresh claim: %v", err)
	}
	claim, err := processes.GetClaim(db.DB, id)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if claim.UOPCapacity != 1320 {
		t.Errorf("fresh claim resolved capacity = %d, want the catalog's 1320", claim.UOPCapacity)
	}
}

// TestClaimCapacity_UnknownPayloadResolvesToZero — a payload the catalog does
// not know resolves to 0. Not to a guess, and not to whatever the dead column
// happens to hold: 0 is what every reader already treats as "no capacity
// known" (evaluateProduceLevel skips the node, the board prints no
// denominator), and a guessed number would silently drive a demand edge.
func TestClaimCapacity_UnknownPayloadResolvesToZero(t *testing.T) {
	db := testDB(t)
	seedCapacityProcess(t, db)

	id, err := processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID:             1,
		CoreNodeName:        "PLN_01",
		Role:                "produce",
		SwapMode:            "two_robot",
		PayloadCode:         "NOT-IN-CATALOG",
		InboundSource:       "SMN_001",
		InboundStaging:      "STG_001",
		OutboundDestination: "SMN_001",
		PairedCoreNode:      "PLN_02",
		Source:              "hmi",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	// Write a non-zero number into the dead column directly: if resolution
	// ever falls back to the stored copy, this is the value that would show up.
	if _, err := db.Exec(`UPDATE style_node_claims SET uop_capacity = 9999 WHERE id = ?`, id); err != nil {
		t.Fatalf("plant a stale copy: %v", err)
	}
	claim, err := processes.GetClaim(db.DB, id)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if claim.UOPCapacity != 0 {
		t.Errorf("unknown payload resolved to %d, want 0 — the stored copy must not be a fallback", claim.UOPCapacity)
	}
}

// TestClaimCapacity_StoredColumnIsNoLongerWritten — the column stays in the
// table (dropping it is its own change, with its own rebuild risk) but nothing
// writes it any more, so it reads as its DDL default on every row the code
// creates. A write reappearing here is the drift mechanism coming back.
func TestClaimCapacity_StoredColumnIsNoLongerWritten(t *testing.T) {
	db := testDB(t)
	seedCapacityProcess(t, db)
	if err := catalog.UpsertCatalog(db.DB, &catalog.CatalogEntry{
		ID: 1, Name: "Part A", Code: "PART-A", UOPCapacity: 1320,
	}); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	// UOPCapacity is set deliberately: the input still carries the field (the
	// admin API's shape is unchanged) and the point is that the store ignores
	// it rather than persisting it.
	in := processes.NodeClaimInput{
		StyleID:             1,
		CoreNodeName:        "PLN_01",
		Role:                "produce",
		SwapMode:            "two_robot",
		PayloadCode:         "PART-A",
		UOPCapacity:         4321,
		InboundSource:       "SMN_001",
		InboundStaging:      "STG_001",
		OutboundDestination: "SMN_001",
		PairedCoreNode:      "PLN_02",
		Source:              "hmi",
	}
	id, err := processes.UpsertClaim(db.DB, in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	assertStoredCapacityZero(t, db, id, "after INSERT")

	// And on the UPDATE arm, which is a different column list.
	if _, err := processes.UpsertClaim(db.DB, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	assertStoredCapacityZero(t, db, id, "after UPDATE")
}

func assertStoredCapacityZero(t *testing.T, db *DB, id int64, when string) {
	t.Helper()
	var stored int
	if err := db.QueryRow(`SELECT uop_capacity FROM style_node_claims WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read stored capacity %s: %v", when, err)
	}
	if stored != 0 {
		t.Errorf("%s the dead uop_capacity column holds %d, want 0 — something is still writing it", when, stored)
	}
}

// seedCapacityProcess makes the smallest process a claim can hang off.
func seedCapacityProcess(t *testing.T, db *DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO processes (id, name, description, production_state) VALUES (1, 'P1', '', 'running')`,
		`INSERT INTO styles (id, name, description, process_id) VALUES (1, 'S1', '', 1)`,
		`INSERT INTO process_nodes (id, process_id, core_node_name, code, name, sequence, enabled)
		   VALUES (1, 1, 'PLN_01', 'pln01', 'PLN 01', 1, 1)`,
		`INSERT INTO process_nodes (id, process_id, core_node_name, code, name, sequence, enabled)
		   VALUES (2, 1, 'PLN_02', 'pln02', 'PLN 02', 2, 1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}
