package assets

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"shingo/shared"
)

// The trees the two binaries embed, read from disk: the handler is built over
// them as each router builds it (shingo-edge/www/static_assets.go,
// shingo-core/www/static_assets.go), so what holds here holds for every file a
// plant is served. A test in shared reading the other modules' trees, as
// shingo-edge/www/css_dead_token_test.go does.
func realTrees(t *testing.T) map[string]*Handler {
	t.Helper()
	out := map[string]*Handler{}
	for name, dir := range map[string]string{
		"edge": filepath.Join("..", "..", "shingo-edge", "www", "static"),
		"core": filepath.Join("..", "..", "shingo-core", "www", "static"),
	} {
		h, err := New(Mount{"/static/", os.DirFS(dir)}, Mount{"/static/shared/", shared.Files})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if h.Stats().Files < 20 {
			t.Fatalf("%s: %d files: not the tree", name, h.Stats().Files)
		}
		out[name] = h
	}
	return out
}

func source(t *testing.T, f *file) []byte {
	t.Helper()
	b, err := fs.ReadFile(f.fsys, f.name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every stylesheet keeps its line count, strips to a fixed point, and every
// line that holds no part of a comment is served byte for byte: a string, a
// url() or a selector outside a comment is never touched.
func TestRealTrees_CSSStripKeepsLinesAndCode(t *testing.T) {
	for tree, h := range realTrees(t) {
		n := 0
		for u, f := range h.files {
			if path.Ext(u) != ".css" {
				continue
			}
			n++
			src := source(t, f)
			out := StripCSSComments(src)
			sl, ol := strings.Split(string(src), "\n"), strings.Split(string(out), "\n")
			if len(sl) != len(ol) {
				t.Errorf("%s %s: %d lines served from %d", tree, u, len(ol), len(sl))
				continue
			}
			if again := StripCSSComments(out); !bytes.Equal(again, out) {
				t.Errorf("%s %s: stripping twice changes it", tree, u)
			}
			inComment := false
			for i, line := range sl {
				touched := inComment || strings.Contains(line, "/*") || strings.Contains(line, "*/")
				if !touched && ol[i] != line {
					t.Errorf("%s %s:%d: a line with no comment changed:\n  %q\n  %q", tree, u, i+1, line, ol[i])
				}
				for rest := line; ; {
					if inComment {
						j := strings.Index(rest, "*/")
						if j < 0 {
							break
						}
						inComment, rest = false, rest[j+2:]
					} else {
						j := strings.Index(rest, "/*")
						if j < 0 {
							break
						}
						inComment, rest = true, rest[j+2:]
					}
				}
			}
			if strings.Contains(strings.ReplaceAll(string(out), "/**/", ""), "/*") {
				t.Errorf("%s %s: a comment survived", tree, u)
			}
		}
		if n == 0 {
			t.Fatalf("%s: no stylesheet", tree)
		}
	}
}

// What the handler holds is one copy per file at most, the gzip of exactly the
// bytes it serves; a file it does not change it does not hold raw.
func TestRealTrees_OneHeldCopyIsTheServedBytes(t *testing.T) {
	for tree, h := range realTrees(t) {
		var held int64
		for u, f := range h.files {
			src := source(t, f)
			want := src
			if path.Ext(u) == ".css" {
				want = StripCSSComments(src)
			}
			if f.gz != nil && f.plain != nil {
				t.Errorf("%s %s: holds a gzip and a raw copy", tree, u)
			}
			if f.sourceServed != bytes.Equal(want, src) || (f.sourceServed && f.plain != nil) {
				t.Errorf("%s %s: served as embedded %v, raw copy held %v", tree, u, f.sourceServed, f.plain != nil)
			}
			if f.gz != nil && !bytes.Equal(gunzip(t, f.gz), want) {
				t.Errorf("%s %s: the held gzip is not the served bytes", tree, u)
			}
			if f.plain != nil && !bytes.Equal(f.plain, want) {
				t.Errorf("%s %s: the raw copy is not the served bytes", tree, u)
			}
			if compressible(f.ctype) && len(want) > 1024 && f.gz == nil {
				t.Errorf("%s %s: %d bytes of %s held no gzip", tree, u, len(want), f.ctype)
			}
			held += int64(len(f.gz) + len(f.plain))
		}
		if held != h.Stats().HeldBytes {
			t.Errorf("%s: files hold %d bytes, Stats says %d", tree, held, h.Stats().HeldBytes)
		}
	}
}

// Every file the trees serve has its type from the table, so no answer
// depends on the host.
func TestRealTrees_EveryTypeIsFromTheTable(t *testing.T) {
	for tree, h := range realTrees(t) {
		for u := range h.files {
			if _, ok := contentTypes[strings.ToLower(path.Ext(u))]; !ok {
				t.Errorf("%s %s: extension not in contentTypes, so its type is sniffed", tree, u)
			}
		}
	}
}

// lineImportRes is a second reader of a module's imports, by regular
// expression over whole lines: an import or export-from statement at the
// start of a line (its clause may run over several), and import('…') of one
// string anywhere on a line that is not a comment line. Cruder than
// scanImports and blind in other ways, so where the two agree on every
// module of both trees the lexer is reading these trees right.
var lineImportRes = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^[ \t]*import\s+(?:[\w$*{}\s,]+?\s+from\s*)?['"]([^'"\n]+)['"]`),
	regexp.MustCompile(`(?m)^[ \t]*export\s+(?:\*(?:\s+as\s+[\w$]+)?|\{[^}]*\})\s*from\s*['"]([^'"\n]+)['"]`),
	regexp.MustCompile(`\bimport\(\s*['"]([^'"\n]+)['"]\s*[,)]`),
}

var commentLineRe = regexp.MustCompile(`(?m)^[ \t]*(?://|/?\*).*$`)

func lineImports(src []byte) []string {
	code := commentLineRe.ReplaceAll(src, nil)
	var out []string
	for _, re := range lineImportRes {
		for _, m := range re.FindAllSubmatch(code, -1) {
			out = append(out, string(m[1]))
		}
	}
	return out
}

// Every module's imports, as the handler resolved them for the import map,
// are what a line reader finds in it, on every module of both trees.
func TestRealTrees_ImportScanAgreesWithALineReader(t *testing.T) {
	for tree, h := range realTrees(t) {
		modules, edges := 0, 0
		for u, f := range h.files {
			if path.Ext(u) != ".js" {
				continue
			}
			modules++
			got := map[string]bool{}
			for _, d := range f.deps {
				got[d] = true
			}
			want := map[string]bool{}
			for _, d := range h.resolveDeps(u, lineImports(source(t, f))) {
				want[d] = true
			}
			edges += len(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: the lexer reads %v, the line reader %v", tree, u, keys(got), keys(want))
			}
		}
		if modules == 0 || edges == 0 {
			t.Fatalf("%s: %d modules, %d imports: not the tree", tree, modules, edges)
		}
		t.Logf("%s: %d modules, %d import edges agree", tree, modules, edges)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
