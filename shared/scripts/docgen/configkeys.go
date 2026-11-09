package main

// configkeys.go — the configuration key tables: every yaml key a module's
// Config accepts, with its type, its shipped default and its field comment.
//
// Two sources, because neither holds the whole row. The DEFAULT exists only at
// run time (config.Defaults() builds it), so each module renders it into a
// committed snapshot, config/defaults.snapshot.txt, guarded by that module's
// own TestDefaultsSnapshotIsCurrent; this reads the snapshot's [FIELDS]
// section for key and default. The TYPE and the DESCRIPTION are in the source,
// so the struct tree under `type Config struct` is read as AST for the yaml
// path, the field's type and its doc comment.
//
// The two must name the same keys. A key in one and not the other is a stale
// snapshot (or a walk that has drifted from the renderer's), and it is an
// error rather than a quietly partial table.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func init() {
	for _, dir := range []string{"shingo-core/config", "shingo-edge/config"} {
		register(Generator{
			Name: dir + " Config",
			Emit: func(root string, args []string) (string, error) {
				if len(args) > 0 {
					return "", fmt.Errorf("takes no arguments, got %q", args)
				}
				return cfgkeysEmit(root, dir)
			},
		})
	}
}

// cfgkeysRow is one row of the table.
type cfgkeysRow struct {
	key  string // dotted yaml path; a list element's field is parent[].field
	typ  string
	desc string
	elem bool // a field of a list element: no default of its own
}

func cfgkeysEmit(root, dir string) (string, error) {
	defaults, err := cfgkeysSnapshot(filepath.Join(root, filepath.FromSlash(dir), "defaults.snapshot.txt"))
	if err != nil {
		return "", err
	}
	rows, err := cfgkeysWalk(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return "", err
	}

	leaves := map[string]bool{}
	for _, r := range rows {
		if !r.elem {
			leaves[r.key] = true
		}
	}
	var missing, extra []string
	for k := range leaves {
		if _, ok := defaults[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range defaults {
		if !leaves[k] {
			extra = append(extra, k)
		}
	}
	if len(missing)+len(extra) > 0 {
		sort.Strings(missing)
		sort.Strings(extra)
		return "", fmt.Errorf("%s/defaults.snapshot.txt and the Config struct disagree "+
			"(in the struct only: %v; in the snapshot only: %v) — run `make defaults-snapshot` in the module",
			dir, missing, extra)
	}

	// Leaves in key order, each list's element fields straight after it.
	byParent := map[string][]cfgkeysRow{}
	var top []cfgkeysRow
	for _, r := range rows {
		if r.elem {
			parent := r.key[:strings.Index(r.key, "[]")]
			byParent[parent] = append(byParent[parent], r)
			continue
		}
		top = append(top, r)
	}
	sort.Slice(top, func(i, j int) bool { return top[i].key < top[j].key })

	var b strings.Builder
	b.WriteString("| Key | Type | Default | Description |\n|---|---|---|---|\n")
	write := func(r cfgkeysRow, def string) {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.key, r.typ, def, cfgkeysCell(r.desc))
	}
	for _, r := range top {
		write(r, cfgkeysDefault(defaults[r.key]))
		kids := byParent[r.key]
		sort.Slice(kids, func(i, j int) bool { return kids[i].key < kids[j].key })
		for _, k := range kids {
			write(k, "—")
		}
	}
	return b.String(), nil
}

// cfgkeysSnapshot reads the [FIELDS] section of a defaults snapshot.
func cfgkeysSnapshot(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[FIELDS]" || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, " =")
		if !ok {
			return nil, fmt.Errorf("%s: a [FIELDS] line with no ` = `: %q", path, line)
		}
		out[key] = strings.TrimPrefix(value, " ")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no [FIELDS] section", path)
	}
	return out, nil
}

// cfgkeysStructs is every struct type the package's non-test files declare,
// by name, parsed with comments.
func cfgkeysStructs(fset *token.FileSet, dir string) (map[string]*ast.StructType, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	structs := map[string]*ast.StructType{}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				ts := s.(*ast.TypeSpec)
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[ts.Name.Name] = st
				}
			}
		}
	}
	return structs, nil
}

