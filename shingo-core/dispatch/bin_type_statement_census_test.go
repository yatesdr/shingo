package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCensus_BinTypeStatements pins every site that resolves a STORE, and what
// each one is able to say about the bin type it is placing.
//
// ── WHY THIS LIST EXISTS ────────────────────────────────────────────────────
//
// The per-node Allowed Bin Types fence was written, tested, and inert for years:
// the bin type reached it as `binTypeID *int64` and five of seven callers passed
// nil. Wiring it by deriving the bin type inside the resolver fixed six of them
// and left intake still blind, because intake resolves BEFORE the order row
// exists. A knockdown went to a tote-only position at Hopkinsville on
// 2026-09-22 with every gate behaving exactly as written.
//
// Both misses have the same cause and it is not carelessness: reasoning about
// which of N call sites can answer a question is not something a person does
// reliably, and nothing failed when they got it wrong. So the answer stops being
// reasoning. Every site states its carrier in the call, this list names every
// site, and a new one fails here until somebody adds it and says which it is.
//
// This is the same instrument as TestCensus_OrderCreationDoors, and for the same
// reason: that list exists because somebody assumed CreateInboundOrder was "the
// admission path" and the census disproved it. The doors census would not have
// caught this one — a door is where an order is BORN, and these are where its
// destination is CHOSEN, which is a different question with a different answer.
//
// ── THE THIRD COLUMN IS THE POINT ───────────────────────────────────────────
//
// `states` is what the site can honestly say. A site marked "unknown" is not a
// bug to be fixed on sight — resolving untyped is what every order did before
// the fence, and refusing instead would turn a read gap into a parked robot. It
// is a site where the fence DOES NOT APPLY, and saying so out loud is the whole
// value: GroupResolver.settleBinType logs each one against the group it was
// resolving, so the plant's own traffic says which paths are blind rather than
// somebody's reading of the call graph.
func TestCensus_BinTypeStatements(t *testing.T) {
	t.Parallel()

	type site struct {
		file   string
		fn     string
		states string // "known" | "settled" | "unknown" | "none"
		why    string
	}
	sites := []site{
		// KNOWN — the site holds the answer and says it.
		{"dispatch/lifecycle_service.go", "resolveSyntheticDestination", "known",
			"binTypeAtIntake: the maintained episode, the order's own bin, or the source node holding exactly one carrier. THE SITE THAT PLACED A KNOCKDOWN IN A TOTE-ONLY POSITION before it had this"},
		{"dispatch/lifecycle_service.go", "tryOverflow", "known",
			"the same placement one group over, so it gets the same answer rather than a second weaker one"},
		{"engine/maintainer.go", "createAsks", "known",
			"the level IS a bin type; the one door that always named it, and the only caller that exercised the fence for years"},
		{"dispatch/complex_steps.go", "resolveStepNode", "known",
			"the paired pickup resolves to a slot holding exactly one carrier; unknown when it is not exactly one"},

		// SETTLED — the site cannot say, but the order is persisted and holds a
		// bin or a reservation, so settleBinType recovers it from the order id.
		{"dispatch/store_slot.go", "resolveSyntheticDropoff", "settled",
			"scanner deferral keeper: runs inside ReserveStorageDropoff, after the source claim"},
		{"dispatch/store_slot.go", "redirectStoreOffDugLane", "settled",
			"re-aims an order that already chose a destination, so it holds its bin"},
		{"dispatch/lane_gate_release.go", "widenDropoffToGroup", "settled",
			"at the lane gate the order is mid-plan and holds its carrier"},
		{"dispatch/planning_service.go", "planMove", "settled",
			"runs after CreateInboundOrder, so the order id is real"},

		// NONE — places nothing.
		{"dispatch/source_finder.go", "FindSourceForNeed", "none",
			"a retrieve is looking FOR a carrier, not putting one down"},
	}

	// Every named site must still resolve a store (or, for "none", a retrieve).
	// A site that stops doing so has moved or merged, and this list is then
	// describing a system that does not exist.
	root := repoRootForCensus(t)
	for _, s := range sites {
		body := readSourceForCensus(t, filepath.Join(root, s.file))
		if !strings.Contains(body, s.fn) {
			t.Errorf("site %q names %s in %s, which no longer exists. If it moved, say where; "+
				"if it merged, delete the entry and widen the one it merged into.", s.fn, s.fn, s.file)
		}
	}

	// AND THE DIRECTION THAT CATCHES A NEW WAY IN: no file may call ResolveStore
	// or Resolve(...ResolveModeStore...) without being named above. This is the
	// arm that would have caught engine/maintainer.go, which calls ResolveStore
	// DIRECTLY rather than through Resolve and was missed by a hand census of
	// `.Resolve(` while this change was being written.
	named := map[string]bool{}
	for _, s := range sites {
		named[s.file] = true
	}
	callsStore := regexp.MustCompile(`ResolveStore\(|ResolveModeStore`)
	for _, f := range walkGoFilesForCensus(t, root) {
		rel := strings.ReplaceAll(f.rel, `\`, "/")
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "dispatch/binresolver/") {
			continue // the resolver itself, and tests
		}
		if !callsStore.MatchString(f.body) {
			continue
		}
		if !named[rel] {
			t.Errorf("%s resolves a store destination but is not named in the bin type census.\n\n"+
				"Add it, and say what it can state about the bin type it places: \"known\" (it has "+
				"the answer), \"settled\" (the order is persisted and holds its bin, so "+
				"settleBinType recovers it), or \"unknown\" (it genuinely cannot — say why, and "+
				"accept that the Allowed Bin Types fence will not apply there).", rel)
		}
	}
}

type censusFile struct {
	rel  string
	body string
}

func repoRootForCensus(t *testing.T) string {
	t.Helper()
	// dispatch/ -> shingo-core/
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func readSourceForCensus(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func walkGoFilesForCensus(t *testing.T, root string) []censusFile {
	t.Helper()
	var out []censusFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		out = append(out, censusFile{rel: rel, body: string(b)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
