package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// --- test-only-doors ---------------------------------------------------------
//
// An exported store door that nothing but tests calls is named in doorList
// with the reason it stays, or it goes:
//
//	door shingo-core/store DB.FailOrderAtomic <why a door only tests call stays>
//
// WHY. A door kept for tests drifts from the product: a test that reaches a
// state through a door no product path uses builds a state no plant can have
// (study round 3, docket D27: Core's UpdateOrderStatus had 33 test calls and
// no product caller, while every product status move went through the
// lifecycle's compare-and-swap write). Deleting the old door when its last
// product caller goes makes a new use stop compiling; this rule notices the
// door that was left behind, and a new one written for a test.
//
// WHAT IS READ. The doors: the exported methods on DB declared in the non-test
// .go files of each store package (shingo-core/store, shingo-edge/store), and
// the exported top-level funcs of the packages under it, internal/ excepted.
// Their callers: every reference in the .go files of the door's module and of
// integration, as a selector X.Name (a call or a method value; for a package
// func, X names an import of its package), or, for a package func, its bare
// name in its own package. A reference is a test's in a _test.go file, in a
// test-support package (doorTestSupport) or in integration, and a product's
// anywhere else, under any build tag.
//
// WHAT IT CANNOT SEE. A selector's receiver type is not resolved, so a call of
// another type's method that shares a facade door's name reads as the door's,
// and that door never fires; the ok line counts the doors whose name another
// declaration in their module shares. A door reached only by reflection or a
// template reads as test-only.
//
// WHAT FIRES: an unlisted door with no product reference; a listed door that
// a product file references now, or that is declared no more (the line goes);
// a line not of the list's shape, or naming a door twice.
//
// HOW IT SHRINKS. The list began as the doors test-only at the rule's start,
// each marked doorUnaudited where nothing past "only tests call it" was
// established. A door leaves when it is deleted (its tests moved to the seam
// the product uses, AGENTS.md's table) or gains a product caller; a door kept
// on purpose says why in place of the mark.
const doorList = "shared/scripts/repolint/test-only-doors.txt"

// doorUnaudited starts a listed reason that says no more than that only tests
// call the door.
const doorUnaudited = "UNAUDITED"

// doorStores are the store packages whose doors are read, one per module.
var doorStores = []string{"shingo-core/store", "shingo-edge/store"}

// doorTestSupport are the non-test packages that serve tests only: a reference
// from one is a test's.
var doorTestSupport = []string{
	"shingo-core/internal/testdb/", "shingo-core/testharness/",
	"shingo-edge/internal/testdb/", "shingo-edge/testharness/",
}

// storeDoor is one exported door and who references it.
type storeDoor struct {
	dir, name  string // name is "DB.Method" for the facade, "Func" for a package func
	file       string
	line       int
	prod, test int
	pos        token.Pos // the declaration's name, which is no reference
}

// parsedGo is one parsed file with its file set.
type parsedGo struct {
	f    *file
	af   *ast.File
	fset *token.FileSet
}

// doorIndex is the store doors, looked up the two ways a reference names one,
// and every function name the modules declare.
type doorIndex struct {
	doors    map[string]*storeDoor   // "<dir> <name>"
	facade   map[string][]*storeDoor // "<module> <Method>"
	pkgFunc  map[string]*storeDoor   // "<dir> <Func>"
	declared map[string]int          // "<module> <name>" -> declarations
}

func testOnlyDoors(t *tree) result {
	listed, fs, found := readList(t, doorList, map[string]listShape{"door": {2, true}})
	if !found {
		return result{findings: fs}
	}
	all, bad := parseDoorFiles(t)
	ix := indexDoors(all)
	modPath := goModulePaths(t)
	for _, p := range all {
		ix.countReferences(p, modPath)
	}
	return ix.check(listed, append(fs, bad...))
}

// goModulePaths maps each module's path, as an import spells it, to its
// directory, so an import names a directory.
func goModulePaths(t *tree) map[string]string {
	modPath := map[string]string{}
	for _, f := range t.files {
		if dir, ok := strings.CutSuffix(f.path, "/go.mod"); ok && f.data != nil && !strings.Contains(dir, "/") {
			for _, l := range strings.Split(string(f.data), "\n") {
				if p, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
					modPath[strings.TrimSpace(p)] = dir
				}
			}
		}
	}
	return modPath
}

// parseDoorFiles parses the .go files of the store modules and integration, in
// path order.
func parseDoorFiles(t *tree) ([]parsedGo, []Finding) {
	var files []*file
	for _, f := range moduleGo(t) {
		top, _, _ := strings.Cut(f.path, "/")
		if top == "shingo-core" || top == "shingo-edge" || top == "integration" {
			files = append(files, f)
		}
	}
	var all []parsedGo
	bad := parseAll(files, func(f *file, af *ast.File, fset *token.FileSet) {
		all = append(all, parsedGo{f, af, fset})
	})
	sort.Slice(all, func(i, j int) bool { return all[i].f.path < all[j].f.path })
	return all, bad
}

