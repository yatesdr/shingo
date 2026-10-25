package bins_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// count_spelling_census_test.go — ground rule 7, "a count is a claim; counted to
// nothing has one spelling", as a census of the tree.
//
// A carrier's count (bins.uop_remaining, the bin row's UOPRemaining) compared
// with 0 says "this carrier holds stock" or "it holds none". The stock fragment
// and the bins feed's Stocked say it as `> 0`; other sites say `<= 0`, `= 0`,
// `<> 0` or `COALESCE(…, 0) > 0`, and the last three disagreed with the first
// about a negative count, an overpack. Since phase F of the press-tranche-2
// brief it is one function each side: domain.Bin.Stocked in Go and
// helpers.StockSQL(alias) in SQL (bins.StockSQL outside store/); the Edge reads
// the bins feed's Stocked, which binRow derives from Stocked.
//
// THE UNIT IS A SITE: a file and the declaration holding the comparison (Func,
// Recv.Method, or a const, var or type by its name), found on go/ast over every
// non-test .go file of the five modules: Go comparisons, and SQL in Go strings,
// a `+` chain of literals read as one text (an operand that is no literal reads
// as `x`), so a fragment split around an alias is read whole. A comment naming
// a comparison is no site, and no line number is in the key.
//
// Each fact has two lists, both exact, and no third: the one function is the
// fact's only spelling. Its other questions compare the same column with 0 to
// ask something else, each with its reason. A site on neither list fails as a
// new spelling, and a listed site the tree no longer has fails as stale. (Until
// phase F the spellings were a third list, committed red in testdb.VerifyRed's
// form; F folded them.)
//
// The second fact is "is this a tote type" (finch-4300 A7): totes per layer
// stated. It is protocol.IsToteType beside ToteLayers, and in SQL the null
// test, `totes_per_layer IS [NOT] NULL`, which is therefore no spelling. The
// `<= 0` and `> 0` arms it folded were dead under CHECK (totes_per_layer > 0).

// countOtherQuestions compare the count with 0 to ask something else.
var countOtherQuestions = map[string]string{
	"shingo-core/service/bin_manifest.go:emptiedCarrierSET":            "a write (SET uop_remaining=0), not a comparison",
	"shingo-core/store/bins/ledger_integrity.go:OpenNegativeBins":      "the overpack finding: a count below 0 is the finding, not the stock question",
	"shingo-core/www/material_flags_view.go:ClassifyLedger":            "the ledger's reading (settled, or binloads below 0): the overpack question",
	"shingo-edge/engine/sim_operator.go:simOperator.clearNegativeBins": "the sim sweeps overpacked market bins: the overpack question",
	// Judged and named in phase F (the brief's "judged and named").
	"shingo-core/store/lineside_divergence.go:DB.LinesideCoreSide": "`<> 0`: the reconciliation asks whether Core holds a count at a seat that the Edge's " +
		"report must answer for, an overpack included — a count to compare, not stock",
	"shingo-edge/engine/sim_operator.go:simOperator.consumeDisposition": "the sim build's floor bands the oracle's count (underpack at or below 0, the lineside " +
		"pull, a partial back): test tooling reading Core's bin JSON over HTTP, where the Edge's one reader of stock is the feed's Stocked",
}

// toteOtherQuestions compare totes per layer with 0 or nil to ask something else.
var toteOtherQuestions = map[string]string{
	"shingo-core/store/schema/postgres_ddl.go:postgresDDL": "the CHECK (totes_per_layer > 0) that makes every `<= 0` arm dead: the schema, not a reader",
}

// TestRule7_CountedToNothingHasOneSpelling is ground rule 7's census.
func TestRule7_CountedToNothingHasOneSpelling(t *testing.T) {
	t.Parallel()
	judgeCensus(t, "count", siteCensus(t, countGoSite, countSQL, isTheOneCount), countOtherQuestions)
}

// TestRule7_ToteTypeHasOneSpelling is the census's second list (A7).
func TestRule7_ToteTypeHasOneSpelling(t *testing.T) {
	t.Parallel()
	judgeCensus(t, "tote type", siteCensus(t, toteGoSite, toteSQL, isTheOneToteType), toteOtherQuestions)
}

// judgeCensus fails on a site that is not on the other-questions list, and on a
// listed site the tree no longer has.
func judgeCensus(t *testing.T, what string, got map[string][]string, other map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if _, ok := other[k]; ok {
			continue
		}
		t.Errorf("rule 7: %s [%s] is a second spelling of the %s fact.\n\n"+
			"Compose the one function (domain.Bin.Stocked and helpers.StockSQL for the count, "+
			"protocol.IsToteType and the null test for the tote type), or, if the site asks a "+
			"different question, name it in the other-questions list with the reason.",
			k, strings.Join(got[k], "; "), what)
	}
	for k, why := range other {
		if _, ok := got[k]; !ok {
			t.Errorf("rule 7: %s (%s) is listed, but the tree no longer compares the %s there: "+
				"delete its line, or repoint it if the site moved", k, why, what)
		}
	}
}

