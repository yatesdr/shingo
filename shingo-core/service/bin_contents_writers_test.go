package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// bin_contents_writers_test.go — every LOAD door runs the held-carrier check
// (owner, 2026-10-18: "every LOAD door (operator, admin, any importer that
// writes contents) runs the one check; a grep test that no bin-contents writer
// skips it").
//
// THE CENSUS IS OF STATEMENTS, NOT OF DOORS. A door is reached through layers;
// a statement that writes a bin's payload_code, uop_remaining or manifest is
// where the contents actually change. Every such statement in shingo-core's
// non-test source is found by parsing it, and each must be in binContentsWriters
// below with what it is: the LOAD write (which runs the check), the named
// exemption, or a write that is not a LOAD at all. A new statement fails this
// test until someone writes down which it is.

// writerKind is what a bin-contents statement is, for the held-carrier rule.
type writerKind string

const (
	// loadWrite: the one statement every LOAD door reaches. It runs the check
	// unless its caller names the exemption (heldRule).
	loadWrite writerKind = "load: runs the held check"
	// produceWrite: the produce writer, the one statement that writes a payload
	// onto a carrier a live order may hold, on purpose. The reason says why.
	produceWrite writerKind = "produce: the one exemption"
	// exemptWrite: writes a payload with no held check, reached by no plant
	// door. The reason says why.
	exemptWrite writerKind = "exempt"
	// notALoad: changes a count, or empties a carrier, without stamping a
	// payload onto it. Not a LOAD door.
	notALoad writerKind = "not a load"
)

type writerEntry struct {
	kind   writerKind
	reason string
}

// binContentsWriters is every statement in non-test shingo-core source that
// writes a bin's contents, keyed "path:declaration".
var binContentsWriters = map[string]writerEntry{
	"service/bin_manifest.go:writeProductionTx": {loadWrite,
		"the LOAD write: the loader's LOAD (LoadAtLine) and the admin Load Payload " +
			"(SetFromTemplate) pass refuseHeld and the check runs before it"},
	"store/bins/bin_manifest.go:RecordProducedTx": {produceWrite,
		"the produce writer: produce finalize (RecordProducedBin) and the produce tick's " +
			"bind record what a station made onto the carrier in hand, including one an " +
			"order is carrying away; not a person's LOAD"},
	"store/bins/bin_manifest.go:SetManifest": {exemptWrite,
		"fixtures and the dev seeder only (pinned below); no plant door reaches it"},
	"service/bin_manifest.go:emptiedCarrierSET": {notALoad,
		"empties a carrier (clear, clear-and-claim, consumed): no payload is stamped"},
	"store/bins/bins.go:RecordCount": {notALoad,
		"a count of what the carrier holds; the payload is unchanged"},
	"uop/applier.go:ApplyBinUOPDelta": {notALoad,
		"a produce or consume tick's delta on the count; the payload is unchanged"},
	"service/bin_manifest.go:syncUOPAndClaimTx": {notALoad,
		"a partial release's count, written by the order claiming the bin; payload unchanged"},
	"service/bin_manifest.go:syncOrClearForReleased": {notALoad,
		"a release's declared count (or its zero) and the payload's part list; payload unchanged"},
	"service/bin_manifest.go:stampDeclaredFullTx": {notALoad,
		"a station's RELEASE within its miscount tolerance raising the count just booked to the pack, inside the produce " +
			"writer's transaction; the payload is the one the produce write set"},
}

var (
	binUpdateSetRe = regexp.MustCompile(`(?is)\bUPDATE\s+bins\b(?:\s+(?:AS\s+)?\w+)?\s+SET\b(.*?)(?:\bWHERE\b|$)`)
	contentsColRe  = regexp.MustCompile(`(?i)\b(payload_code|uop_remaining|manifest)\s*=[^=]`)
	setFragmentRe  = regexp.MustCompile(`(?i)^\s*(payload_code|uop_remaining|manifest)\s*=[^=]`)
)

// writesBinContents reports whether a SQL string literal writes a bin's
// contents: an UPDATE bins whose SET names a contents column, or a SET fragment
// (spliced into such an UPDATE) that starts with one.
func writesBinContents(lit string) bool {
	if setFragmentRe.MatchString(lit) {
		return true
	}
	for _, m := range binUpdateSetRe.FindAllStringSubmatch(lit, -1) {
		if contentsColRe.MatchString(m[1]) {
			return true
		}
	}
	return false
}

// skippedSource is source that is not a door: migrations and the baseline DDL
// run once against the schema, not against a carrier at a window.
func skippedSource(rel string) bool {
	return strings.HasPrefix(rel, "store/migrations") || strings.HasPrefix(rel, "store/schema/")
}

type parsedFile struct {
	rel  string
	file *ast.File
}

func parseCoreSource(t *testing.T) []parsedFile {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	var out []parsedFile
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if skippedSource(rel) {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		out = append(out, parsedFile{rel: rel, file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walk shingo-core source: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("parsed only %d source files from %s; the walk did not see the module", len(out), root)
	}
	return out
}

// eachDecl visits each top-level declaration with its name: a function, or the
// const/var a value belongs to.
func eachDecl(f *ast.File, fn func(name string, node ast.Node)) {
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			fn(d.Name.Name, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						fn(n.Name, vs.Values[i])
					} else if len(vs.Values) == 1 {
						fn(n.Name, vs.Values[0])
					}
				}
			}
		}
	}
}

