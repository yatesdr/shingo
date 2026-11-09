package main

// routes.go — the HTTP route table and the page rosters, read off each
// module's chi router.
//
// THE SOURCE IS NewRouter in <module>/www/router.go, read as AST: every
// r.Get/Post/Put/Patch/Delete/Handle literal, inside the r.Group and r.Route
// closures that scope its path prefix and its middleware, plus the bodies of
// any h.register* helper NewRouter calls (a file built only under the `sim`
// tag marks its routes sim-only). The auth group is the middleware stack: a
// route registered after r.Use(<auth middleware>) in its closure needs a login.
//
// A PAGE is a route whose handler renders one of the module's
// www/templates/*.html: its body (or a method it calls on h, two levels deep,
// or a package-level var it reads, such as a kind-to-template map) names the
// file as a string literal. Both routers render that way — Core clones a page
// set per templates/*.html file (fs.Glob) and Edge parses one set — so the
// literal names the template in both. The title is the template's first <h1>
// (else its <title>) when that is static text.
//
// Three generators share the parse:
//
//	<!-- generated: shingo-core/www/router.go routes -->  every Core route, grouped
//	<!-- generated: shingo-core/www/router.go pages -->   Core's page roster
//	<!-- generated: shingo-edge/www/router.go pages [public|login] -->  Edge's

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// routesModule is what the parse needs to know about one module's router.
type routesModule struct {
	dir  string // the www package, root-relative
	auth string // the middleware method whose presence means "needs a login"
}

var routesModules = map[string]routesModule{
	"shingo-core": {dir: "shingo-core/www", auth: "requireAuth"},
	"shingo-edge": {dir: "shingo-edge/www", auth: "adminMiddleware"},
}

func init() {
	register(Generator{
		Name: "shingo-core/www/router.go routes",
		Emit: func(root string, args []string) (string, error) {
			if len(args) > 0 {
				return "", fmt.Errorf("takes no arguments, got %q", args)
			}
			return routesEmitTable(root, routesModules["shingo-core"])
		},
	})
	for _, mod := range []string{"shingo-core", "shingo-edge"} {
		register(Generator{
			Name: mod + "/www/router.go pages",
			Emit: func(root string, args []string) (string, error) {
				return routesEmitPages(root, routesModules[mod], args)
			},
		})
	}
}

// routesRoute is one registered route.
type routesRoute struct {
	method, path string
	handler      ast.Expr // as written at the registration
	login        bool     // registered behind the auth middleware
	sim          bool     // registered only in a `sim` build
}

// routesPkg is the parsed www package.
type routesPkg struct {
	fset    *token.FileSet
	methods map[string][]*routesFunc // Handlers method name -> its decls (one per build variant)
	vars    map[string]ast.Expr      // package-level var -> its initializer
	router  *ast.FuncDecl
}

type routesFunc struct {
	decl *ast.FuncDecl
	sim  bool // declared in a file built only with the sim tag
}

var routesSimBuild = regexp.MustCompile(`(?m)^//go:build .*\bsim\b`)

