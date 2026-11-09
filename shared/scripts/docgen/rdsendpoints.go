package main

// rdsendpoints.go — the endpoint index at the top of docs/rds-api-reference.md:
// every Seer RDS path the shingo-core/rds client calls, the client func that
// calls it, and whether the fleet adapter (shingo-core/fleet/seerrds) reaches
// that func.
//
// THE ENDPOINTS are the path literals in shingo-core/rds's non-test funcs: a
// string starting "/<name>" in a func body is the endpoint that func calls,
// and the func's GET or POST is which of the client's helpers it calls
// (get/getRaw, post/postRaw, or an http.Client's Post).
//
// "USED BY THE ADAPTER" is reachability by name. The seeds are every rds
// func the adapter's non-test files name — rds.X, or a method called on its
// client or poller field (a.client.X, a.poller.X, client.X); from each, every
// rds func its body calls is reached too, so an endpoint the adapter gets to
// only through the poller, or through one wrapper calling another, is marked.
// Matching is by func name within the rds package, not by type, which can
// over-mark a func that shares a name with a reached one; it cannot
// under-mark one.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func init() {
	register(Generator{
		Name: "shingo-core/rds endpoints",
		Emit: func(root string, args []string) (string, error) {
			if len(args) > 0 {
				return "", fmt.Errorf("takes no arguments, got %q", args)
			}
			return rdsepEmit(root)
		},
	})
}

var rdsepPath = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9]*`)

// rdsepFunc is one func of the rds package.
type rdsepFunc struct {
	name      string          // Recv.Name for a method, Name for a func
	endpoints map[string]bool // path literals
	methods   map[string]bool // GET / POST
	calls     map[string]bool // names of the funcs and methods its body calls
}

func rdsepParseDir(dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func rdsepFuncs(dir string) ([]*rdsepFunc, error) {
	files, err := rdsepParseDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*rdsepFunc
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := &rdsepFunc{name: fd.Name.Name, endpoints: map[string]bool{}, methods: map[string]bool{}, calls: map[string]bool{}}
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				t := fd.Recv.List[0].Type
				if st, ok := t.(*ast.StarExpr); ok {
					t = st.X
				}
				if id, ok := t.(*ast.Ident); ok {
					fn.name = id.Name + "." + fd.Name.Name
				}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind == token.STRING {
						if s, err := strconv.Unquote(n.Value); err == nil {
							if m := rdsepPath.FindString(s); m != "" {
								fn.endpoints[m] = true
							}
						}
					}
				case *ast.CallExpr:
					switch f := n.Fun.(type) {
					case *ast.SelectorExpr:
						fn.calls[f.Sel.Name] = true
						switch f.Sel.Name {
						case "get", "getRaw":
							fn.methods["GET"] = true
						case "post", "postRaw", "Post":
							fn.methods["POST"] = true
						}
					case *ast.Ident:
						fn.calls[f.Name] = true
					}
				}
				return true
			})
			out = append(out, fn)
		}
	}
	return out, nil
}

// rdsepSeeds is the rds func names the adapter's files call.
func rdsepSeeds(dir string) (map[string]bool, error) {
	files, err := rdsepParseDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch x := sel.X.(type) {
			case *ast.Ident:
				if x.Name == "rds" || x.Name == "client" {
					out[sel.Sel.Name] = true
				}
			case *ast.SelectorExpr:
				if x.Sel.Name == "client" || x.Sel.Name == "poller" {
					out[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	return out, nil
}

func rdsepEmit(root string) (string, error) {
	funcs, err := rdsepFuncs(filepath.Join(root, "shingo-core", "rds"))
	if err != nil {
		return "", err
	}
	seeds, err := rdsepSeeds(filepath.Join(root, "shingo-core", "fleet", "seerrds"))
	if err != nil {
		return "", err
	}
	// Reach by name: a func is reached when its bare name is.
	bare := func(name string) string {
		if i := strings.LastIndex(name, "."); i >= 0 {
			return name[i+1:]
		}
		return name
	}
	byName := map[string][]*rdsepFunc{}
	for _, fn := range funcs {
		byName[bare(fn.name)] = append(byName[bare(fn.name)], fn)
	}
	reached := map[string]bool{}
	var queue []string
	for s := range seeds {
		if len(byName[s]) > 0 {
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if reached[n] {
			continue
		}
		reached[n] = true
		for _, fn := range byName[n] {
			for c := range fn.calls {
				if len(byName[c]) > 0 && !reached[c] {
					queue = append(queue, c)
				}
			}
		}
	}

	type row struct {
		endpoint, method, fn string
		used                 bool
	}
	var rows []row
	for _, fn := range funcs {
		if len(fn.endpoints) == 0 {
			continue
		}
		var ms []string
		for m := range fn.methods {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		method := strings.Join(ms, ", ")
		if method == "" {
			return "", fmt.Errorf("rds func %s names an endpoint but calls none of get/getRaw/post/postRaw/Post", fn.name)
		}
		for ep := range fn.endpoints {
			rows = append(rows, row{ep, method, fn.name, reached[bare(fn.name)]})
		}
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("shingo-core/rds: no endpoint literals found")
	}
	sort.Slice(rows, func(i, j int) bool {
		if !strings.EqualFold(rows[i].endpoint, rows[j].endpoint) {
			return strings.ToLower(rows[i].endpoint) < strings.ToLower(rows[j].endpoint)
		}
		return rows[i].fn < rows[j].fn
	})
	used, eps, usedEps := 0, map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		eps[r.endpoint] = true
		if r.used {
			used++
			usedEps[r.endpoint] = true
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d endpoints through %d `rds` client funcs; the fleet adapter reaches %d of the funcs (%d endpoints). "+
		"**Adapter** is `yes` when `shingo-core/fleet/seerrds` calls the func, directly or through another `rds` func "+
		"(the poller included).\n\n", len(eps), len(rows), used, len(usedEps))
	b.WriteString("| Endpoint | Method | `rds` func | Adapter |\n|---|---|---|---|\n")
	for _, r := range rows {
		u := "no"
		if r.used {
			u = "yes"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s |\n", r.endpoint, r.method, r.fn, u)
	}
	return b.String(), nil
}
