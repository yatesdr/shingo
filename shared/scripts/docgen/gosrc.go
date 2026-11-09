package main

// gosrc.go — the repo's Go source read as syntax, for the generators that
// render a doc table from code.
//
// NO TYPE CHECKER. The shared module may not import shingocore/... or
// shingoedge/..., and type-checking those trees would pull in every third-party
// dependency they have. So a package is parsed file by file and a name is
// resolved the way a reader resolves it: a package qualifier through the file's
// imports, a receiver's field through its struct, a constant through its
// declaration. Files are filtered by build constraints as a linux/amd64 build
// with no tags sees them, which is what a plant runs, so the answer does not
// depend on the machine the generator runs on.

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// gosrcTree is the workspace's Go packages, parsed on demand.
type gosrcTree struct {
	root    string
	fset    *token.FileSet
	modules map[string]string    // module path → slash dir from root
	pkgs    map[string]*gosrcPkg // slash dir from root → package (nil: no Go files)
}

// gosrcPkg is one package's declarations, indexed by name.
type gosrcPkg struct {
	tree   *gosrcTree
	dir    string
	files  []*gosrcFile
	funcs  map[string]*gosrcFunc // "Name", or "Recv.Name" for a method
	types  map[string]*gosrcType
	values map[string]*gosrcValue // package-level const and var
}

// gosrcFile is one parsed file and its import names.
type gosrcFile struct {
	pkg     *gosrcPkg
	path    string // slash, from root
	ast     *ast.File
	imports map[string]string // local name → import path
}

// gosrcFunc is a function or method declaration.
type gosrcFunc struct {
	file *gosrcFile
	decl *ast.FuncDecl
	recv string // receiver type name; "" for a function
}

// name is how a doc cites it: Recv.Name for a method, Name for a function.
func (f *gosrcFunc) name() string {
	if f.recv == "" {
		return f.decl.Name.Name
	}
	return f.recv + "." + f.decl.Name.Name
}

// gosrcType is a type declaration and its doc comment.
type gosrcType struct {
	file *gosrcFile
	spec *ast.TypeSpec
	doc  *ast.CommentGroup
}

// gosrcValue is a package-level const or var and the expression it is
// declared with (nil when it has none of its own).
type gosrcValue struct {
	file *gosrcFile
	expr ast.Expr
	typ  ast.Expr // the declared type, nil when inferred
	doc  *ast.CommentGroup
}

// gosrcNamed is a named type: the package that declares it and its name.
type gosrcNamed struct {
	pkg  *gosrcPkg
	name string
}

func (n gosrcNamed) ok() bool { return n.pkg != nil && n.name != "" }

// typeDecl is the named type's declaration, or nil.
func (n gosrcNamed) typeDecl() *gosrcType {
	if !n.ok() {
		return nil
	}
	return n.pkg.types[n.name]
}

// method is the named type's method, or nil.
func (n gosrcNamed) method(name string) *gosrcFunc {
	if !n.ok() {
		return nil
	}
	return n.pkg.funcs[n.name+"."+name]
}

// newGosrcTree reads the workspace's module map from go.work and each
// module's go.mod.
func newGosrcTree(root string) (*gosrcTree, error) {
	t := &gosrcTree{root: root, fset: token.NewFileSet(), modules: map[string]string{}, pkgs: map[string]*gosrcPkg{}}
	work, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		return nil, err
	}
	for _, dir := range gosrcWorkUses(string(work)) {
		mod, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "go.mod"))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(mod), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
				t.modules[f[1]] = dir
			}
		}
	}
	if len(t.modules) == 0 {
		return nil, fmt.Errorf("go.work names no module")
	}
	return t, nil
}

// gosrcWorkUses is the directories a go.work file's use directives name.
func gosrcWorkUses(work string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(work, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		switch {
		case line == "use (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock && line != "":
			out = append(out, path.Clean(line))
		case strings.HasPrefix(line, "use ") && !strings.HasSuffix(line, "("):
			out = append(out, path.Clean(strings.TrimSpace(strings.TrimPrefix(line, "use "))))
		}
	}
	return out
}

// pkgByImport is the workspace package an import path names, or nil for one
// outside the workspace (the standard library, a dependency).
func (t *gosrcTree) pkgByImport(importPath string) *gosrcPkg {
	best := ""
	for mod := range t.modules {
		if (importPath == mod || strings.HasPrefix(importPath, mod+"/")) && len(mod) > len(best) {
			best = mod
		}
	}
	if best == "" {
		return nil
	}
	p, err := t.pkg(path.Join(t.modules[best], strings.TrimPrefix(importPath, best)))
	if err != nil {
		return nil
	}
	return p
}

