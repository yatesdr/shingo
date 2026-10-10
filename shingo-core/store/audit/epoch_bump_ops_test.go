package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// epoch_bump_ops_test.go — the enforcement half of EpochBumpOps.
//
// EpochBumpOps is a claim about code somewhere else: "these are the ops whose
// write also bumps a bin's delta_epoch". Nothing in the type system holds that,
// and the consequence of it going stale is silent and points the reassuring way.
// A missing op means CarrierBindings walks past a real binding boundary and joins
// two bindings into one, reporting an age that is too LONG — a stale-binding
// candidate manufactured out of a bookkeeping gap, on a page whose entire value
// is that its rows are worth walking to.
//
// So the set is checked against the structure it describes, the same way
// TestRampStepsMatchesTokenSet and TestCycleFlushIntervalMatchesEdge are. The
// source file is read as DATA, not imported — store/audit must not depend on
// service.

// TestEpochBumpOpsCoversEveryBumpSite pins the number of bumpEpoch call sites.
//
// It cannot prove that each site writes an op in the set — that would need to
// follow control flow — but it can make a new site impossible to add quietly.
// The failure lands on the person adding the bump, in a diff, with this comment
// in view, which is the same instrument the provenance registry uses for its
// SME exemptions.
func TestEpochBumpOpsCoversEveryBumpSite(t *testing.T) {
	path := filepath.Join("..", "..", "service", "bin_manifest.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (this test is the only thing holding EpochBumpOps to the "+
			"code it describes; if the file moved, repoint it rather than deleting it)", path, err)
	}

	// Call sites, not the declaration: every caller passes the transaction first,
	// and `func (s *BinManifestService) bumpEpoch(tx *sql.Tx` would otherwise
	// match too. The `s.` prefix is required, which is also what makes the
	// second check below meaningful.
	sites := regexp.MustCompile(`(?m)^\s*(?:if\s+)?(?:_|\w+)?(?:,\s*\w+)?\s*(?::?=\s*)?s\.bumpEpoch\(tx,`).
		FindAllIndex(src, -1)

	// SEVEN SITES — the clear-manifest site takes its op as a PARAMETER, so
	// clear_for_reuse and released_capture_empty both reach it. The sixth site
	// is the dispatch claim of a partly consumed carrier (sync_uop_and_claim); the
	// seventh is the rebase after a station's count stream went backward, which
	// writes edge_rollback. Counting sites rather than ops is deliberate: a new op
	// routed through an existing site is already covered, and it is a NEW SITE
	// that can introduce a boundary this set has never heard of.
	//
	// CONVERTED (Lane H, DECISIONS D11 (i), 2026-10-10): the two release sites
	// (released-empty, three ops; released-partial, two) are still counted, but
	// they now bump ONLY for a release that carries no stream fence (an Edge
	// that predates it), and their ops left EpochBumpOps: a fenced release
	// declares a count inside its binding, and the binding ends at the lift
	// (OpLifted). The lift's bump is not an s.bumpEpoch site at all: it rides
	// the lift's own UPDATE in store/bins (LiftToTransitTx), which
	// TestTheLiftIsTheOneUnannouncedBump below holds.
	const wantSites = 7
	if len(sites) != wantSites {
		t.Errorf("service/bin_manifest.go has %d s.bumpEpoch(tx, …) call sites; this test is "+
			"pinned at %d.\n\nIf a site was ADDED, find which audit op its function writes "+
			"and make sure that op is in EpochBumpOps — a boundary op missing from that set "+
			"makes CarrierBindings join two bindings into one and report an age that is too "+
			"long, which manufactures a stale-binding candidate. Then update this number.\n\n"+
			"If a site was REMOVED, check whether its op still belongs in the set at all.",
			len(sites), wantSites)
	}
}

