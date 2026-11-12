package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lineside_part_census_test.go — the Edge does not guess what is on a spot to
// choose where a carrier goes. It aims every leg at the outbound of the claim
// the leg is for; Core holds the bin and sends a carrier of another part to its
// own claim's outbound at the lift (shingo-core dispatch's by-contents choice).
//
// The lineside part (what Core said the carrier on a node is) stays, for the
// board, the tick and the changeover's done rule. Every reader is named here
// with what it uses it for, found by parsing, so a new one that aims a leg by
// it has to arrive here and say so.

// TestCensus_NoLegIsAimedByTheLinesidePart pins the readers of the lineside
// part, and that the deleted aim is gone.
func TestCensus_NoLegIsAimedByTheLinesidePart(t *testing.T) {
	t.Parallel()
	readers := map[string]string{
		"engine/catid_holds.go:holdRows":                        "the CATID hold rows: what part stands where, shown",
		"engine/handler_bin_landed.go:HandleBinLanded":          "a landing: records what Core says the carrier is",
		"engine/handler_bin_picked_up.go:takeTicksAfterLift":    "the tick: counts against the carrier that left",
		"engine/handler_uop_adjustment.go:HandleUOPAdjustment":  "a count adjustment: against the carrier's own part",
		"engine/lineside_carrier_doorway.go:recordBoundCarrier": "the doorway's writer",
		"engine/lineside_carrier_doorway.go:recordedAs":         "the doorway's reader",
		"engine/operator_bin_ops.go:afterLoad":                  "a manual load: records the part loaded",
		"engine/operator_bin_ops.go:seatManuallyLoadedBin":      "a manual load: records the part seated",
		"engine/release_commit.go:commitMaterial":               "the Material page's move carries the carrier's own part; it is aimed at the claim's outbound",
		"engine/swap_leg_role.go:nodeStateAt":                   "the changeover's done rule: a node already holding the incoming part",
		"engine/wiring_completion.go:applyLoaderEmptyIn":        "a loader's empty in: records it",
		"engine/wiring_counter_delta.go:binAtNode":              "the tick: the carrier it counts on",
		"engine/wiring_counter_delta.go:carrierIsPart":          "the tick: whether the carrier is the claim's part",
		"engine/wiring_counter_delta.go:recordedOtherPart":      "the tick: a carrier recorded as another part",
		"engine/wiring_delivered.go:landFromOldCore":            "a delivery from a Core that sends no landing: binds it",
		"service/station_serves.go:servedHave":                  "the board: what the station has",
		"store/processes/processes.go:scanRuntime":              "the runtime row's scan",
		"domain/process.go:PartRecorded":                        "the domain's accessor",
	}
	gone := []string{"residentEvacDest", "withResidentEvacDest", "aimEvacsAtResidentHome", "ClaimForLinesidePayload"}

	var got []string
	declared := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.Walk("..", func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return perr
		}
		rel := strings.TrimPrefix(filepath.ToSlash(p), "../")
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if slices.Contains(gone, fd.Name.Name) {
				declared[fd.Name.Name] = rel
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				if se, ok := n.(*ast.SelectorExpr); ok && (se.Sel.Name == "LinesidePayloadCode" || se.Sel.Name == "LinesidePayloadKnown") {
					if site := rel + ":" + fd.Name.Name; !slices.Contains(got, site) {
						got = append(got, site)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	for name, at := range declared {
		t.Errorf("%s is declared again in %s: the Edge aims a leg at its claim's outbound, and Core sends a "+
			"carrier of another part home", name, at)
	}
	slices.Sort(got)
	var want []string
	for k := range readers {
		want = append(want, k)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		var extra, missing []string
		for _, g := range got {
			if _, ok := readers[g]; !ok {
				extra = append(extra, g)
			}
		}
		for _, w := range want {
			if !slices.Contains(got, w) {
				missing = append(missing, w)
			}
		}
		t.Errorf("the readers of the lineside part moved.\n  in the tree, not named: %v\n  named, not in the tree: %v\n"+
			"A new reader says here what it uses the part for. One that chooses where a carrier goes belongs in Core, "+
			"which holds the bin.", extra, missing)
	}
}