// cfgkeysWalk parses the package's non-test files and walks the struct tree
// from Config the way the snapshot renderer walks the value: exported fields,
// yaml tag names (the field name where there is none), `yaml:"-"` skipped, a
// struct of this package a section, time.Time a leaf.
func cfgkeysWalk(dir string) ([]cfgkeysRow, error) {
	fset := token.NewFileSet()
	structs, err := cfgkeysStructs(fset, dir)
	if err != nil {
		return nil, err
	}
	if structs["Config"] == nil {
		return nil, fmt.Errorf("%s: no `type Config struct`", dir)
	}
	var rows []cfgkeysRow
	var walk func(prefix string, st *ast.StructType, elem bool, depth int) error
	walk = func(prefix string, st *ast.StructType, elem bool, depth int) error {
		if depth > 8 {
			return fmt.Errorf("%s: the Config struct tree is deeper than 8 at %q", dir, prefix)
		}
		// A doc comment over a run of consecutive fields (no blank line between
		// them) speaks for the whole run, the way gofmt groups it: the fields
		// after the first inherit it until a blank line or their own doc.
		group, prevEnd := "", -1
		for _, f := range st.Fields.List {
			switch {
			case f.Doc != nil:
				group = cfgkeysPara(f.Doc)
			case fset.Position(f.Pos()).Line != prevEnd+1:
				group = ""
			}
			prevEnd = fset.Position(f.End()).Line
			if len(f.Names) == 0 {
				return fmt.Errorf("%s: an embedded field under %q, which the walk does not follow", dir, prefix)
			}
			for _, nm := range f.Names {
				if !nm.IsExported() {
					continue
				}
				name := nm.Name
				if f.Tag != nil {
					tag, _ := strconv.Unquote(f.Tag.Value)
					y := reflect.StructTag(tag).Get("yaml")
					if y == "-" {
						continue
					}
					if y != "" {
						name = strings.Split(y, ",")[0]
					}
				}
				key := name
				if prefix != "" {
					key = prefix + "." + name
				}
				if id, ok := f.Type.(*ast.Ident); ok && structs[id.Name] != nil {
					if err := walk(key, structs[id.Name], elem, depth+1); err != nil {
						return err
					}
					continue
				}
				rows = append(rows, cfgkeysRow{key: key, typ: cfgkeysType(f.Type, structs), desc: cfgkeysFieldDoc(group, f), elem: elem})
				if at, ok := f.Type.(*ast.ArrayType); ok {
					if id, ok := at.Elt.(*ast.Ident); ok && structs[id.Name] != nil {
						if err := walk(key+"[]", structs[id.Name], true, depth+1); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
	if err := walk("", structs["Config"], false, 0); err != nil {
		return nil, err
	}
	return rows, nil
}

// cfgkeysType names a field's type the way a yaml author reads it.
func cfgkeysType(e ast.Expr, structs map[string]*ast.StructType) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return cfgkeysType(t.X, structs)
	case *ast.SelectorExpr:
		switch x, _ := t.X.(*ast.Ident); {
		case x != nil && x.Name == "time" && t.Sel.Name == "Duration":
			return "duration"
		case x != nil && x.Name == "time" && t.Sel.Name == "Time":
			return "timestamp"
		case x != nil && x.Name == "time" && t.Sel.Name == "Weekday":
			return "weekday"
		case x != nil:
			return x.Name + "." + t.Sel.Name
		}
	case *ast.ArrayType:
		if id, ok := t.Elt.(*ast.Ident); ok && structs[id.Name] != nil {
			return "list"
		}
		return "list of " + cfgkeysType(t.Elt, structs)
	case *ast.MapType:
		return "map"
	}
	return "?"
}

// cfgkeysPara is the first paragraph of a comment, on one line.
func cfgkeysPara(c *ast.CommentGroup) string {
	para, _, _ := strings.Cut(strings.TrimSpace(c.Text()), "\n\n")
	return strings.Join(strings.Fields(para), " ")
}

// cfgkeysFieldDoc is a field's description: the first paragraph of the doc
// comment that speaks for it, with its trailing line comment after it in
// parentheses; or the line comment alone.
func cfgkeysFieldDoc(doc string, f *ast.Field) string {
	line := ""
	if f.Comment != nil {
		line = strings.Join(strings.Fields(f.Comment.Text()), " ")
	}
	switch {
	case doc != "" && line != "":
		return doc + " (" + line + ")"
	case doc != "":
		return doc
	}
	return line
}

// cfgkeysDefault renders one snapshot value for a table cell.
func cfgkeysDefault(v string) string {
	switch {
	case v == "" || v == "<empty>":
		return "_(empty)_"
	case v == "<unset>":
		return "_(unset)_"
	case strings.HasPrefix(v, "<set"):
		return "_(set; redacted)_"
	case v == "0001-01-01 00:00:00 +0000 UTC":
		return "_(zero)_"
	}
	return "`" + v + "`"
}

// cfgkeysTagLike is a `<` that GitHub would read as the start of an HTML tag.
var cfgkeysTagLike = regexp.MustCompile(`<([A-Za-z/!])`)

// cfgkeysCell makes comment text safe inside a markdown table cell.
func cfgkeysCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return cfgkeysTagLike.ReplaceAllString(s, "&lt;$1")
}
