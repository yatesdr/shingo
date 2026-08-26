package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The release paths must not drift apart on ordering.
//
// Every operator release surface has the same two halves — gates and
// validations that can refuse, then side effects that cannot be taken back —
// and the same failure mode when they get interleaved: an ADVISORY refusal
// ("not yet, click again") that has already shipped a manifest, zeroed a count
// or created an order. It has happened twice on this branch, in two different
// files:
//
//	ReleaseStagedOrders      produceIngestAtRelease ran 26 lines above the
//	                         collision gate, so a held release shipped the
//	                         departing bin's manifest and zeroed the press.
//	ReleaseOrderWithLineside MaybeCreateUnloaderFullIn — which creates a real
//	                         order at the unloader on the premise that a bin was
//	                         just finished — ran before the release was enqueued.
//
// This is a CENSUS, not an ordering analysis: deciding from source whether a
// given `return err` is a gate refusing or a side effect reporting failure is
// not reliably decidable, and a test that guessed would be worse than none.
// What it pins is that the set of release surfaces is known, and that the two
// that carry real gate/side-effect tension carry the stated rule where the next
// person to edit them will read it. A NEW release surface fails this test until
// somebody has decided which half it belongs in.
// ---------------------------------------------------------------------------

// releaseSurfacesWithStatedOrdering are the release entry points whose bodies
// mix refusals with side effects, and which therefore have to say so. The value
// is the ordering test that pins the behaviour.
var releaseSurfacesWithStatedOrdering = map[string]string{
	"ReleaseStagedOrders":      "TestReleaseStagedOrders_HeldReleaseShipsNoPaperwork",
	"ReleaseOrderWithLineside": "TestReleaseOrderWithLineside_U1FiresAfterTheRelease",
}

// releaseSurfacesWithoutGates are the entry points that have no gate/side-effect
// tension to state: thin wrappers, or paths whose every validation already
// precedes their single side effect.
var releaseSurfacesWithoutGates = map[string]string{
	"ReleaseNodeEmpty":             "wrapper around ReleaseNodePartial",
	"ReleaseNodePartial":           "wrapper around releaseNodeInternal",
	"ReleaseNodeWithRemainingUOP":  "wrapper around releaseNodeInternal",
	"ReleaseChangeoverWait":        "fans out to ReleaseChangeoverWaitForNode",
	"ReleaseChangeoverWaitForNode": "validates, then releases; no side effect precedes a refusal",
}

var engineReleaseFunc = regexp.MustCompile(`func \(e \*Engine\) (Release\w+|FinalizeProduce\w+)\(`)

func TestReleaseSurfacesAreCensused(t *testing.T) {
	t.Parallel()

	found := map[string]string{} // name -> file
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range engineReleaseFunc.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = name
		}
	}
	if len(found) == 0 {
		t.Fatal("no Engine release surfaces found — the matcher has drifted from the code")
	}

	var uncensused []string
	for name, file := range found {
		_, stated := releaseSurfacesWithStatedOrdering[name]
		_, exempt := releaseSurfacesWithoutGates[name]
		if !stated && !exempt {
			uncensused = append(uncensused, name+" ("+file+")")
		}
	}
	sort.Strings(uncensused)
	if len(uncensused) > 0 {
		t.Errorf("release surface(s) not censused:\n  %s\n\n"+
			"Every operator release path has the same two halves — refusals, then side effects —\n"+
			"and the same failure mode when they interleave: an advisory \"not yet, click again\"\n"+
			"that has already shipped a manifest, zeroed a count, or created an order. Decide which\n"+
			"half this one is, then either add it to releaseSurfacesWithStatedOrdering with the\n"+
			"ordering test that pins it, or to releaseSurfacesWithoutGates with the reason it has\n"+
			"no tension to state.",
			strings.Join(uncensused, "\n  "))
	}

	// A censused surface that no longer exists is a stale entry, and a stale
	// census reads as coverage it is not providing.
	for name, pinnedBy := range releaseSurfacesWithStatedOrdering {
		if _, ok := found[name]; !ok {
			t.Errorf("releaseSurfacesWithStatedOrdering names %s, which no longer exists — delete the entry", name)
			continue
		}
		// The named test must EXIST. An entry pointing at a test somebody
		// renamed or deleted is the census reporting coverage that is gone,
		// which is worse than an honest gap.
		if !testExistsInPackage(t, pinnedBy) {
			t.Errorf("%s is censused as pinned by %s, but no such test exists in this package.\n"+
				"Either restore the test or point the entry at the one that replaced it.", name, pinnedBy)
		}
	}
	for name := range releaseSurfacesWithoutGates {
		if _, ok := found[name]; !ok {
			t.Errorf("releaseSurfacesWithoutGates names %s, which no longer exists — delete the entry", name)
		}
	}
}

// TestReleaseSurfacesStateTheirOrdering checks that the two surfaces with real
// tension carry the rule in their own source, where the next person editing
// them will read it — not only in a test file they may never open.
func TestReleaseSurfacesStateTheirOrdering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ file, marker string }{
		{"operator_stations.go", "FROM HERE ON, SIDE EFFECTS"},
		{"operator_release.go", "U1 AFTER THE RELEASE, NOT BEFORE IT"},
	} {
		src, err := os.ReadFile(filepath.Clean(tc.file))
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if !strings.Contains(string(src), tc.marker) {
			t.Errorf("%s no longer states its gate/side-effect ordering (looked for %q).\n"+
				"The rule is what stops the next edit from putting a side effect above a refusal; "+
				"if the code changed shape, restate it rather than dropping it.", tc.file, tc.marker)
		}
	}
}

// testExistsInPackage reports whether a Test function of this name is declared
// anywhere in the package's test files.
func testExistsInPackage(t *testing.T, name string) bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	decl := "func " + name + "("
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if strings.Contains(string(src), decl) {
			return true
		}
	}
	return false
}
