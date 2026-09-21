package plantclaims

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legColumns are the four claim legs the S0 demand loop carries into the
// mirror: where a node's inbound material comes from, where its outbound goes,
// and the paired positions of a two-robot cell. Spelled as the COLUMN names,
// because what this file is about is SQL.
var legColumns = []string{
	"inbound_source",
	"outbound_destination",
	"paired_core_node",
	"second_paired_core_node",
}

// TestLegColumnsAreNotReadByTheSourceabilityPath pins the boundary between the
// two things style_claims is now for.
//
// The mirror answers ONE question today — can this (process, style) be
// sourced — and it answers it from payload_code, allowed_payload_codes and
// core_node_name. The legs are for a SECOND question, asked by the loop
// compiler: which nodes feed which. They ride in the same table because they
// arrive in the same message and are replaced wholesale with it, not because
// the sourceability verdict has anything to do with them.
//
// So a leg must not reach the recompute, and the reason is not tidiness. Two
// readers are wired to this table's CONTENT: DirtyIndex decides which (process,
// style) pairs are recomputed when a payload's stock moves, and
// loadStylesAndClaims decides the verdict itself. A leg projected into either
// one would make a changeover-position edit — which changes no payload and no
// stock — either mark styles dirty or move a verdict, and a sourceability
// verdict that moves when nothing about sourcing moved is a false alarm on an
// operator screen.
//
// A SOURCE TEST because the property is about the SELECT list, and a
// behavioural fixture can only demonstrate the cases somebody thought to
// write. What must not drift is which columns the recompute projects, and that
// is checkable directly.
//
// DirtyIndex IS READ AS A FUNCTION, not as a file, and that distinction is
// load-bearing: ReplaceProcess lives in the same file and its INSERT names
// every leg column, because writing them is this lane's whole point. A
// file-wide scan here would have read the writer as a reader and failed on the
// change it exists to permit.
func TestLegColumnsAreNotReadByTheSourceabilityPath(t *testing.T) {
	t.Parallel()

	if src := funcSource(t, "plantclaims.go", "DirtyIndex"); src == "" {
		t.Fatal("DirtyIndex not found in plantclaims.go — this guard is reading the wrong name")
	} else {
		assertNoLegColumns(t, "the dirty index (plantclaims.DirtyIndex)", src)
	}

	// The sourceability package never writes style_claims — it only reads the
	// mirror — so there its whole files are the right grain.
	for what, path := range map[string]string{
		"the recompute's claim load (sourceability.loadStylesAndClaims)": filepath.Join("..", "sourceability", "read.go"),
		"the sourcing page's per-process read":                           filepath.Join("..", "sourceability", "read_page.go"),
	} {
		b, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		assertNoLegColumns(t, what+" ("+path+")", string(b))
	}
}

func assertNoLegColumns(t *testing.T, what, src string) {
	t.Helper()
	for _, col := range legColumns {
		if strings.Contains(src, col) {
			t.Errorf("%s now names style_claims.%s.\n"+
				"The legs are the loop compiler's input, not the sourceability verdict's. A leg "+
				"projected here makes a changeover-position edit move a sourcing verdict or mark "+
				"styles dirty, neither of which has anything to do with whether the parts are "+
				"there. If a leg genuinely belongs in the verdict, that is a ruling — write it "+
				"down and change this test with it.", what, col)
		}
	}
}

// funcSource returns the source text of one top-level function in a file, or
// "" when there is no such function.
func funcSource(t *testing.T, path, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, b, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		return string(b[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
	}
	return ""
}
