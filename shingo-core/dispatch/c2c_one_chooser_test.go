package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// c2c_one_chooser_test.go — C2c (Lane C), P1 (b) of DESIGN-C2c §9: the
// structural half of "order 266's shape passes with no widen arm".
//
// C2c ruling 1: one spelling for "choose a drop in this group" — the hard rules,
// then the group's algorithm — called at acquiring and at release. DESIGN-C2c
// §7.1 names it plan.Planner.ChooseDrop; §7.3 deletes the release's second and
// third spellings: widenDropoffToGroup, widenRefusedDropoff, refusalCanWiden and
// the in-lane re-bind (rebindGatedDropoff's own FindStoreSlotInLaneExcluding).
// D13 Q-NAMED keeps a person-named drop lane-scoped; that is a narrower ask of
// the same chooser, not a second selector.
//
// Parsed, not grepped: a comment that names a function is not a function, and a
// line number moves with every edit above it.
//
// RED AT BASE (run 2026-10-10): the three widen functions exist, the re-bind
// calls FindStoreSlotInLaneExcluding, the widen calls Resolve with
// ResolveModeStore, and nothing in package dispatch calls ChooseDrop.

// c2cDispatchSources parses every non-test .go file of package dispatch itself
// (not its subpackages).
func c2cDispatchSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files[n] = f
	}
	return fset, files
}

// TestC2cPin_OneChooserNoWidenArm: the release reaches its slot through one
// chooser.
func TestC2cPin_OneChooserNoWidenArm(t *testing.T) {
	t.Parallel()
	_, files := c2cDispatchSources(t)
	retired := map[string]bool{"widenDropoffToGroup": true, "widenRefusedDropoff": true, "refusalCanWiden": true}

	var declared, laneSelector, storeResolve, chooseDrop []string
	for name, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if retired[fn.Name.Name] {
				declared = append(declared, name+":"+fn.Name.Name)
			}
			if fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(x ast.Node) bool {
				call, ok := x.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				site := name + ":" + fn.Name.Name
				switch sel.Sel.Name {
				case "FindStoreSlotInLaneExcluding":
					laneSelector = append(laneSelector, site)
				case "Resolve", "ResolveStore":
					for _, a := range call.Args {
						if s, ok := a.(*ast.SelectorExpr); ok && s.Sel.Name == "ResolveModeStore" {
							storeResolve = append(storeResolve, site)
						}
					}
				case "ChooseDrop":
					chooseDrop = append(chooseDrop, site)
				}
				return true
			})
		}
	}
	slices.Sort(declared)
	slices.Sort(laneSelector)
	slices.Sort(storeResolve)

	if len(declared) != 0 {
		t.Errorf("the widen arm is still declared: %v. C2c ruling 1 leaves one spelling for choosing a drop "+
			"in a group; DESIGN-C2c §7.3 deletes these", declared)
	}
	if len(laneSelector) != 0 {
		t.Errorf("package dispatch still asks the lane selector directly at %v: a release that re-binds "+
			"inside its pencilled lane is a second chooser (the in-lane re-bind, DESIGN-C2c §1 #10)", laneSelector)
	}
	if len(storeResolve) != 0 {
		t.Errorf("package dispatch still resolves a store into a group itself at %v: a third chooser "+
			"(the widen's Resolve, DESIGN-C2c §1 #11)", storeResolve)
	}
	if len(chooseDrop) == 0 {
		t.Errorf("nothing in package dispatch calls ChooseDrop: the release does not reach its slot through " +
			"the one chooser (plan.Planner.ChooseDrop, DESIGN-C2c §7.1)")
	}
}

// TestC2cPin_EveryDropInAGroupGoesThroughChooseDrop is the orc's ruling of
// 2026-10-10 evening (RULINGS-C2c-run-through §3.2): ONE chooser. Every choice
// of a drop in a group — the doors' StoreDestination, the keeper's and the
// cancel-return's group store, the acquiring settle and the dug-lane re-aim —
// goes through plan.Planner.ChooseDrop, so no second spelling of "where in this
// group does this carrier go" survives beside it.
//
// Asked of the planner package (where every group store is chosen) and package
// dispatch (where the doors ask): outside ChooseDrop itself, nothing calls the
// resolver's store arm (Resolve with ResolveModeStore, ResolveStore,
// ResolveStoreExcludingLanes, RankStoreExcludingLanes), and the planner's store
// entries (StoreDestination, MaintainedSlot, GroupStoreSlot) reach their answer
// through ChooseDrop.
//
// RED AT BASE (56a702c6): StoreDestination calls Resolve with ResolveModeStore
// and groupStore calls GroupResolver.ResolveStore, beside ChooseDrop.
func TestC2cPin_EveryDropInAGroupGoesThroughChooseDrop(t *testing.T) {
	t.Parallel()
	type site struct{ file, fn string }
	sources := map[string]map[string]*ast.File{}
	_, sources["dispatch"] = c2cDispatchSources(t)
	sources["plan"] = c2cPackageSources(t, "plan")

	chooser := map[string]bool{"ChooseDrop": true}
	var second []string
	calls := map[string]map[string]bool{} // planner function → the selectors it calls
	for pkg, files := range sources {
		for name, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				at := site{pkg + "/" + name, fn.Name.Name}
				ast.Inspect(fn.Body, func(x ast.Node) bool {
					call, ok := x.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if pkg == "plan" {
						if calls[fn.Name.Name] == nil {
							calls[fn.Name.Name] = map[string]bool{}
						}
						calls[fn.Name.Name][sel.Sel.Name] = true
					}
					storeArm := false
					switch sel.Sel.Name {
					case "ResolveStore", "ResolveStoreExcludingLanes", "RankStoreExcludingLanes":
						storeArm = true
					case "Resolve":
						for _, a := range call.Args {
							if s, ok := a.(*ast.SelectorExpr); ok && s.Sel.Name == "ResolveModeStore" {
								storeArm = true
							}
						}
					}
					if storeArm && !(pkg == "plan" && chooser[at.fn]) {
						second = append(second, at.file+":"+at.fn+"→"+sel.Sel.Name)
					}
					return true
				})
			}
		}
	}
	slices.Sort(second)
	if len(second) != 0 {
		t.Errorf("a drop in a group is chosen outside ChooseDrop at %v: a second chooser beside the one "+
			"(RULINGS-C2c-run-through §3.2: every choice of a drop in a group goes through ChooseDrop)", second)
	}
	// Reached through the planner's own calls: StoreDestination asks StoreDrop,
	// which asks ChooseDrop.
	var reaches func(fn string, seen map[string]bool) bool
	reaches = func(fn string, seen map[string]bool) bool {
		if seen[fn] {
			return false
		}
		seen[fn] = true
		for callee := range calls[fn] {
			if chooser[callee] || reaches(callee, seen) {
				return true
			}
		}
		return false
	}
	for _, entry := range []string{"StoreDestination", "groupStore"} {
		if !reaches(entry, map[string]bool{}) {
			t.Errorf("plan.Planner.%s does not reach its answer through ChooseDrop", entry)
		}
	}
}

// c2cPackageSources parses every non-test .go file of the package in dir.
func c2cPackageSources(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, dir+"/"+n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, n, err)
		}
		files[n] = f
	}
	return files
}
