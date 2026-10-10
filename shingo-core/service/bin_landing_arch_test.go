package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestArch_EveryArrivalWriterAnnouncesOrIsNamed — Lane H, H.2 (BRIEF-
// unification-C2c 2026-10-10; DESIGN-H §3): every Core writer that puts a bin
// at a node announces the landing through the one verb (announceLanding, or
// its hook form, OnPlaced / LandingHook) in the transaction that moves the bin,
// or is named here with the reason it does not.
//
// Two halves. The CENSUS walks every non-test Core file for a statement that
// writes bins.node_id (or inserts a bin), and fails on one whose function is
// not in the table: a new arrival writer cannot arrive without somebody
// deciding whether its seat hears of it. The CALL check reads each announcing
// writer's body for the call that announces, so deleting the call fails here
// rather than in a sim.
//
// Expiry: a writer whose disposition changes (a named one starts announcing,
// or the verb moves) edits its row in the same change.
func TestArch_EveryArrivalWriterAnnouncesOrIsNamed(t *testing.T) {
	t.Parallel()

	type writer struct {
		// calls is the text whose presence in the function body is the
		// announcement; empty for a named writer.
		calls string
		// why is the reason, for every row.
		why string
	}
	// The statements that write where a bin is, by function.
	writers := map[string]writer{
		// ── Writers whose placement is announced ──
		"store/internal/helpers/place_bin.go:PlaceBinTx": {
			why: "THE placement; it returns what its UPDATE moved (helpers.Placed), and every caller announces it (placeCallers below)"},
		"store/bins/bins.go:move": {calls: "onPlaced(tx, placed)",
			why: "the hand move's statement; MoveByHandAnnounced passes the verb (BinService.Move, so MoveByHand and the engine's carrier parking, onto a synthetic deck node where nothing is sent). MoveAndClearStaging / MoveOffTransit pass nil and have no production caller"},
		"store/bins/bins.go:RecoverToNode": {calls: "onPlaced(tx, placed)",
			why: "the recovery's statement; RecoverTransitAnomaly passes the verb: a person placed the found carrier, and it told nobody before"},

		// ── Named, not announced ──
		"store/internal/helpers/helpers.go:EvictStaleGhostBinsTx": {
			why: "moves a ghost OFF the node to _TRANSIT, inside the placement whose landing then overwrites the seat (a different carrier writes)"},
		"store/bins/bins.go:Retire":        {why: "node_id=NULL: a retire is no arrival"},
		"store/bins/bins.go:MoveToTransit": {why: "the old lift, to _TRANSIT; no production caller since Lane H D11 (BinService.Lift); test fixtures"},
		"store/bins/bins.go:LiftToTransitTx": {
			why: "the lift, to _TRANSIT, with the generation bump of a streamed bin (Lane H D11 (i)); the pickup message in its transaction is its announcement (BinService.Lift)"},
		"store/bins/bins.go:TakeOffDeck": {why: "a deck read empty moves the bin to _TRANSIT; no seat"},
		"store/bins/bins.go:Create": {
			why: "creating a bin record is not a carrier landing (admin create; the seeder seeds the Edge directly). DESIGN-H §3 records an admin creating a bin at a line seat as a gap"},
		"service/bin_service.go:CreateBatch": {why: "as Create"},
	}

	// Every caller of the placement primitive, and the call that announces
	// what it placed; plus the callers that hand the verb to a store writer.
	placeCallers := map[string]string{
		"service/bin_service.go:applyArrival":                       "s.announceLanding(",
		"service/bin_service.go:PlaceObserved":                      "s.announceLanding(",
		"store/order_bins.go:ApplyMultiBinArrival":                  "onPlaced(tx, placed)",
		"store/recovery/recovery.go:RepairConfirmedOrderCompletion": "onPlaced(tx, placed)",
		"store/order_bins.go:PlaceBinTx":                            "return helpers.PlaceBinTx(tx, p)",
	}
	callers := map[string]string{
		"service/bin_service.go:Move":                       "s.announceLanding(",
		"service/bin_service.go:RecoverTransitAnomaly":      "s.announceLanding(",
		"service/bin_landing.go:ApplyMultiBinArrival":       "s.LandingHook(",
		"engine/recovery_service.go:ReapplyOrderCompletion": "LandingHook(",
	}

	root := filepath.Join("..")
	writeRE := regexp.MustCompile(`(?is)(update\s+bins\b[^` + "`" + `]{0,600}?\bnode_id\s*=)|(insert\s+into\s+bins\s*\()`)
	found := map[string]bool{}
	bodies := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == "schema" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return perr
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			key := rel + ":" + fn.Name.Name
			bodies[key] = body
			if writeRE.MatchString(body) {
				found[key] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !found["store/internal/helpers/place_bin.go:PlaceBinTx"] {
		t.Fatal("the census did not find the placement primitive's own node_id write; the needle no longer matches and every assertion here is vacuous")
	}

	var unlisted []string
	for key := range found {
		if _, ok := writers[key]; !ok {
			unlisted = append(unlisted, key)
		}
	}
	sort.Strings(unlisted)
	for _, key := range unlisted {
		t.Errorf("%s writes where a bin is and is not in the landing census. Announce through "+
			"the one verb (service.BinService.announceLanding, or OnPlaced from the store), in the "+
			"move's transaction, or name it here with the reason its seat is not told (Lane H, H.2).", key)
	}
	for key, w := range writers {
		if !found[key] {
			t.Errorf("%s is in the landing census but no longer writes a bin's node; remove its row "+
				"or repoint it", key)
			continue
		}
		if w.calls != "" && !strings.Contains(bodies[key], w.calls) {
			t.Errorf("%s no longer calls %q: a carrier it places is not announced (Lane H, H.2)", key, w.calls)
		}
	}
	placeRE := regexp.MustCompile(`\bPlaceBinTx\(`)
	for key, body := range bodies {
		if key == "store/internal/helpers/place_bin.go:PlaceBinTx" || !placeRE.MatchString(body) {
			continue
		}
		call, ok := placeCallers[key]
		if !ok {
			t.Errorf("%s places a bin through PlaceBinTx and is not in the landing census: announce "+
				"what it placed (announceLanding / OnPlaced) in its transaction, or name it (Lane H, H.2)", key)
			continue
		}
		if !strings.Contains(body, call) {
			t.Errorf("%s no longer calls %q: a carrier it places is not announced (Lane H, H.2)", key, call)
		}
	}
	for key := range placeCallers {
		if !placeRE.MatchString(bodies[key]) {
			t.Errorf("%s is listed as a PlaceBinTx caller and no longer calls it; remove its row", key)
		}
	}
	for key, call := range callers {
		if !strings.Contains(bodies[key], call) {
			t.Errorf("%s no longer passes the landing verb (%q)", key, call)
		}
	}
	// The engine reaches the multi-bin settle only through BinService, which
	// passes the verb; a direct store call would settle unannounced.
	for key, body := range bodies {
		if strings.HasPrefix(key, "engine/") && strings.Contains(body, "e.db.ApplyMultiBinArrival(") {
			t.Errorf("%s calls the store's multi-bin settle directly; go through "+
				"BinService.ApplyMultiBinArrival so each bin's landing is written", key)
		}
	}
}