// gosrcBuildContext is the build a plant runs: linux/amd64, no tags.
func gosrcBuildContext() build.Context {
	c := build.Default
	c.GOOS, c.GOARCH, c.BuildTags, c.CgoEnabled = "linux", "amd64", nil, false
	return c
}

// pkg parses the package in dir (slash, from root): every non-test .go file
// the plant build would compile.
func (t *gosrcTree) pkg(dir string) (*gosrcPkg, error) {
	dir = path.Clean(dir)
	if p, ok := t.pkgs[dir]; ok {
		if p == nil {
			return nil, fmt.Errorf("%s: no Go package", dir)
		}
		return p, nil
	}
	abs := filepath.Join(t.root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	ctx := gosrcBuildContext()
	p := &gosrcPkg{tree: t, dir: dir, funcs: map[string]*gosrcFunc{}, types: map[string]*gosrcType{}, values: map[string]*gosrcValue{}}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if match, err := ctx.MatchFile(abs, n); err != nil || !match {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(t.fset, filepath.Join(abs, n), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		gf := &gosrcFile{pkg: p, path: path.Join(dir, n), ast: f, imports: map[string]string{}}
		for _, im := range f.Imports {
			ip, _ := strconv.Unquote(im.Path.Value)
			local := path.Base(ip)
			if im.Name != nil {
				local = im.Name.Name
			}
			gf.imports[local] = ip
		}
		p.files = append(p.files, gf)
		p.index(gf)
	}
	if len(p.files) == 0 {
		t.pkgs[dir] = nil
		return nil, fmt.Errorf("%s: no Go package", dir)
	}
	t.pkgs[dir] = p
	return p, nil
}

// index records a file's top-level declarations. The first declaration of a
// name wins, so a name two build variants both declare resolves to one.
func (p *gosrcPkg) index(f *gosrcFile) {
	for _, d := range f.ast.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			fn := &gosrcFunc{file: f, decl: d}
			key := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				fn.recv = gosrcBaseTypeName(d.Recv.List[0].Type)
				key = fn.recv + "." + key
			}
			if _, dup := p.funcs[key]; !dup {
				p.funcs[key] = fn
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					doc := s.Doc
					if doc == nil && len(d.Specs) == 1 {
						doc = d.Doc
					}
					if _, dup := p.types[s.Name.Name]; !dup {
						p.types[s.Name.Name] = &gosrcType{file: f, spec: s, doc: doc}
					}
				case *ast.ValueSpec:
					doc := s.Doc
					if doc == nil && len(d.Specs) == 1 {
						doc = d.Doc
					}
					for i, n := range s.Names {
						v := &gosrcValue{file: f, typ: s.Type, doc: doc}
						if i < len(s.Values) {
							v.expr = s.Values[i]
						}
						if _, dup := p.values[n.Name]; !dup {
							p.values[n.Name] = v
						}
					}
				}
			}
		}
	}
}

// gosrcBaseTypeName is a receiver or type expression's type name, with any
// pointer, parentheses and type arguments stripped.
func gosrcBaseTypeName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		default:
			return ""
		}
	}
}

// namedType resolves a type expression written in this file to the named type
// it denotes, through the file's imports. A builtin or a type outside the
// workspace is not ok.
func (f *gosrcFile) namedType(e ast.Expr) gosrcNamed {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			if _, ok := f.pkg.types[x.Name]; ok {
				return gosrcNamed{f.pkg, x.Name}
			}
			return gosrcNamed{}
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return gosrcNamed{}
			}
			ip, ok := f.imports[id.Name]
			if !ok {
				return gosrcNamed{}
			}
			if p := f.pkg.tree.pkgByImport(ip); p != nil {
				if _, ok := p.types[x.Sel.Name]; ok {
					return gosrcNamed{p, x.Sel.Name}
				}
			}
			return gosrcNamed{}
		default:
			return gosrcNamed{}
		}
	}
}

// structField is the named type's field: its type expression, the file it is
// written in, and its struct tag (unquoted). ok is false for a type that is
// not a struct or has no such field (embedded fields are not searched).
func (n gosrcNamed) structField(name string) (typ ast.Expr, file *gosrcFile, tag string, ok bool) {
	td := n.typeDecl()
	if td == nil {
		return nil, nil, "", false
	}
	st, isStruct := td.spec.Type.(*ast.StructType)
	if !isStruct {
		return nil, nil, "", false
	}
	for _, fld := range st.Fields.List {
		for _, id := range fld.Names {
			if id.Name != name {
				continue
			}
			if fld.Tag != nil {
				tag, _ = strconv.Unquote(fld.Tag.Value)
			}
			return fld.Type, td.file, tag, true
		}
	}
	return nil, nil, "", false
}

