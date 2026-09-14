package bins_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// one_sourcing_predicate_test.go — the guard that keeps one rule from becoming
// six again.
//
// "May this bin be sourced?" was answered by six hand-written WHERE clauses
// that did not agree; a golden photograph found four divergences and the
// 2026-09-14 rulings collapsed them onto bins.BinSourceableSQL. Nothing about
// that collapse stops the seventh reader from being written the old way, and
// the old way is cheap: copy a neighbouring query, adjust the SELECT. That is
// how the six happened.
//
// No database needed — the claim is about source text.

// sourcingReaders are the readers that answer the sourcing question. Each must
// compose the shared predicate and spell none of it itself.
var sourcingReaders = []struct{ file, fn string }{
	{"bin_manifest.go", "func FindSourceFIFO("},
	{"../lane_queries.go", "func (db *DB) FindSourceBinInLane("},
	{"../sourceability/read.go", "func availablePoolByPayload("},
	{"../sourceability/read_page.go", "func PoolBreakdownByPayload("},
	{"../../service/inventory_preflight.go", "func (s *InventoryService) PreflightAvailability("},
}

// handSpelled are the clauses that belong to the one predicate. A sourcing
// reader naming any of them literally has started a second spelling.
var handSpelled = []string{
	"claimed_by IS NULL",
	"locked = false",
	"manifest_confirmed = true",
	"payload_bin_types",
	"n.enabled",
	"is_synthetic",
	"status NOT IN",
}

func readBody(t *testing.T, file, fn string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(file))
	if err != nil {
		t.Fatalf("read %s: %v (if the reader moved, repoint this guard rather than deleting it)", file, err)
	}
	_, rest, found := strings.Cut(string(b), fn)
	if !found {
		t.Fatalf("could not find %q in %s — the guard is stale, and a silent miss "+
			"would leave it passing on nothing while the spelling question went unwatched", fn, file)
	}
	body, _, _ := strings.Cut(rest, "\nfunc ")
	return body
}

// TestSourcingReadersSpellNothingThemselves.
//
// VERIFIED RED BY: putting `AND b.claimed_by IS NULL` back into
// availablePoolByPayload — the test named the reader and the clause.
func TestSourcingReadersSpellNothingThemselves(t *testing.T) {
	t.Parallel()
	for _, r := range sourcingReaders {
		body := readBody(t, r.file, r.fn)
		for _, clause := range handSpelled {
			if strings.Contains(body, clause) {
				t.Errorf("%s (%s) spells %q itself.\n\n"+
					"That clause belongs to bins.BinSourceableSQL. Six readers spelling it "+
					"separately is the drift the 2026-09-14 collapse ended: they disagreed on "+
					"off-spec statuses, on the bin-type rule and on disabled nodes, and nothing "+
					"said so until a golden file was built. Compose the predicate (or a named "+
					"part of it) instead.",
					r.fn, r.file, clause)
			}
		}
		if !strings.Contains(body, "BinSourceableSQL") &&
			!strings.Contains(body, "BinCarriesSourceableStockSQL") {
			t.Errorf("%s (%s) composes neither BinSourceableSQL nor its stock half — "+
				"a sourcing reader that references the shared predicate nowhere is "+
				"answering the question some other way", r.fn, r.file)
		}
	}
}

// TestNoBinStatusRejectListSurvives is a frozen ratchet, not a clean sheet.
//
// The status column carries no CHECK constraint, so a reject-list answers TRUE
// for any value it forgot to name and an off-spec status slips through by
// default. The 2026-09-14 ruling deleted every reject-list in the SOURCING
// readers in favour of the SourceableStatusSQL allow-list.
//
// THREE SURVIVE, DELIBERATELY, AND ARE LISTED HERE RATHER THAN FIXED. They are
// not eligibility readers — they are physical-inventory totals (the system bin
// count, the system UOP total, the lineside ledger's bin term). For a sourcing
// reader, failing closed on an unrecognised status is right: do not send a
// robot there. For a count of what is physically in the plant, failing closed
// means under-reporting stock that is really there, which is the opposite of
// what a cycle-count surface is for. That is a different decision from the one
// the ruling made, so it is reported and left, not quietly taken.
//
// The list is frozen: a fourth reject-list fails this test, and removing one of
// these three means deleting its line here. Either way nobody adds one by
// copying a neighbour.
//
// Scoped to BIN statuses by the literals it matches — order and leg statuses
// are a different column with a different rule and legitimately use NOT IN.
//
// VERIFIED RED BY: restoring the reject-list in availablePoolByPayload.
func TestNoBinStatusRejectListSurvives(t *testing.T) {
	t.Parallel()

	// Known, reported, awaiting a ruling of their own: inventory totals, never
	// sourcing.
	frozen := map[string]bool{
		"inventory_system_count.go":    true,
		"inventory_lineside_ledger.go": true,
	}

	rejectList := regexp.MustCompile(`status NOT IN \([^)]*'(maintenance|flagged|retired|quality_hold)'`)

	var offenders []string
	roots := []string{".", "../", "../sourceability", "../../service", "../../dispatch", "../../engine", "../../www"}
	for _, root := range roots {
		entries, err := os.ReadDir(filepath.FromSlash(root))
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if frozen[name] {
				continue
			}
			path := filepath.Join(filepath.FromSlash(root), name)
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			for line := range strings.SplitSeq(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue // a comment recording the history is not a spelling
				}
				if rejectList.MatchString(line) {
					offenders = append(offenders, path+": "+strings.TrimSpace(line))
				}
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("a bin-status REJECT-LIST is back:\n  %s\n\n"+
			"Use bins.SourceableStatusSQL. A reject-list admits every status it does "+
			"not name, the column has no CHECK constraint, and write-time validation "+
			"is deferred so off-spec values are representable — so the list is wrong "+
			"the day anyone hand-corrects a row.",
			strings.Join(offenders, "\n  "))
	}
}