func routesParse(root string, m routesModule) (*routesPkg, error) {
	dir := filepath.Join(root, filepath.FromSlash(m.dir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	p := &routesPkg{fset: token.NewFileSet(), methods: map[string][]*routesFunc{}, vars: map[string]ast.Expr{}}
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(p.fset, n, src, 0)
		if err != nil {
			return nil, err
		}
		head := string(src)
		if i := strings.Index(head, "\npackage "); i >= 0 {
			head = head[:i]
		}
		sim := routesSimBuild.MatchString(head) && !strings.Contains(head, "!sim")
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					if d.Name.Name == "NewRouter" {
						p.router = d
					}
					continue
				}
				p.methods[d.Name.Name] = append(p.methods[d.Name.Name], &routesFunc{decl: d, sim: sim})
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					for i, nm := range vs.Names {
						if i < len(vs.Values) {
							p.vars[nm.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	if p.router == nil {
		return nil, fmt.Errorf("%s: no func NewRouter", m.dir)
	}
	return p, nil
}

// routesCtx is the scope a registration happens in.
type routesCtx struct {
	prefix  string
	mws     []string
	routers []string // the identifiers that name the router in this scope
	sim     bool
	depth   int
}

var routesVerbs = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE",
	"Head": "HEAD", "Options": "OPTIONS", "Handle": "ANY", "HandleFunc": "ANY",
}

// routesCollect walks NewRouter and returns every route it registers.
func routesCollect(p *routesPkg, m routesModule) ([]routesRoute, error) {
	var out []routesRoute
	var walk func(stmts []ast.Stmt, c routesCtx) error
	walk = func(stmts []ast.Stmt, c routesCtx) error {
		for _, s := range stmts {
			var err error
			switch s := s.(type) {
			case *ast.ExprStmt:
				if call, ok := s.X.(*ast.CallExpr); ok {
					err = routesCall(p, m, call, &c, walk, &out)
				}
			case *ast.BlockStmt:
				err = walk(s.List, c)
			case *ast.IfStmt:
				err = walk(s.Body.List, c)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(p.router.Body.List, routesCtx{routers: []string{"r"}}); err != nil {
		return nil, err
	}
	return out, nil
}

// routesCall handles one call statement in a router body.
func routesCall(p *routesPkg, m routesModule, call *ast.CallExpr, c *routesCtx,
	walk func([]ast.Stmt, routesCtx) error, out *[]routesRoute) error {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	recv, _ := sel.X.(*ast.Ident)
	if recv == nil {
		return nil
	}
	name := sel.Sel.Name
	at := p.fset.Position(call.Pos())
	if recv.Name == "h" {
		// A helper on Handlers that registers routes on the router it is
		// handed (registerSimRoutes): walk each build variant's body in place,
		// with the helper's own parameter names as the router.
		if !strings.HasPrefix(name, "register") {
			return nil
		}
		if c.depth > 4 {
			return fmt.Errorf("%s: register helpers nest deeper than 4", at)
		}
		for _, fn := range p.methods[name] {
			inner := *c
			inner.sim = c.sim || fn.sim
			inner.depth = c.depth + 1
			inner.mws = append([]string(nil), c.mws...)
			inner.routers = routesParams(fn.decl.Type)
			if err := walk(fn.decl.Body.List, inner); err != nil {
				return err
			}
		}
		return nil
	}
	if !slices.Contains(c.routers, recv.Name) {
		return nil
	}
	switch name {
	case "Use":
		for _, a := range call.Args {
			c.mws = append(c.mws, routesExprName(a))
		}
		return nil
	case "Group", "Route":
		prefix, fnArg := c.prefix, 0
		if name == "Route" {
			lit, err := routesString(call, 0)
			if err != nil {
				return fmt.Errorf("%s: r.Route: %w", at, err)
			}
			prefix, fnArg = c.prefix+lit, 1
		}
		if len(call.Args) <= fnArg {
			return fmt.Errorf("%s: r.%s with no closure", at, name)
		}
		fl, ok := call.Args[fnArg].(*ast.FuncLit)
		if !ok {
			return fmt.Errorf("%s: r.%s with a closure that is not a func literal", at, name)
		}
		inner := *c
		inner.prefix = prefix
		inner.mws = append([]string(nil), c.mws...)
		inner.routers = routesParams(fl.Type)
		return walk(fl.Body.List, inner)
	case "Mount", "Method", "MethodFunc", "With", "NotFound", "MethodNotAllowed":
		return fmt.Errorf("%s: r.%s is not read by docgen's route parse; teach routes.go", at, name)
	}
	verb, ok := routesVerbs[name]
	if !ok {
		return nil
	}
	path, err := routesString(call, 0)
	if err != nil || len(call.Args) < 2 {
		return fmt.Errorf("%s: r.%s without a literal path and a handler", at, name)
	}
	login := slices.Contains(c.mws, m.auth)
	*out = append(*out, routesRoute{method: verb, path: c.prefix + path, handler: call.Args[1], login: login, sim: c.sim})
	return nil
}

// routesParams is the parameter names of a func type: inside a Group/Route
// closure or a register helper, those name the router.
func routesParams(ft *ast.FuncType) []string {
	var out []string
	for _, f := range ft.Params.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func routesString(call *ast.CallExpr, i int) (string, error) {
	if len(call.Args) <= i {
		return "", fmt.Errorf("argument %d missing", i)
	}
	bl, ok := call.Args[i].(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", fmt.Errorf("argument %d is not a string literal", i)
	}
	return strconv.Unquote(bl.Value)
}

// routesExprName names a middleware or handler expression: h.requireAuth is
// requireAuth, middleware.Compress(5) is middleware.Compress.
func routesExprName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok && x.Name == "h" {
			return e.Sel.Name
		}
		return routesExprName(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		return routesExprName(e.Fun)
	}
	return "?"
}

// routesHandler renders a handler for the table.
func routesHandler(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		// h.apiListNodes is apiListNodes; hub.SSEHandler, a method on a local
		// value, is its method name.
		return "`" + e.Sel.Name + "`"
	case *ast.FuncLit:
		target := ""
		ast.Inspect(e.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || routesExprName(call.Fun) != "http.Redirect" || len(call.Args) < 3 {
				return true
			}
			target = "?"
			if bl, ok := call.Args[2].(*ast.BasicLit); ok && bl.Kind == token.STRING {
				target, _ = strconv.Unquote(bl.Value)
			}
			return false
		})
		switch target {
		case "":
			return "(inline)"
		case "?":
			return "(inline redirect)"
		}
		return "(redirect to `" + target + "`)"
	case *ast.CallExpr:
		static := false
		ast.Inspect(e, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "FileServer" {
				static = true
			}
			return !static
		})
		if static {
			return "(static files)"
		}
		return "`" + routesExprName(e.Fun) + "`"
	}
	return "`" + routesExprName(e) + "`"
}

func routesAuth(r routesRoute) string {
	if r.login {
		return "login"
	}
	return "public"
}

// routesEmitTable is the full route table, one section per area.
func routesEmitTable(root string, m routesModule) (string, error) {
	p, err := routesParse(root, m)
	if err != nil {
		return "", err
	}
	routes, err := routesCollect(p, m)
	if err != nil {
		return "", err
	}
	groups := map[string][]routesRoute{}
	for _, r := range routes {
		groups[routesArea(r.path)] = append(groups[routesArea(r.path)], r)
	}
	var names []string
	for g := range groups {
		names = append(names, g)
	}
	// Pages and forms first, then the /api areas alphabetically.
	sort.Slice(names, func(i, j int) bool {
		ai, aj := strings.HasPrefix(names[i], "/api"), strings.HasPrefix(names[j], "/api")
		if ai != aj {
			return !ai
		}
		return names[i] < names[j]
	})
	login, sim := 0, 0
	for _, r := range routes {
		if r.login {
			login++
		}
		if r.sim {
			sim++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d routes (%d behind `%s`, %d registered only in a `sim` build), from `NewRouter` in `%s/router.go`. "+
		"**Auth**: `login` is behind `h.%s` (a session cookie from `/login`); `public` needs none. "+
		"Grouped by area: everything outside `/api` under *Pages and forms*, then one section per first path segment under `/api`; "+
		"within a section, by path then method.\n\n",
		len(routes), login, m.auth, sim, m.dir, m.auth)
	for _, g := range names {
		rs := groups[g]
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].path != rs[j].path {
				return rs[i].path < rs[j].path
			}
			return rs[i].method < rs[j].method
		})
		title := "`" + g + "`"
		if g == "" {
			title = "Pages and forms"
		}
		fmt.Fprintf(&b, "### %s\n\n| Method | Path | Handler | Auth |\n|---|---|---|---|\n", title)
		for _, r := range rs {
			h := routesHandler(r.handler)
			if r.sim {
				h += " (sim build)"
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", r.method, r.path, h, routesAuth(r))
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// routesArea is a route's section: "" outside /api, else /api/<first segment>.
func routesArea(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/")
	if !ok {
		return ""
	}
	seg, _, _ := strings.Cut(rest, "/")
	return "/api/" + seg
}

// routesPage is one roster row.
type routesPage struct {
	path      string
	templates []string
	login     bool
}

// routesEmitPages is a module's page roster. The optional argument narrows
// it to the public pages or the login ones.
func routesEmitPages(root string, m routesModule, args []string) (string, error) {
	filter := ""
	switch {
	case len(args) == 1 && (args[0] == "public" || args[0] == "login"):
		filter = args[0]
	case len(args) > 0:
		return "", fmt.Errorf("takes no argument, `public` or `login`; got %q", args)
	}
	pages, err := routesPages(root, m)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| Path | Template (in `%s/templates/`) | Title | Auth |\n|---|---|---|---|\n", m.dir)
	for _, pg := range pages {
		if (filter == "public" && pg.login) || (filter == "login" && !pg.login) {
			continue
		}
		var tmpls []string
		for _, t := range pg.templates {
			tmpls = append(tmpls, "`"+t+"`")
		}
		title, err := routesTitle(filepath.Join(root, filepath.FromSlash(m.dir), "templates"), pg.templates[0], pg.path)
		if err != nil {
			return "", err
		}
		auth := "public"
		if pg.login {
			auth = "login"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", pg.path, strings.Join(tmpls, ", "), title, auth)
	}
	return b.String(), nil
}

// routesPages finds the GET routes outside /api whose handler renders a
// template, sorted by path.
func routesPages(root string, m routesModule) ([]routesPage, error) {
	p, err := routesParse(root, m)
	if err != nil {
		return nil, err
	}
	routes, err := routesCollect(p, m)
	if err != nil {
		return nil, err
	}
	tdir := filepath.Join(root, filepath.FromSlash(m.dir), "templates")
	var out []routesPage
	for _, r := range routes {
		if r.method != "GET" || strings.HasPrefix(r.path, "/api/") {
			continue
		}
		sel, ok := r.handler.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "h" {
			continue
		}
		found := map[string]bool{}
		routesTemplatesOf(p, sel.Sel.Name, tdir, found, map[string]bool{}, 0)
		if len(found) == 0 {
			continue
		}
		var ts []string
		for t := range found {
			ts = append(ts, t)
		}
		sort.Strings(ts)
		out = append(out, routesPage{path: r.path, templates: ts, login: r.login})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// routesTemplatesOf adds every templates/*.html a Handlers method renders: a
// string literal passed to a call in its body (h.render(w, r, "x.html", ...),
// h.renderTemplate, ExecuteTemplate), a value of a package-level var its body
// reads (a kind-to-template map), or the same in a method it calls on h, up
// to two calls deep. A literal used as an index (h.tmpls["x.html"], how a
// fragment handler reaches a page's sub-template) does not make a page.
func routesTemplatesOf(p *routesPkg, method, tdir string, found, seen map[string]bool, depth int) {
	if seen[method] || depth > 2 {
		return
	}
	seen[method] = true
	add := func(e ast.Expr) {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return
		}
		s, err := strconv.Unquote(bl.Value)
		if err != nil || !strings.HasSuffix(s, ".html") || strings.Contains(s, "/") {
			return
		}
		if _, err := os.Stat(filepath.Join(tdir, s)); err == nil {
			found[s] = true
		}
	}
	for _, fn := range p.methods[method] {
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if cl, ok := p.vars[n.Name].(*ast.CompositeLit); ok {
					for _, el := range cl.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							add(kv.Value)
						} else {
							add(el)
						}
					}
				}
			case *ast.CallExpr:
				for _, a := range n.Args {
					add(a)
				}
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "h" {
						routesTemplatesOf(p, sel.Sel.Name, tdir, found, seen, depth+1)
					}
				}
			}
			return true
		})
	}
}

var (
	routesH1Tag    = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
	routesTitleTag = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	routesNavLink  = regexp.MustCompile(`<a href="(/[^"]*)"[^>]*>([^<]*)</a>`)
)

// routesTitle is a page's static heading: its template's first <h1>, else
// its <title>, when that holds no template action; else the label of a link
// to the path in the module's nav (any templates/*.html, the layout or the
// header). Blank when none of them is static.
func routesTitle(tdir, template, path string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(tdir, template))
	if err != nil {
		return "", err
	}
	for _, re := range []*regexp.Regexp{routesH1Tag, routesTitleTag} {
		if m := re.FindSubmatch(raw); m != nil {
			if t := routesStatic(string(m[1])); t != "" {
				return t, nil
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(tdir, "*.html"))
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		for _, m := range routesNavLink.FindAllStringSubmatch(string(raw), -1) {
			if m[1] == path {
				if t := routesStatic(m[2]); t != "" {
					return t, nil
				}
			}
		}
	}
	return "", nil
}

// routesStatic is s on one line, or "" when it carries a template action or
// markup.
func routesStatic(s string) string {
	t := strings.Join(strings.Fields(s), " ")
	if strings.ContainsAny(t, "{<") {
		return ""
	}
	return t
}
