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
// leftover. A site that cannot be described by one of the kinds below does not
// belong on the list; it belongs behind IsLoaderNode.
//
// THE COUNT IS PART OF THE EXEMPTION. The allowlist used to be file-granular:
// it computed how many times each file read the constant, printed that number
// in the failure message, and never asserted on it. domain/process.go is ~1000
// lines and was exempted wholesale, so a third reference anywhere in it passed
// in silence — the guard against the cluster growing back could not see it grow
// inside a file it had already forgiven. Each entry now states how many reads
// it is exempting, and gaining or losing one fails.
//
// n = -1 means the count cannot be bounded and why says what governs it
// instead. No entry uses it today; it exists so that a genuinely unbounded
// site has somewhere to go other than deleting the assertion for everyone.
//
// MUTATION: write `claim.SwapMode == protocol.SwapModeManualSwap` in any file
// not listed below — this names the file and says what to call instead. Add a
// tenth read to domain/process.go — this now names that too.
type modeReader struct {
	n    int    // reads of SwapModeManualSwap in this file; -1 = unbounded
	kind string // declaration | derivation | write | shape | label
	why  string
}

var loaderModeReaders = map[string]modeReader{
	"protocol/swap_mode.go": {3, "declaration", "the constant, plus its membership in AllSwapModes " +
		"and ConfigurableSwapModes. The law that governs it lives here too"},

	"shingo-edge/domain/process.go": {2, "derivation", "THE Edge derivation — NodeClaim.IsLoaderNode " +
		"and its NodeClaimInput twin, one read each. Everything on the Edge asks the question here"},
	"shingo-core/plantspec/plantspec.go": {1, "derivation", "THE Core derivation — Claim.IsLoader"},

	"shingo-edge/domain/loader.go": {1, "write", "Loader.SynthClaim WRITES the mode, it does not read " +
		"it. A Core-owned loader window has no per-style claim, so the synthesised one carries the " +
		"node-kind fact in the field that stores it. Deleting this stamp does not remove a " +
		"branch; it makes every Core-owned loader board stop working"},
	"shingo-edge/store/processes/walk.go": {1, "write", "PayloadsForLoader WRITES the mode into a " +
		"WalkOpts filter — a query predicate against the stored column, which is where the fact lives"},

	"shingo-core/cmd/simcalc/main.go": {1, "shape", "fleetMovesPerSwap costs each step-list shape in " +
		"floor crossings and robots. It switches over every mode because the shape is the question"},
	"shingo-edge/domain/claim_validation.go": {1, "shape", "the per-mode required-field registry. The " +
		"OTHER arms are shape-of-swap questions; THIS one is not — a loader has no shape of swap, and " +
		"the arm only says a loader claim needs an outbound_destination. It is a node-kind arm sitting " +
		"in a switch over the mode, which is the honest description and the reason it is a `shape` " +
		"entry only by adjacency. Rewriting the switch to ask IsLoaderNode for this one arm is a " +
		"change to a validated input path, not a comment fix, so it is left alone deliberately"},

	"shingo-edge/engine/completion_table.go": {1, "label", "the mode's STRING as a completion-case row " +
		"name, for the log. The predicate behind the row is matchManualSwap, which asks IsLoaderNode"},
	"shingo-edge/engine/wiring_counter_delta.go": {1, "label", "the mode's STRING as a skip-reason " +
		"label in a log line"},
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
		e, ok := loaderModeReaders[rel]
		if !ok {
			t.Errorf("%s reads SwapModeManualSwap %d time(s) and is not an allowed reader.\n"+
				"Ask the loader question by name instead — claim.IsLoaderNode() on the Edge, "+
				"c.IsLoader() on Core. If this really is a step-list-shape question and not a "+
				"node-kind one, add the file to loaderModeReaders with the reason, which is the "+
				"argument you would have had to make anyway.", rel, n)
			continue
		}
		if e.n >= 0 && n != e.n {
			t.Errorf("%s reads SwapModeManualSwap %d time(s), but its exemption covers %d.\n"+
				"kind=%s why=%q\n"+
				"A file on this list is forgiven for the reads it was ADDED for, not for any "+
				"number of them. If the new read is the same kind, raise the count and say so; "+
				"if it is not, it belongs behind IsLoaderNode.", rel, n, e.n, e.kind, e.why)
		}
	}

	// The list must not rot either. An entry for a file that no longer mentions
	// the constant is a stale exemption, and a stale exemption silently pre-
	// approves the next reference someone adds to that file.
	for rel, e := range loaderModeReaders {
		if found[rel] == 0 {
			t.Errorf("loaderModeReaders lists %s (%q) but that file no longer references "+
				"SwapModeManualSwap — drop the entry rather than leaving it to pre-approve the "+
				"next one", rel, e.why)
		}
	}
}
