package store_test

import (
	"slices"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// process_payloads_test.go — the part set's rules.
//
// THE PART SET IS AN OFFER LIST, and the two rules that matter are about what
// the offer list IS, not about what may be written to it: it is the union of
// the stored rows and what the process's live claims already name, and a
// replace is a replace — a name taken out stops being offered.
//
// A UNION AND NOT A BACKFILL (SYNTH §2). Only one half is writable, so it is
// not two sources of truth. A backfill would have repeated the routing set's
// "21 rows nobody switched on": rows landing disabled that an engineer then
// has to adopt one at a time, forever.

func TestProcessPayloads_ReplaceIsASetTo(t *testing.T) {
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")

	if err := db.ReplaceProcessPayloads(seeded.ProcessID, []string{"PART-A", "PART-B"}); err != nil {
		t.Fatalf("ReplaceProcessPayloads: %v", err)
	}
	got, err := db.ListProcessPayloads(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ListProcessPayloads: %v", err)
	}
	if len(got) != 2 || got[0] != "PART-A" || got[1] != "PART-B" {
		t.Fatalf("stored rows = %v, want [PART-A PART-B] in code order", got)
	}

	// A name taken out of the list stops being a stored row. This is the half
	// an engineer controls; the claimed half below is not theirs to remove.
	if err := db.ReplaceProcessPayloads(seeded.ProcessID, []string{"PART-B", "PART-C"}); err != nil {
		t.Fatalf("second ReplaceProcessPayloads: %v", err)
	}
	got, err = db.ListProcessPayloads(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ListProcessPayloads: %v", err)
	}
	if len(got) != 2 || got[0] != "PART-B" || got[1] != "PART-C" {
		t.Fatalf("after replace rows = %v, want [PART-B PART-C]", got)
	}
}

// BLANKS AND DUPLICATES ARE NOT ROWS. The list arrives from a picker, and a
// picker with an empty chip in it would otherwise write a payload code of ""
// that every screen then offers as a nameless part.
func TestProcessPayloads_ReplaceDropsBlanksAndDuplicates(t *testing.T) {
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")

	if err := db.ReplaceProcessPayloads(seeded.ProcessID,
		[]string{" PART-A ", "", "PART-A", "PART-B", "   "}); err != nil {
		t.Fatalf("ReplaceProcessPayloads: %v", err)
	}
	got, err := db.ListProcessPayloads(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ListProcessPayloads: %v", err)
	}
	if len(got) != 2 || got[0] != "PART-A" || got[1] != "PART-B" {
		t.Fatalf("rows = %v, want the two trimmed distinct names", got)
	}
}

// THE PALETTE IS THE UNION, and this is the case the feature exists for: a
// part nobody has typed into the part set is still offered when a live claim
// of the process already runs it.
func TestProcessPalette_UnionsStoredRowsWithClaimedPayloads(t *testing.T) {
	db := testdb.Open(t)
	fx := scenefixtures.A()
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")

	claims, err := processes.ListLiveClaimsByProcess(db.DB, seeded.ProcessID)
	if err != nil {
		t.Fatalf("ListLiveClaimsByProcess: %v", err)
	}
	claimed := ""
	for _, c := range claims {
		if c.PayloadCode != "" {
			claimed = c.PayloadCode
			break
		}
	}
	if claimed == "" {
		t.Fatal("the fixture seeded no claim with a payload; this test has nothing to union")
	}

	if err := db.ReplaceProcessPayloads(seeded.ProcessID, []string{"TYPED-ONLY"}); err != nil {
		t.Fatalf("ReplaceProcessPayloads: %v", err)
	}
	palette, err := db.ProcessPalette(seeded.ProcessID)
	if err != nil {
		t.Fatalf("ProcessPalette: %v", err)
	}
	has := func(code string) bool { return slices.Contains(palette, code) }
	if !has("TYPED-ONLY") {
		t.Errorf("palette %v does not carry the typed row", palette)
	}
	if !has(claimed) {
		t.Errorf("palette %v does not carry %q, which a live claim of this process runs — "+
			"a part the cell already makes has to stay offerable without anyone retyping it",
			palette, claimed)
	}
	// One entry per code, however many claims name it.
	seen := map[string]int{}
	for _, p := range palette {
		seen[p]++
	}
	for code, n := range seen {
		if n > 1 {
			t.Errorf("palette carries %q %d times; a union is a set", code, n)
		}
	}
}

// A NEW CELL HAS NO CLAIMS, which is the whole reason the table exists: the
// derived-only shape the plan started from returns nothing here, and there is
// then nothing to put on the cell's first position.
func TestProcessPalette_ATypedRowIsOfferedWithNoClaimsAtAll(t *testing.T) {
	db := testdb.Open(t)
	processID, err := db.CreateProcess("NEW-CELL", "new", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	if err := db.ReplaceProcessPayloads(processID, []string{"PART-NEW"}); err != nil {
		t.Fatalf("ReplaceProcessPayloads: %v", err)
	}
	palette, err := db.ProcessPalette(processID)
	if err != nil {
		t.Fatalf("ProcessPalette: %v", err)
	}
	if len(palette) != 1 || palette[0] != "PART-NEW" {
		t.Fatalf("palette = %v, want [PART-NEW] — a cell with no claims still has a part set", palette)
	}
}
