package bins_test

import (
	"io/fs"
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
// TWO SURVIVE, DELIBERATELY, AND ARE LISTED HERE RATHER THAN FIXED. They are
// not eligibility readers — they are physical-inventory totals (the system bin
// count and the system UOP total, both in inventory_system_count.go). A third,
// the lineside ledger's per-node bin term, was deleted with the R1 read-model
// it fed (seat-count round 1, lane A of the memory build). For a sourcing
// reader, failing closed on an unrecognised status is right: do not send a
// robot there. For a count of what is physically in the plant, failing closed
// means under-reporting stock that is really there, which is the opposite of
// what a cycle-count surface is for. That is a different decision from the one
// the ruling made, so it is reported and left, not quietly taken.
//
// The list is frozen: a third reject-list fails this test, and removing one of
// these two means deleting its line here. Either way nobody adds one by
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
		"inventory_system_count.go": true,
	}

	rejectList := regexp.MustCompile(`status NOT IN \([^)]*'(maintenance|flagged|retired|quality_hold)'`)

	var offenders []string
	for _, path := range moduleGoFiles(t) {
		if frozen[filepath.Base(path)] {
			continue
		}
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
	if len(offenders) > 0 {
		t.Errorf("a bin-status REJECT-LIST is back:\n  %s\n\n"+
			"Use bins.SourceableStatusSQL. A reject-list admits every status it does "+
			"not name, the column has no CHECK constraint, and write-time validation "+
			"is deferred so off-spec values are representable — so the list is wrong "+
			"the day anyone hand-corrects a row.",
			strings.Join(offenders, "\n  "))
	}
}

// moduleGoFiles walks the whole shingo-core module and returns every
// non-test .go file in it.
//
// ── WHY A WALK AND NOT A LIST OF ROOTS ────────────────────────────────────
//
// This guard used to read seven named directories with os.ReadDir and `if
// e.IsDir() { continue }`, which is not a recursion — it is a list of seven
// directories, and nothing below any of them was ever looked at. Two things
// were wrong with that.
//
// IT DID NOT COVER THE PREDICATE'S OWN HOME. store/internal/helpers is where
// BinSourceableSQL lives, and it sat outside the guard that exists to protect
// it. A reject-list written in the file next to the allow-list would have
// passed.
//
// AND THE ROOTS WERE ALREADY LYING ABOUT THEIR REACH. "../../dispatch" reads
// dispatch/ and not dispatch/binresolver, dispatch/binsource or
// dispatch/loaders — which is most of the sourcing code by volume, and all of
// the Go predicates the three dispatch doors run on.
//
// A walk cannot drift as the tree is reorganised, which is the property a
// ratchet needs: the guard should not have to be edited every time a package
// is split, because the edit that keeps it compiling is also the edit that
// quietly narrows it.
func moduleGoFiles(t *testing.T) []string {
	t.Helper()

	root, err := filepath.Abs(filepath.FromSlash("../.."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod (%v) — this guard walks the module "+
			"from store/bins, so a move breaks its bearings; repoint it rather than "+
			"deleting it", root, err)
	}

	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata holds golden fixtures, not code.
			if d.Name() == "testdata" || d.Name() == "node_modules" || d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("walked only %d .go files from %s — a guard that silently walks "+
			"nothing passes on nothing", len(out), root)
	}
	return out
}

// goPredicates are the pure Go predicates the three dispatch doors run on —
// tier-4 concrete-node pickup, the tier-2 dedicated-loader pool, and the
// complex allocator. They are not SQL readers, so the clause list above cannot
// speak about them; what they must not lose is the bin-type arm.
//
// THE RULE REACHED THEM LAST. Every SQL sourcing reader has enforced
// payload_bin_types since 493a8062 and these two enforced nothing, because the
// doors they serve list bins with ListBinsByNode and filter in Go. A bin loaded
// with a part its carrier may not hold counts as stock and can never be
// fetched, and that is the row the gap produced.
var goPredicates = []struct{ file, fn, must string }{
	{"../../dispatch/binresolver/helpers.go", "func BinUnavailableReason(", "binTypes.Permits"},
	{"../../dispatch/binsource/source.go", "func RejectReason(", "binTypeReject"},
}

// TestGoPredicatesKeepTheBinTypeArm.
//
// VERIFIED RED BY: deleting the Permits call from BinUnavailableReason.
func TestGoPredicatesKeepTheBinTypeArm(t *testing.T) {
	t.Parallel()
	for _, p := range goPredicates {
		body := readBody(t, p.file, p.fn)
		if !strings.Contains(body, p.must) {
			t.Errorf("%s (%s) no longer consults the bin-type rule (looked for %q).\n\n"+
				"payload_bin_types is hard where it has rows: a part may only travel in "+
				"a carrier the plant declares for it. This predicate is one of the two "+
				"the three dispatch doors reduce to, so dropping the arm re-opens the "+
				"gap at all three at once — and the bins it admits are ones every SQL "+
				"sourcing reader refuses, so they become stock nothing can fetch.",
				p.fn, p.file, p.must)
		}
	}
}

// onLineComplement are the two readers that answer "is it already AT the line",
// not "can it be fetched" — the staged-at-line complement of the sourcing
// question, and disjoint from it by construction.
//
// THEY ARE ON THE DRIFT LIST RATHER THAN THE READER LIST, and the difference is
// the `status = 'staged'` term. Every sourcing reader excludes staged, because
// a staged bin is one an operator is working at; these two want ONLY staged,
// which is what makes them the complement rather than a seventh spelling. Drop
// that term and the reader silently becomes a sourcing reader that answers the
// sourcing question wrongly — no compile error, no failing test, and a pool
// count that now includes bins nothing may take.
//
// No logic of theirs changes here. They are watched, not collapsed.
var onLineComplement = []struct{ file, fn string }{
	{"../sourceability/read.go", "func onLinePoolByProcess("},
	{"../sourceability/read_page.go", "func OnLineBreakdownByProcess("},
}

// TestOnLineComplementStaysTheComplement.
//
// VERIFIED RED BY: deleting the staged term from onLinePoolByProcess.
func TestOnLineComplementStaysTheComplement(t *testing.T) {
	t.Parallel()
	for _, r := range onLineComplement {
		body := readBody(t, r.file, r.fn)
		if !strings.Contains(body, `b.status = 'staged'`) {
			t.Errorf("%s (%s) lost its `status = 'staged'` term.\n\n"+
				"That term is the ONLY thing separating this reader from a sourcing "+
				"reader. Sourcing excludes staged; this counts only staged. Without it "+
				"the two questions collide and this becomes a seventh spelling of "+
				"'may this bin be sourced' that answers it wrongly.", r.fn, r.file)
		}
		// The disabled-node rule binds them too — a dead node holds no countable
		// stock, which is the same ruling lineUOPByNode was corrected under.
		if !strings.Contains(body, "BinAtLiveNodeSQL") {
			t.Errorf("%s (%s) no longer composes BinAtLiveNodeSQL — a switched-off "+
				"node is dead to automation, counting included", r.fn, r.file)
		}
	}
}