// countRE is the count compared with 0 in SQL: the column, or the column under
// COALESCE(…, 0), on either side of the operator. SUM(uop_remaining) is an
// aggregate, not a carrier's count, and does not match.
var countRE = regexp.MustCompile(`(?i)(?:coalesce\(\s*(?:\w+\.)?uop_remaining\s*,\s*0\s*\)|\b(?:\w+\.)?uop_remaining)\s*(?:<>|!=|<=|>=|=|<|>)\s*0\b|\b0\s*(?:<>|!=|<=|>=|=|<|>)\s*(?:\w+\.)?uop_remaining\b`)

// toteRE is totes per layer compared with 0 in SQL, not as the divisor of a
// modulo: the leftover arm's `% totes_per_layer <> 0` is arithmetic, not a test
// of the type.
var toteRE = regexp.MustCompile(`(?i)(%\s*)?\b(?:\w+\.)?totes_per_layer\s*(?:<>|!=|<=|>=|=|<|>)\s*0\b`)

// countSQL returns the offset of each count comparison in text.
func countSQL(text string) []int {
	var at []int
	for _, m := range countRE.FindAllStringIndex(text, -1) {
		at = append(at, m[0], m[1])
	}
	return at
}

// toteSQL returns the offset of each tote-type comparison in text.
func toteSQL(text string) []int {
	var at []int
	for _, m := range toteRE.FindAllStringSubmatchIndex(text, -1) {
		if m[2] < 0 {
			at = append(at, m[0], m[1])
		}
	}
	return at
}

// countGoSite is a comparison of a carrier's count with 0: UOPRemaining (the
// bin row's field on both modules) or binsource.Cand's UOP, the same column.
func countGoSite(e *ast.BinaryExpr) bool {
	return isComparison(e.Op) &&
		(isNamed(e.X, "UOPRemaining", "UOP") && isLit(e.Y, "0") ||
			isNamed(e.Y, "UOPRemaining", "UOP") && isLit(e.X, "0"))
}

// toteGoSite is totes per layer compared with nil or 0, dereferenced or not.
func toteGoSite(e *ast.BinaryExpr) bool {
	tote := func(x ast.Expr) bool { return isNamed(x, "TotesPerLayer", "totesPerLayer") }
	other := func(x ast.Expr) bool { return isLit(x, "0") || isNil(x) }
	return isComparison(e.Op) && (tote(e.X) && other(e.Y) || tote(e.Y) && other(e.X))
}

// isTheOneCount exempts the one function each side: domain.Bin.Stocked, and
// helpers.StockSQL.
func isTheOneCount(rel, decl string) bool {
	return rel == "shingo-core/domain/bin.go" && decl == "Bin.Stocked" ||
		rel == "shingo-core/store/internal/helpers/bin_sourceable.go" && decl == "StockSQL"
}

// isTheOneToteType exempts protocol.IsToteType, beside ToteLayers.
func isTheOneToteType(rel, decl string) bool {
	return rel == "protocol/carrier.go" && decl == "IsToteType"
}

func isComparison(op token.Token) bool {
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

// isNamed is an identifier or a selector's last name, through parens and a
// pointer dereference.
func isNamed(x ast.Expr, names ...string) bool {
	switch v := x.(type) {
	case *ast.ParenExpr:
		return isNamed(v.X, names...)
	case *ast.StarExpr:
		return isNamed(v.X, names...)
	case *ast.Ident:
		return slices.Contains(names, v.Name)
	case *ast.SelectorExpr:
		return slices.Contains(names, v.Sel.Name)
	}
	return false
}

func isLit(x ast.Expr, val string) bool {
	l, ok := x.(*ast.BasicLit)
	return ok && l.Kind == token.INT && l.Value == val
}

func isNil(x ast.Expr) bool {
	id, ok := x.(*ast.Ident)
	return ok && id.Name == "nil"
}

// siteCensus walks every non-test .go file of the five modules (legacy
// migrations exempt by file, as the status census exempts them) and returns,
// per "path:Decl", each form found there with its line: the Go comparisons
// goSite accepts, and the SQL comparisons sqlAt finds in a string's text. A
// declaration exempt names is the one function, not a spelling.
func siteCensus(t *testing.T, goSite func(*ast.BinaryExpr) bool, sqlAt func(string) []int,
	exempt func(rel, decl string) bool) map[string][]string {
	t.Helper()
	root := repoRoot(t)
	got := map[string][]string{}
	fset := token.NewFileSet()
	for _, f := range repoGoFiles(t, root) {
		rel := repoRel(t, root, f)
		if isLegacyMigration(rel) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, u := range declUnits(file) {
			if exempt(rel, u.name) {
				continue
			}
			key := rel + ":" + u.name
			note := func(form string, pos token.Pos) {
				got[key] = append(got[key], strings.Join(strings.Fields(form), " ")+
					" (line "+strconv.Itoa(fset.Position(pos).Line)+")")
			}
			read := map[*ast.BasicLit]bool{}
			ast.Inspect(u.node, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BinaryExpr:
					if goSite(x) {
						note(string(src[fset.Position(x.Pos()).Offset:fset.Position(x.End()).Offset]), x.Pos())
					}
					if x.Op == token.ADD {
						noteSQL(stringChain(x, read), sqlAt, note)
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING && !read[x] {
						read[x] = true
						noteSQL([]*ast.BasicLit{x}, sqlAt, note)
					}
				}
				return true
			})
		}
	}
	return got
}

