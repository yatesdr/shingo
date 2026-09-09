package protocol_test

import (
	"go/ast"
	"go/token"
	"testing"
)

// THE GUARD UNDER CLAIM GEOMETRY.
//
// "Which core nodes does this claim occupy" was answered by hand-writing the
// three layout fields into a []string at a dozen sites across nine files. The
// duplication is not hypothetical: the extension-positions helper was extracted
// only after the changeover fan-out and the participant builder disagreed about
// a press's geometry, and that extraction reached two of the sites.
//
// It now has one derivation per shape — domain.NodeClaim.Positions and
// ExtensionPositions for the stored claim, NodeClaimInput.Positions for the
// input twin — and this is what stops a thirteenth spelling from appearing. The
// cluster grows back one honest little literal at a time otherwise, each of
// which looks fine in its own diff.
//
// WHY THE LITERAL AND NOT THE FIELD. The obvious guard — count reads of
// SecondPairedCoreNode per file, the shape the loader guard uses — is the wrong
// instrument here, and building it would have produced a second dumping ground.
// SwapModeManualSwap is a constant whose every read asks one question. These are
// FIELDS with two legitimate uses that no count can tell apart: as a SET (which
// nodes does this cell hold) and POSITIONALLY (A indexes from B, B from C).
// material_orders.go alone reads them nine times, every one of them positional
// and every one of them correct. A per-file count would have forgiven all nine
// and then forgiven the tenth, which is exactly the failure the loader guard's
// own count assertion was added to fix.
//
// So this matches the SHAPE the duplication actually takes: a []string literal
// naming both index positions. Positional reads never build one; the set
// spelling always did. A struct literal copying the fields across types (see
// replenishment_admin.go) is a conversion, not a derivation, and does not match.
//
// MUTATION: write `[]string{c.PairedCoreNode, c.SecondPairedCoreNode}` anywhere
// — this names the file and says to call Positions or ExtensionPositions
// instead.
var claimGeometryLiterals = map[string]modeReader{
	// The one entry, and it records a decision rather than an exception.
	//
	// The geometry question has three faces: the Edge's stored claim
	// (domain.NodeClaim), the Edge's unsaved input (domain.NodeClaimInput) and
	// Core's plant spec (plantspec.Claim). The first two share a package and a
	// derivation. The third cannot: shingo-core and shingo-edge are separate Go
	// modules, so the only shared homes are protocol/ — a WIRE package, and cell
	// geometry is not wire — or a new shared/ package holding a helper over three
	// strings. Neither is earned by one site, and the site is a validator that
	// must name which field is wrong anyway.
	//
	// If Core ever grows a second spelling, that is when the shared home is
	// earned, and this entry going from 1 to 2 is what says so.
	"shingo-core/plantspec/validate.go": {1, "spec", "Core's own face of the geometry question — " +
		"the changeover_evac_nodes held-set, checked at spec-read because a plant file is written " +
		"long before an Edge sees it. Cross-module, so it cannot share the Edge derivation"},
}

// TestClaimGeometryHasOneDerivationPoint walks every production file in every
// module and fails on any []string literal that names both index positions.
func TestClaimGeometryHasOneDerivationPoint(t *testing.T) {
	t.Parallel()

	found := map[string]int{}
	// The walk must be seen to work: this guard's clean state is ZERO matches
	// outside the allowlist, so a broken matcher and a clean repo look identical.
	// Counting files that mention the field at all is the independent witness.
	mentions := 0
	walkRepoProduction(t, func(rel string, file *ast.File, _ *token.FileSet) {
		mentionsHere := false
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "SecondPairedCoreNode" {
				mentionsHere = true
			}
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isStringSlice(lit.Type) {
				return true
			}
			if selectsBoth(lit.Elts) {
				found[rel]++
			}
			return true
		})
		if mentionsHere {
			mentions++
		}
	})
	if mentions < 5 {
		t.Fatalf("only %d production files mention SecondPairedCoreNode — the walk or the identifier "+
			"match is broken, so this guard is passing vacuously rather than the repo being clean", mentions)
	}

	for rel, n := range found {
		e, ok := claimGeometryLiterals[rel]
		if !ok {
			t.Errorf("%s builds a []string of the claim's index positions %d time(s).\n"+
				"Ask for the geometry by name instead — claim.Positions() for the whole cell, "+
				"claim.ExtensionPositions() for the positions behind the front node, "+
				"in.Positions() on an unsaved input. If this is genuinely a POSITIONAL read "+
				"(A indexes from B) it should not be a list at all.", rel, n)
			continue
		}
		if e.n >= 0 && n != e.n {
			t.Errorf("%s builds the geometry list %d time(s), but its exemption covers %d.\n"+
				"kind=%s why=%q\n"+
				"A file on this list is forgiven for the literals it was ADDED for, not for any "+
				"number of them.", rel, n, e.n, e.kind, e.why)
		}
	}

	for rel, e := range claimGeometryLiterals {
		if found[rel] == 0 {
			t.Errorf("claimGeometryLiterals lists %s (%q) but that file no longer builds one — "+
				"drop the entry rather than leaving it to pre-approve the next one", rel, e.why)
		}
	}
}

// isStringSlice reports whether a composite literal's type is []string. A
// STRUCT literal that happens to carry the same fields across types is a
// conversion, not a derivation of the set, and is deliberately not matched.
func isStringSlice(t ast.Expr) bool {
	arr, ok := t.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "string"
}

// selectsBoth reports whether these literal elements name both index positions.
// BOTH, not either: a one-element list is a positional read (the back position
// specifically), and the duplication being guarded is always the pair.
func selectsBoth(elts []ast.Expr) bool {
	var paired, second bool
	for _, e := range elts {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		switch sel.Sel.Name {
		case "PairedCoreNode":
			paired = true
		case "SecondPairedCoreNode":
			second = true
		}
	}
	return paired && second
}