// indexDoors finds the doors, and counts every function name declared.
func indexDoors(all []parsedGo) *doorIndex {
	ix := &doorIndex{doors: map[string]*storeDoor{}, facade: map[string][]*storeDoor{},
		pkgFunc: map[string]*storeDoor{}, declared: map[string]int{}}
	for _, p := range all {
		mod, _, _ := strings.Cut(p.f.path, "/")
		dir := path.Dir(p.f.path)
		isTest := strings.HasSuffix(p.f.path, "_test.go")
		for _, d := range p.af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ix.declared[mod+" "+fd.Name.Name]++
			if isTest || !fd.Name.IsExported() {
				continue
			}
			var sd *storeDoor
			switch {
			case slices.Contains(doorStores, dir) && fd.Recv != nil && len(fd.Recv.List) == 1 && typeName(fd.Recv.List[0].Type) == "DB":
				sd = &storeDoor{dir: dir, name: "DB." + fd.Name.Name}
				ix.facade[mod+" "+fd.Name.Name] = append(ix.facade[mod+" "+fd.Name.Name], sd)
			case fd.Recv == nil && hasPrefixAny(dir, storePrefixes()) && !strings.Contains(dir+"/", "/internal/"):
				sd = &storeDoor{dir: dir, name: fd.Name.Name}
				ix.pkgFunc[dir+" "+fd.Name.Name] = sd
			default:
				continue
			}
			sd.file, sd.line, sd.pos = p.f.path, p.fset.Position(fd.Name.Pos()).Line, fd.Name.Pos()
			ix.doors[sd.dir+" "+sd.name] = sd
		}
	}
	return ix
}

// countReferences counts, against each door it names, every reference in
// one file: a test's or a product's by where the file is.
func (ix *doorIndex) countReferences(p parsedGo, modPath map[string]string) {
	mod, _, _ := strings.Cut(p.f.path, "/")
	mods := []string{mod}
	if mod == "integration" {
		mods = []string{"shingo-core", "shingo-edge"}
	}
	test := mod == "integration" || strings.HasSuffix(p.f.path, "_test.go") || hasPrefixAny(p.f.path, doorTestSupport)
	count := func(d *storeDoor) {
		if test {
			d.test++
		} else {
			d.prod++
		}
	}
	imports := map[string]string{} // local name -> directory
	for _, im := range p.af.Imports {
		ip, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		for prefix, dir := range modPath {
			if rest, ok := strings.CutPrefix(ip, prefix+"/"); ok {
				name := path.Base(ip)
				if im.Name != nil {
					name = im.Name.Name
				}
				imports[name] = dir + "/" + rest
			}
		}
	}
	dir := path.Dir(p.f.path)
	// A selector's name is counted as a selector; its X is walked again, and
	// any other identifier is a bare name (its own declaration excepted).
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			for _, m := range mods {
				for _, d := range ix.facade[m+" "+x.Sel.Name] {
					count(d)
				}
			}
			if id, ok := x.X.(*ast.Ident); ok {
				if d := ix.pkgFunc[imports[id.Name]+" "+x.Sel.Name]; d != nil {
					count(d)
				}
			}
			ast.Inspect(x.X, visit)
			return false
		case *ast.Ident:
			if d := ix.pkgFunc[dir+" "+x.Name]; d != nil && !(p.f.path == d.file && x.Pos() == d.pos) {
				count(d)
			}
		}
		return true
	}
	ast.Inspect(p.af, visit)
}

// check holds the counted doors to the list.
func (ix *doorIndex) check(listed []listEntry, fs []Finding) result {
	seen := map[string]int{}
	var unaudited int
	for _, e := range listed {
		key := e.place[0] + " " + e.place[1]
		if first, dup := seen[key]; dup {
			fs = append(fs, Finding{File: doorList, Line: e.line, Why: fmt.Sprintf("names %s again (first on line %d)", key, first)})
			continue
		}
		seen[key] = e.line
		if e.reason == doorUnaudited || strings.HasPrefix(e.reason, doorUnaudited+":") {
			unaudited++
		}
		switch d := ix.doors[key]; {
		case d == nil:
			fs = append(fs, Finding{File: doorList, Line: e.line, Why: "door " + key + ": no exported store door of that name is declared, so the line goes"})
		case d.prod > 0:
			fs = append(fs, Finding{File: doorList, Line: e.line,
				Why: fmt.Sprintf("door %s: %d product reference(s) now, so the line goes", key, d.prod)})
		}
	}
	var shared, testOnly int
	for key, d := range ix.doors {
		mod, _, _ := strings.Cut(d.dir, "/")
		if method, ok := strings.CutPrefix(d.name, "DB."); ok && ix.declared[mod+" "+method] > 1 {
			shared++ // a package func's references resolve through imports, so only the facade's can be shared
		}
		if d.prod > 0 {
			continue
		}
		testOnly++
		if seen[key] > 0 {
			continue
		}
		why := fmt.Sprintf("%s has no caller outside tests (%d test reference(s))", d.name, d.test)
		if d.test == 0 {
			why = d.name + " has no caller at all"
		}
		fs = append(fs, Finding{File: d.file, Line: d.line, Why: why})
	}
	return result{fs, fmt.Sprintf("%d exported store doors, %d called by tests only, each listed (%d of them %s); %d facade doors share a name another declaration in their module has, so a call of it cannot be told from the door's",
		len(ix.doors), testOnly, unaudited, doorUnaudited, shared)}
}

// storePrefixes are the directories under each store package.
func storePrefixes() []string {
	out := make([]string, len(doorStores))
	for i, s := range doorStores {
		out[i] = s + "/"
	}
	return out
}
