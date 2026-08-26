package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// origin_totality_test.go — every order create names its origin.
//
// ── WHY A CENSUS AND NOT A LINT RULE ──────────────────────────────────────
//
// The first attempt at this fence was two forbidigo patterns,
// `ordermgr\.Origin\{\}` and `orders\.Origin\{\}`. THEY MATCHED NOTHING, and
// the run reported "0 issues" — indistinguishable from a fence that works.
// forbidigo walks identifiers and selector expressions, so what it sees at a
// composite literal is `ordermgr.Origin`; the braces are syntax and are not
// part of the node's text. Widening the pattern to `ordermgr\.Origin` would
// have matched every parameter and every struct field of that type too, which
// is most of the machinery this is meant to protect.
//
// It was caught by deliberately writing a bare Origin{} at an unsanctioned site
// and re-running the linter, which is the only thing that distinguishes a guard
// from a claim. The census below is red under that same mutation.
//
// AN AST WALK CAN TELL A LITERAL FROM A DECLARATION, which is the whole
// distinction the lint rule could not express: `origin ordermgr.Origin` is a
// parameter and fine, `ordermgr.Origin{}` is an unstated origin and is not.
//
// ── WHAT AN UNSTATED ORIGIN COSTS ─────────────────────────────────────────
//
// It is the whole of specimen (c). The zero value was defended as honest —
// "Core classifies those; saying nothing here is honest, where guessing would
// not be" — and every door nobody had wired took it: the HMI button, the
// sequential backfill, the changeover applier's legs, RequestEmptyBin's
// multi-step arm, the four operator-driven changeover entry points. Each was
// demand-serving, each reached Core carrying nothing, and Core honestly stamped
// orphan. The episode surface showed changeovers and thresholds only — not
// mislabelled, absent.
//
// The unattributed create wrappers are deleted, so "forgot" is a compile error
// now. This covers the one hole Go leaves open.

// originZeroAllowed lists the files permitted to build a zero Origin, and each
// one is here because it has just LOGGED why it could not resolve an episode.
//
// That is a third state neither constructor describes: the demand is real and
// we failed to record it. Calling it NoDemand would be a lie that removes a
// live defect from the orphan bucket permanently — and a wrong no_demand is the
// one direction this campaign cannot afford, because it answers the question
// and leaves. A wrong attachment is visible on a surface somebody reads.
//
// Each entry comes off this list the day its failure mode is made unreachable
// rather than merely reported.
var originZeroAllowed = map[string]string{
	"demand_episode.go":     "the process name would not resolve, or no episode is open to join — both logged",
	"changeover_applier.go": "the changeover's origin could not be read back — logged",
	"operator_stations.go":  "openCellEpisode returned no id for a consume request — logged",
	"operator_produce.go":   "openCellEpisode returned no id for a produce request — logged",
	"operator_bin_ops.go":   "openCellEpisode returned no id for an operator request — logged",
}

// TestOriginTotality_NoUnstatedOriginOutsideTheEpisodeHelpers is the census.
//
// MUTATION (verified): write `ordermgr.Origin{}` at any create site in this
// package — wiring_status_changed.go's sequential backfill is the one it was
// checked against — and this fails naming the file and line. Delete an entry
// from originZeroAllowed and it fails for that file, which is what keeps the
// allowlist from silently growing.
func TestOriginTotality_NoUnstatedOriginOutsideTheEpisodeHelpers(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var offenders []string
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := originZeroAllowed[name]; ok {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) != 0 {
				// A literal with fields set is stating something; the defect is
				// specifically the EMPTY one, which states nothing.
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Origin" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || (pkg.Name != "ordermgr" && pkg.Name != "orders") {
				return true
			}
			offenders = append(offenders, name+":"+
				strconv.Itoa(fset.Position(lit.Pos()).Line))
			return true
		})
	}

	// A census that scanned nothing must not read as a census that found
	// nothing — the same rule the instruments in this batch are built on.
	if scanned == 0 {
		t.Fatal("the census parsed ZERO files, so its silence means nothing. Check the package " +
			"directory and the allowlist: every file being excluded is itself the finding.")
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("unstated origin at %d site(s): %s\n\n"+
			"A bare Origin{} says nothing, and Core classifies an order that says nothing as an "+
			"ORPHAN. That is not a labelling detail: an order with no origin is invisible to every "+
			"instrument keyed on the episode, and a service dig raised for one cannot look up who "+
			"is collecting its target — so it hands its corridor to nobody and files an alarm "+
			"against a demand that was, in fact, coming.\n\n"+
			"Name it. orders.Attached(originID) when the order serves a demand episode; "+
			"orders.NoDemand() when it belongs to no episode BY CONSTRUCTION — a direct API "+
			"command, a loader's owed outbound move, housekeeping the system schedules for "+
			"itself. If you are unsure, it is the NoDemand answer that needs the argument: a wrong "+
			"no_demand answers the question and leaves the orphan bucket for good, where a wrong "+
			"attachment is visible on a surface somebody reads.\n\n"+
			"If this site genuinely could not resolve an episode and has LOGGED why, add it to "+
			"originZeroAllowed with the reason — and expect to be asked why the failure is only "+
			"reported rather than made unreachable.",
			len(offenders), strings.Join(offenders, ", "))
	}
}
