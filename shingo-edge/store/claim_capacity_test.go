package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
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

	// The copy the claim row used to store (Edge v60 dropped the column), off
	// the fixture's rows.
	stored := map[int64]int{}
	for _, row := range f.StyleNodeClaims {
		stored[fixtureInt(row["id"])] = int(fixtureInt(row["uop_capacity"]))
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

	id, err := processes.UpsertClaim(db.DB, domain.CoreNodeKinds{}, processes.NodeClaimInput{
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

	id, err := processes.UpsertClaim(db.DB, domain.CoreNodeKinds{}, processes.NodeClaimInput{
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
	claim, err := processes.GetClaim(db.DB, id)
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if claim.UOPCapacity != 0 {
		t.Errorf("unknown payload resolved to %d, want 0 — nothing but the catalog may answer", claim.UOPCapacity)
	}
}

// TestClaimPiecesPerCycle_ResolvesFromTheCatalogManifest — a claim's pieces per
// cycle is its part's one manifest line's ratio, read from the catalog copy on
// the claim read (every claim read shares claimSelect, the tick's included).
// Anything but exactly one line is 1: a kit keeps one per stroke, and a part
// with no lines or no catalog row has no ratio. A ratio below 1, which Core
// refuses to store, reads 1 rather than a draw of nothing.
func TestClaimPiecesPerCycle_ResolvesFromTheCatalogManifest(t *testing.T) {
	db := testDB(t)
	seedCapacityProcess(t, db)
	id, err := processes.UpsertClaim(db.DB, domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: 1, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot", PayloadCode: "SYN-PPC",
		InboundSource: "SMN_001", InboundStaging: "STG_001", OutboundDestination: "SMN_001", PairedCoreNode: "PLN_02",
		Source: "hmi",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	line := func(part string, ratio int64) protocol.ManifestItem {
		return protocol.ManifestItem{PartNumber: part, PartsPerCycle: ratio}
	}
	for _, c := range []struct {
		name     string
		manifest []protocol.ManifestItem
		noRow    bool
		want     int
	}{
		{name: "one line at 8", manifest: []protocol.ManifestItem{line("SYN-PPC", 8)}, want: 8},
		{name: "one line at 1", manifest: []protocol.ManifestItem{line("SYN-PPC", 1)}, want: 1},
		{name: "a kit of two lines", manifest: []protocol.ManifestItem{line("SYN-PPC-L", 2), line("SYN-PPC-R", 3)}, want: 1},
		{name: "a row with no lines", want: 1},
		{name: "one line at 0", manifest: []protocol.ManifestItem{line("SYN-PPC", 0)}, want: 1},
		{name: "no catalog row", noRow: true, want: 1},
	} {
		if _, err := db.Exec(`DELETE FROM payload_catalog`); err != nil {
			t.Fatalf("%s: clear catalog: %v", c.name, err)
		}
		if !c.noRow {
			if err := catalog.UpsertCatalog(db.DB, &catalog.CatalogEntry{
				ID: 1, Name: "SYN-PPC", Code: "SYN-PPC", UOPCapacity: 800, Manifest: c.manifest,
			}); err != nil {
				t.Fatalf("%s: seed catalog: %v", c.name, err)
			}
		}
		claim, err := processes.GetClaim(db.DB, id)
		if err != nil {
			t.Fatalf("%s: get claim: %v", c.name, err)
		}
		if claim.PiecesPerCycle != c.want {
			t.Errorf("%s: pieces per cycle = %d, want %d", c.name, claim.PiecesPerCycle, c.want)
		}
	}
}

// TestClaimPiecesPerCycle_AgreesWithTheCountExamples — the claim read's SQL
// copy of the ratio rule (capacity.PiecesPerCycleSQL, which cannot call Go)
// answers protocol.UOPRatio for every worked example of
// protocol/testdata/count-examples.json, the file the Go and browser copies
// are pinned by.
func TestClaimPiecesPerCycle_AgreesWithTheCountExamples(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "protocol", "testdata", "count-examples.json"))
	if err != nil {
		t.Fatalf("read the worked examples: %v", err)
	}
	var f struct {
		Examples []struct {
			Name  string                  `json:"name"`
			Lines []protocol.ManifestItem `json:"lines"`
			Ratio int                     `json:"ratio"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Examples) == 0 {
		t.Fatalf("parse the worked examples (%d read): %v", len(f.Examples), err)
	}
	db := testDB(t)
	seedCapacityProcess(t, db)
	id, err := processes.UpsertClaim(db.DB, domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: 1, CoreNodeName: "PLN_01", Role: "consume", SwapMode: "two_robot", PayloadCode: "SYN-CNT",
		InboundSource: "SMN_001", InboundStaging: "STG_001", OutboundDestination: "SMN_001", PairedCoreNode: "PLN_02",
		Source: "hmi",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	for _, ex := range f.Examples {
		if _, err := db.Exec(`DELETE FROM payload_catalog`); err != nil {
			t.Fatalf("%s: clear catalog: %v", ex.Name, err)
		}
		if err := catalog.UpsertCatalog(db.DB, &catalog.CatalogEntry{
			ID: 1, Name: "SYN-CNT", Code: "SYN-CNT", UOPCapacity: 800, Manifest: ex.Lines,
		}); err != nil {
			t.Fatalf("%s: seed catalog: %v", ex.Name, err)
		}
		claim, err := processes.GetClaim(db.DB, id)
		if err != nil {
			t.Fatalf("%s: get claim: %v", ex.Name, err)
		}
		if want := protocol.UOPRatio(ex.Lines); claim.PiecesPerCycle != want || want != ex.Ratio {
			t.Errorf("%s: the claim read gives %d, protocol.UOPRatio %d, the example %d", ex.Name, claim.PiecesPerCycle, want, ex.Ratio)
		}
	}
}

// seedCapacityProcess makes the smallest process a claim can hang off.
func seedCapacityProcess(t *testing.T, db *DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO processes (id, name, description) VALUES (1, 'P1', '')`,
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