// stringChain is the leaves of a `+` chain when one of them is a string
// literal, nil otherwise; its literals are marked read so no part of the chain
// is read twice.
func stringChain(e *ast.BinaryExpr, read map[*ast.BasicLit]bool) []*ast.BasicLit {
	var leaves []*ast.BasicLit
	hasLit := false
	var walk func(x ast.Expr)
	walk = func(x ast.Expr) {
		if b, ok := x.(*ast.BinaryExpr); ok && b.Op == token.ADD {
			walk(b.X)
			walk(b.Y)
			return
		}
		l, ok := x.(*ast.BasicLit)
		if ok && l.Kind == token.STRING {
			hasLit = true
		} else {
			l = nil // an operand that is no literal: an alias, a fragment, a call
		}
		leaves = append(leaves, l)
	}
	walk(e)
	if !hasLit || read[firstLit(leaves)] {
		return nil
	}
	for _, l := range leaves {
		if l != nil {
			read[l] = true
		}
	}
	return leaves
}

func firstLit(leaves []*ast.BasicLit) *ast.BasicLit {
	for _, l := range leaves {
		if l != nil {
			return l
		}
	}
	return nil
}

// noteSQL reads a chain's literals, unquoted, as one text, an operand that is
// no literal as `x`, and notes each comparison sqlAt finds at the literal
// holding it (to the line, in a raw string; at its start, in a quoted one).
func noteSQL(leaves []*ast.BasicLit, sqlAt func(string) []int, note func(string, token.Pos)) {
	if len(leaves) == 0 {
		return
	}
	var text strings.Builder
	starts := make([]int, len(leaves))
	for i, l := range leaves {
		starts[i] = text.Len()
		if l == nil {
			text.WriteString("x")
			continue
		}
		v, err := strconv.Unquote(l.Value)
		if err != nil {
			v = l.Value
		}
		text.WriteString(v)
	}
	s := text.String()
	at := sqlAt(s)
	for i := 0; i+1 < len(at); i += 2 {
		j := len(starts) - 1
		for j > 0 && (starts[j] > at[i] || leaves[j] == nil) {
			j--
		}
		pos := token.NoPos
		if l := leaves[j]; l != nil {
			pos = l.Pos()
			if strings.HasPrefix(l.Value, "`") {
				pos += token.Pos(1 + at[i] - starts[j])
			}
		}
		note(s[at[i]:at[i+1]], pos)
	}
}

// declUnit is one named declaration the census keys a site by.
type declUnit struct {
	name string
	node ast.Node
}

// declUnits are a file's declarations by name: Func, Recv.Method, and each
// const, var or type spec of a block by its own first name.
func declUnits(f *ast.File) []declUnit {
	var out []declUnit
	for _, d := range f.Decls {
		switch v := d.(type) {
		case *ast.FuncDecl:
			name := v.Name.Name
			if v.Recv != nil && len(v.Recv.List) > 0 {
				name = recvName(v.Recv.List[0].Type) + "." + name
			}
			out = append(out, declUnit{name, v})
		case *ast.GenDecl:
			for _, s := range v.Specs {
				switch spec := s.(type) {
				case *ast.ValueSpec:
					out = append(out, declUnit{spec.Names[0].Name, spec})
				case *ast.TypeSpec:
					out = append(out, declUnit{spec.Name.Name, spec})
				}
			}
		}
	}
	return out
}

func recvName(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.StarExpr:
		return recvName(v.X)
	case *ast.IndexExpr:
		return recvName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return "?"
}