// interfaceMethod is the interface's method field, or nil when the named type
// is not an interface or declares no such method.
func (n gosrcNamed) interfaceMethod(name string) *ast.Field {
	td := n.typeDecl()
	if td == nil {
		return nil
	}
	it, ok := td.spec.Type.(*ast.InterfaceType)
	if !ok {
		return nil
	}
	for _, m := range it.Methods.List {
		for _, id := range m.Names {
			if id.Name == name {
				return m
			}
		}
	}
	return nil
}

// isInterface reports whether the named type is declared as an interface.
func (n gosrcNamed) isInterface() bool {
	td := n.typeDecl()
	if td == nil {
		return false
	}
	_, ok := td.spec.Type.(*ast.InterfaceType)
	return ok
}

// stringConst is the string a constant expression written in this file
// denotes: a literal, a name declared in the package, or a qualified name in a
// workspace package, followed through aliases (dispatch.StatusQueued =
// protocol.StatusQueued) and concatenation.
func (f *gosrcFile) stringConst(e ast.Expr) (string, bool) {
	return f.stringConstDepth(e, 0)
}

func (f *gosrcFile) stringConstDepth(e ast.Expr, depth int) (string, bool) {
	if depth > 8 {
		return "", false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return f.stringConstDepth(x.X, depth)
	case *ast.BinaryExpr:
		// Depth counts name hops, not the operands of a long concatenation.
		if x.Op != token.ADD {
			return "", false
		}
		a, okA := f.stringConstDepth(x.X, depth)
		b, okB := f.stringConstDepth(x.Y, depth)
		return a + b, okA && okB
	case *ast.Ident:
		if v := f.pkg.values[x.Name]; v != nil && v.expr != nil {
			return v.file.stringConstDepth(v.expr, depth+1)
		}
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		if p := f.pkg.tree.pkgByImport(f.imports[id.Name]); p != nil {
			if v := p.values[x.Sel.Name]; v != nil && v.expr != nil {
				return v.file.stringConstDepth(v.expr, depth+1)
			}
		}
	}
	return "", false
}

// gosrcExpr renders an expression as source text.
func gosrcExpr(e ast.Expr) string { return types.ExprString(e) }

// gosrcDocSentence is a doc comment's first sentence: the first paragraph with
// its line breaks folded, cut at the first full stop that ends a sentence. A
// stop after a lone capital, a digit or a common abbreviation (e.g., i.e.,
// etc., vs.) does not end one — go/doc's Synopsis cuts "e.g." short, which is
// why this is not that.
func gosrcDocSentence(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return gosrcFirstSentence(cg.Text())
}

// gosrcFirstSentence is text's first sentence (see gosrcDocSentence).
func gosrcFirstSentence(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if i := strings.Index(text, "\n\n"); i >= 0 {
		text = text[:i]
	}
	text = strings.Join(strings.Fields(text), " ")
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '.' && c != '?' && c != '!' {
			continue
		}
		if i+1 < len(text) && text[i+1] != ' ' {
			continue
		}
		if c == '.' && gosrcAbbrevBefore(text[:i]) {
			continue
		}
		return text[:i+1]
	}
	return text
}

// gosrcAbbrevBefore reports whether the word ending at a full stop is one that
// does not end a sentence.
func gosrcAbbrevBefore(s string) bool {
	word := s
	if i := strings.LastIndexAny(s, " ("); i >= 0 {
		word = s[i+1:]
	}
	switch strings.ToLower(word) {
	case "e.g", "i.e", "etc", "vs", "cf":
		return true
	}
	if len(word) == 1 && (unicode.IsUpper(rune(word[0])) || unicode.IsDigit(rune(word[0]))) {
		return true
	}
	return false
}

// gosrcCell makes text safe in a markdown table cell: a pipe is escaped
// everywhere; outside a code span an angle bracket becomes an entity (GitHub
// would read it as an HTML tag), and an asterisk, or an underscore that starts
// a word (_TRANSIT), is escaped so it cannot open emphasis.
func gosrcCell(text string) string {
	parts := strings.Split(text, "`")
	for i := range parts {
		if i%2 == 0 {
			p := strings.NewReplacer("<", "&lt;", ">", "&gt;", "*", `\*`).Replace(parts[i])
			var b strings.Builder
			for j, r := range p {
				if r == '_' && (j == 0 || !gosrcWordByte(p[j-1])) {
					b.WriteString(`\_`)
					continue
				}
				b.WriteRune(r)
			}
			parts[i] = b.String()
		}
		parts[i] = strings.ReplaceAll(parts[i], "|", `\|`)
	}
	return strings.Join(parts, "`")
}

func gosrcWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