// TestBinContentsWriters_EveryLoadDoorRunsTheHeldCheck finds every statement
// that writes a bin's contents and holds it to binContentsWriters: a new one
// fails until it is classified, a removed one fails until its entry goes.
func TestBinContentsWriters_EveryLoadDoorRunsTheHeldCheck(t *testing.T) {
	t.Parallel()
	files := parseCoreSource(t)

	found := map[string]bool{}
	for _, pf := range files {
		eachDecl(pf.file, func(name string, node ast.Node) {
			ast.Inspect(node, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if writesBinContents(s) {
					found[pf.rel+":"+name] = true
				}
				return true
			})
		})
	}

	var unlisted, stale []string
	for k := range found {
		if _, ok := binContentsWriters[k]; !ok {
			unlisted = append(unlisted, k)
		}
	}
	for k := range binContentsWriters {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	for _, k := range unlisted {
		t.Errorf("%s writes a bin's contents and is not in binContentsWriters. If it is a LOAD "+
			"door, route it through writeProductionTx with refuseHeld; otherwise add it with the "+
			"reason it is exempt or not a load", k)
	}
	for _, k := range stale {
		t.Errorf("binContentsWriters lists %s, which no longer writes a bin's contents: delete the entry", k)
	}

	loads, produces := 0, 0
	for _, e := range binContentsWriters {
		switch e.kind {
		case loadWrite:
			loads++
		case produceWrite:
			produces++
		}
		if e.reason == "" {
			t.Errorf("an entry of kind %q carries no reason", e.kind)
		}
	}
	if loads != 1 {
		t.Errorf("%d statements are classified as the LOAD write, want exactly 1 (writeProductionTx)", loads)
	}
	if produces != 1 {
		t.Errorf("%d statements are classified as the produce writer, want exactly 1 (bins.RecordProducedTx)", produces)
	}
}

// TestBinContentsWriters_TheLoadWriteRunsTheCheck holds the LOAD write to the
// check, and the exemption to its users: writeProductionTx calls
// refuseHeldCarrierTx, producedInPlace is named only by RecordProducedBin, and
// the produce writer (bins.RecordProducedTx) is called only by writeProductionTx
// (on the producedInPlace arm) and the produce tick's bind in ApplyBinUOPDelta.
// refuseHeld is what every other caller passes (the parameter is typed, so a
// caller cannot pass neither).
func TestBinContentsWriters_TheLoadWriteRunsTheCheck(t *testing.T) {
	t.Parallel()
	files := parseCoreSource(t)

	checks := false
	exemptUsers := map[string]bool{}
	produceCallers := map[string]bool{}
	for _, pf := range files {
		eachDecl(pf.file, func(name string, node ast.Node) {
			fd, ok := node.(*ast.FuncDecl)
			if !ok {
				return
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "refuseHeldCarrierTx" &&
						pf.rel == "service/bin_manifest.go" && name == "writeProductionTx" {
						checks = true
					}
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RecordProducedTx" {
						produceCallers[pf.rel+":"+name] = true
					}
				case *ast.Ident:
					if n.Name == "producedInPlace" {
						exemptUsers[pf.rel+":"+name] = true
					}
				}
				return true
			})
		})
	}
	if !checks {
		t.Error("writeProductionTx no longer calls refuseHeldCarrierTx: every LOAD door has lost the held check")
	}
	for k := range exemptUsers {
		if k != "service/bin_manifest.go:RecordProducedBin" {
			t.Errorf("%s names producedInPlace, the produce-finalize exemption. Only RecordProducedBin "+
				"may skip the held check; a LOAD door passes refuseHeld", k)
		}
	}
	if !exemptUsers["service/bin_manifest.go:RecordProducedBin"] {
		t.Error("RecordProducedBin no longer names producedInPlace: produce finalize would refuse " +
			"a carrier its removal order holds, writing down that a fill which happened did not")
	}
	wantProduce := []string{"service/bin_manifest.go:writeProductionTx", "uop/applier.go:ApplyBinUOPDelta"}
	for k := range produceCallers {
		if k != wantProduce[0] && k != wantProduce[1] {
			t.Errorf("%s calls bins.RecordProducedTx, the produce writer, which skips the held check. "+
				"Only produce finalize and the produce tick's bind may; a LOAD goes through "+
				"writeProductionTx with refuseHeld", k)
		}
	}
	for _, k := range wantProduce {
		if !produceCallers[k] {
			t.Errorf("%s no longer calls bins.RecordProducedTx: a produce record has left the one "+
				"produce writer", k)
		}
	}
}

// TestBinContentsWriters_SetManifestIsFixturesOnly holds the exemption on
// bins.SetManifest to its reason: only the dev seeder and test fixtures call it.
func TestBinContentsWriters_SetManifestIsFixturesOnly(t *testing.T) {
	t.Parallel()
	allowed := func(rel string) bool {
		return strings.HasPrefix(rel, "cmd/seeddev/") || strings.HasPrefix(rel, "internal/testdb/") ||
			rel == "store/bin_manifest.go" // the *store.DB delegate itself
	}
	for _, pf := range parseCoreSource(t) {
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if (sel.Sel.Name == "SetBinManifest" || sel.Sel.Name == "SetManifest") && !allowed(pf.rel) {
				t.Errorf("%s calls %s, which writes a bin's contents with no held check. A LOAD door goes "+
					"through writeProductionTx with refuseHeld", pf.rel, sel.Sel.Name)
			}
			return true
		})
	}
}