// TestEveryBumpGoesThroughTheAnnouncingWrapper is the other half, and it is
// about the wire rather than the op set.
//
// s.bumpEpoch does two things that must never come apart: it ends the carrier's
// generation and it tells the station holding it. bumpEpochRaw is the bump on
// its own. Calling the raw one from a reset path would reproduce the defect this
// whole change removes — a reset that happens and is never announced, after
// which the station reports counts under a generation that has ended and Core
// discards every one, silently, for as long as nobody looks. At Hopkinsville
// that was half of all production counts.
//
// So exactly one caller: the wrapper.
func TestEveryBumpGoesThroughTheAnnouncingWrapper(t *testing.T) {
	path := filepath.Join("..", "..", "service", "bin_manifest.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Call sites of the raw bump, excluding its own declaration.
	raw := regexp.MustCompile(`(?m)^\s*(?:if\s+)?(?:_|\w+)?(?:,\s*[\w, ]+)?\s*(?::?=\s*)?bumpEpochRaw\(tx,`).
		FindAllIndex(src, -1)

	if len(raw) != 1 {
		t.Errorf("bumpEpochRaw has %d call sites, want exactly 1 (the announcing wrapper).\n\n"+
			"A reset path calling the raw bump directly bumps the generation and tells "+
			"nobody. The station goes on reporting counts stamped with the generation "+
			"that just ended, Core discards them as stale, and nothing anywhere says so. "+
			"That is the Hopkinsville count loss — 49%% of production counts, ongoing, "+
			"invisible until somebody queried the audit table.\n\n"+
			"If you need the bump without the announcement, say why at the call site and "+
			"change this test deliberately. Do not change it to make a build pass.",
			len(raw))
	}
}

// TestEpochBumpOpsIsASupersetOfTheReleaseFamily holds the relationship between
// the two op sets, which is the thing most likely to be got wrong by someone
// adding a release variant.
//
// ReleaseFamilyOps answers "was the carrier emptied" and deliberately excludes
// clear_and_claim. EpochBumpOps answers "did the count start over", and
// clear_and_claim does.
//
// CONVERTED (Lane H, DECISIONS D11 (i), 2026-10-10): it was "every release is
// a fresh start, so EpochBumpOps contains the release family". A release no
// longer ends the generation; the lift does. The containment now holds for the
// release family LESS ReleaseDeclarationOps, the release ops that declare a
// count inside the binding, and those must NOT be boundaries: one treated as a
// boundary would cut a binding at the release click, minutes before the
// carrier left. A new release variant must land in exactly one of the two.
func TestEpochBumpOpsIsASupersetOfTheReleaseFamily(t *testing.T) {
	bump := map[string]bool{}
	for _, op := range EpochBumpOps {
		if bump[op] {
			t.Errorf("EpochBumpOps lists %q twice", op)
		}
		bump[op] = true
	}
	declaration := map[string]bool{}
	for _, op := range ReleaseDeclarationOps {
		declaration[op] = true
		if bump[op] {
			t.Errorf("%q is a release declaration (D11 (i): no bump at the release) and is in "+
				"EpochBumpOps; it would cut every binding at the release click", op)
		}
	}

	for _, op := range ReleaseFamilyOps {
		if !bump[op] && !declaration[op] {
			t.Errorf("%q is in ReleaseFamilyOps but in neither EpochBumpOps nor "+
				"ReleaseDeclarationOps. Decide whether it ends the generation (a boundary "+
				"CarrierBindings must see) or declares a count inside it.", op)
		}
	}

	// The ops that are in this set and NOT in the release family, named
	// explicitly so the difference between the sets stays deliberate. lifted
	// is where a released carrier's generation ends now (D11 (i)).
	for _, op := range []string{OpSetForProduction, OpClearAndClaim, OpSyncUOPAndClaim, OpLifted} {
		if !bump[op] {
			t.Errorf("%q is missing from EpochBumpOps. It is not an unload — which is why "+
				"ReleaseFamilyOps excludes it — but it does start the count over.", op)
		}
	}

	// AND THE ONES THAT MUST NOT BE HERE. cycle_count is the load-bearing
	// exclusion: BinService.RecordCount corrects the count INSIDE a binding and
	// does not bump the epoch. Treating it as a boundary would cut the longest
	// binding in the Springfield dump (bin 27, 22.99 days, recounted in the
	// middle) into two shorter ones and hide it from the candidate list entirely.
	for _, op := range []string{
		OpCycleCount, OpManifestConfirmed, OpManifestUnconfirmed, OpStaleEpochDropped, OpPayloadMismatchDropped,
		OpPayloadBoundFirstDelta, OpPayloadReboundWithInventory,
		OpOperatorOverrideReleasePartial, OpOperatorOverridePullParts, OpAfterLift,
	} {
		if bump[op] {
			t.Errorf("%q is in EpochBumpOps and does not bump the epoch. A non-boundary op "+
				"treated as a boundary SHORTENS every binding it appears in, which hides "+
				"real stale bindings — the failure that points the reassuring way.", op)
		}
	}
}

// TestTheLiftIsTheOneUnannouncedBump holds the named exception to "bump and
// announce are inseparable" (Lane H, DECISIONS D11 (i), 2026-10-10).
//
// Every generation bump in Core is one SQL fragment, delta_epoch=delta_epoch+1.
// Two statements carry it: bumpEpochRaw, reached only through the announcing
// wrapper (TestEveryBumpGoesThroughTheAnnouncingWrapper), and the lift's own
// UPDATE in store/bins (LiftToTransitTx). The lift does NOT broadcast the A4
// uop_adjustment: the bin is at _TRANSIT, which no station models, and the
// pickup message — written in the same transaction, addressed to the station,
// carrying the new Epoch — IS the announcement. A third statement carrying the
// fragment is a bump nobody hears of, which is the Hopkinsville defect.
//
// Expiry: the lift starts announcing through bumpEpoch, or the bump leaves the
// lift; either way this table changes in the same commit.
func TestTheLiftIsTheOneUnannouncedBump(t *testing.T) {
	want := map[string]int{
		"service/bin_manifest.go": 1, // bumpEpochRaw, behind the announcing wrapper
		"store/bins/bins.go":      1, // LiftToTransitTx: the pickup message is the announcement
	}
	bumpRE := regexp.MustCompile(`delta_epoch\s*=\s*delta_epoch\s*\+`)
	root := filepath.Join("..", "..")
	got := map[string]int{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if n := len(bumpRE.FindAllIndex(src, -1)); n > 0 {
			rel, _ := filepath.Rel(root, path)
			got[filepath.ToSlash(rel)] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for f, n := range got {
		if want[f] != n {
			t.Errorf("%s carries %d generation bump(s) (delta_epoch=delta_epoch+…), want %d. A bump outside "+
				"bumpEpoch tells no station; the lift is the one named exception (its pickup message is the "+
				"announcement). Route a new bump through s.bumpEpoch.", f, n, want[f])
		}
	}
	for f, n := range want {
		if got[f] != n {
			t.Errorf("%s carries %d generation bump(s), want %d: the census no longer matches the code", f, got[f], n)
		}
	}
}
