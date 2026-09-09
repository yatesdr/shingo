package protocol_test

import (
	"go/ast"
	"go/token"
	"testing"
)

// THE GUARD UNDER THE SWAP-MODE LAW.
//
// The law on protocol.SwapMode says a gate reads the steps or a declared
// property, never the mode name, and that node kind is not a swap mode. The
// loader question is the standing case: "is this a forklift-managed window?"
// used to be answered by comparing SwapMode to manual_swap at forty-odd sites
// across twenty-six files, which is how the concept ended up with no name.
//
// It now has two derivations — domain.NodeClaim.IsLoaderNode on the Edge,
// plantspec.Claim.IsLoader on Core — and this test is what stops a third from
// appearing. Without it the cluster grows back one honest little comparison at
// a time, each of which looks fine in its own diff.
//
// WHAT THE ALLOWLIST IS FOR. Every remaining reference is here with the reason
// it is allowed to stay, so the residue is a written record rather than a
// leftover. Three kinds live here and only three: the declaration itself, the
// derivations, and the sites where the mode genuinely IS the answer — choosing
// or costing a step list, or naming one for a human to read. A site that cannot
// be described as one of those does not belong on the list; it belongs behind
// IsLoaderNode.
//
// MUTATION: write `claim.SwapMode == protocol.SwapModeManualSwap` in any file
// not listed below — this names the file and says what to call instead.
var loaderModeReaders = map[string]string{
	"protocol/swap_mode.go": "the declaration, and the law that governs it",

	"shingo-edge/domain/process.go": "THE Edge derivation — NodeClaim.IsLoaderNode and its " +
		"NodeClaimInput twin. Everything on the Edge asks the question here",
	"shingo-core/plantspec/plantspec.go": "THE Core derivation — Claim.IsLoader",

	"shingo-edge/domain/loader.go": "Loader.SynthClaim WRITES the mode, it does not read it. A " +
		"Core-owned loader window has no per-style claim, so the synthesised one carries the " +
		"node-kind fact in the field that stores it. Deleting this stamp does not remove a " +
		"branch; it makes every Core-owned loader board stop working",
	"shingo-edge/store/processes/walk.go": "PayloadsForLoader WRITES the mode into a WalkOpts " +
		"filter — a query predicate against the stored column, which is where the fact lives",

	"shingo-edge/domain/claim_validation.go": "the per-mode required-field registry. Its arms say " +
		"what each SHAPE OF SWAP needs configured, so the mode is the answer and not a proxy",
	"shingo-core/cmd/simcalc/main.go": "fleetMovesPerSwap costs each step-list shape in floor " +
		"crossings and robots. It switches over every mode because the shape is the question",

	"shingo-edge/engine/completion_table.go": "the mode's STRING as a completion-case row name, " +
		"for the log. The predicate behind the row is matchManualSwap, which asks IsLoaderNode",
	"shingo-edge/engine/wiring_counter_delta.go": "the mode's STRING as a skip-reason label in a " +
		"log line",
}

// TestLoaderQuestionHasOneDerivationPoint walks every production file in every
// module and fails on any reference to the manual_swap constant that is not
// accounted for above.
func TestLoaderQuestionHasOneDerivationPoint(t *testing.T) {
	t.Parallel()

	found := map[string]int{}
	walkRepoProduction(t, func(rel string, file *ast.File, _ *token.FileSet) {
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if ok && id.Name == "SwapModeManualSwap" {
				found[rel]++
			}
			return true
		})
	})

	// A walk that matches nothing would pass every assertion below in silence,
	// and this one has a known floor: the declaration and both derivations.
	if len(found) < 3 {
		t.Fatalf("found SwapModeManualSwap in only %d files — the walk or the identifier match is "+
			"broken, so this guard is passing vacuously rather than the repo being clean", len(found))
	}

	for rel, n := range found {
		if _, ok := loaderModeReaders[rel]; !ok {
			t.Errorf("%s reads SwapModeManualSwap %d time(s) and is not an allowed reader.\n"+
				"Ask the loader question by name instead — claim.IsLoaderNode() on the Edge, "+
				"c.IsLoader() on Core. If this really is a step-list-shape question and not a "+
				"node-kind one, add the file to loaderModeReaders with the reason, which is the "+
				"argument you would have had to make anyway.", rel, n)
		}
	}

	// The list must not rot either. An entry for a file that no longer mentions
	// the constant is a stale exemption, and a stale exemption silently pre-
	// approves the next reference someone adds to that file.
	for rel, why := range loaderModeReaders {
		if found[rel] == 0 {
			t.Errorf("loaderModeReaders lists %s (%q) but that file no longer references "+
				"SwapModeManualSwap — drop the entry rather than leaving it to pre-approve the "+
				"next one", rel, why)
		}
	}
}
