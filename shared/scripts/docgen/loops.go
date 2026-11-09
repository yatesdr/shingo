package main

// loops.go — the loop inventory in docs/sweeps-and-monitors.md: every
// background loop a service's Engine.Start starts, where it lives, what starts
// it, its cadence, and the first sentence of its doc comment.
//
// WHAT COUNTS AS STARTED. Engine.Start is read statement by statement, and so
// is each Engine method it calls (one level of helpers, the way the
// startRobotPollLoops-style helpers exist only to keep Start under its
// statement budget). In those bodies:
//
//   - `go x.f(...)` starts f, when f's body loops (a for, or a timer);
//   - `go func() {...}()` that loops is the enclosing function's own loop;
//   - a call to a method of any other type whose body has a `go` statement
//     (x.Run(ctx), x.StartPeriodicSweep(d), x.StartPolling()) starts what that
//     body launches: its looping closures are the method's loop, and each
//     looping named goroutine is a loop of its own;
//   - a call through an interface to a method named Start*, Run* or Loop* is a
//     loop the implementation owns, listed with no cadence.
//
// Closures passed as callbacks (event subscriptions) are not read: a goroutine
// an event handler spawns is per-event work, not a background loop.
//
// THE CADENCE is the first timer the loop takes (NewTicker, Tick, a
// `<-x.After(d)`, NewTimer, Sleep), its duration resolved to a value where one
// is written in the tree: a constant to its value, a parameter to the argument
// the caller passed, a local to what it was assigned (and to its fallbacks), a
// receiver field to the composite literal that sets it, a config field to its
// yaml key. What cannot be resolved is printed as the expression.

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

func init() {
	for _, dir := range []string{"shingo-core/engine", "shingo-edge/engine"} {
		register(Generator{Name: dir + " Engine.Start", Emit: func(root string, args []string) (string, error) {
			if len(args) != 0 {
				return "", fmt.Errorf("takes no arguments, got %q", args)
			}
			rows, err := loopsInventory(root, dir)
			if err != nil {
				return "", err
			}
			return loopsTable(rows), nil
		}})
	}
}

// loopsRow is one background loop.
type loopsRow struct {
	name      string // how the doc cites it
	file      string // slash path from the repo root
	startedBy string
	cadence   string
	doc       string
}

// loopsTable renders the rows in the order Start reaches them.
func loopsTable(rows []loopsRow) string {
	var b strings.Builder
	b.WriteString("| Loop | Where | Started by | Cadence | What it does |\n|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | %s |\n", r.name, r.file, r.startedBy, gosrcCell(r.cadence), gosrcCell(r.doc))
	}
	return b.String()
}

// loopsFrame is one function being read, and the call that reached it: the
// call's arguments, read in the parent frame, bind the function's parameters.
type loopsFrame struct {
	fn       *gosrcFunc
	recvName string
	recvType gosrcNamed
	args     []ast.Expr
	parent   *loopsFrame
}

// loopsCall is one call Start or a helper makes, kept so a constructor's
// parameter can be read at the call that built the value.
type loopsCall struct {
	call *ast.CallExpr
	fr   *loopsFrame
}

// loopsCallee is what a call expression calls: a declared function or method,
// or an interface's method.
type loopsCallee struct {
	fn     *gosrcFunc
	iface  gosrcNamed
	method string
	field  *ast.Field
}

type loopsWalker struct {
	tree  *gosrcTree
	eng   *gosrcPkg // the engine package
	calls []loopsCall
	rows  []loopsRow
	seen  map[string]bool
	errs  []string
}

// loopsInventory reads dir's Engine.Start and returns its loops in the order
// Start reaches them.
func loopsInventory(root, dir string) ([]loopsRow, error) {
	tree, err := newGosrcTree(root)
	if err != nil {
		return nil, err
	}
	pkg, err := tree.pkg(dir)
	if err != nil {
		return nil, err
	}
	start := pkg.funcs["Engine.Start"]
	if start == nil {
		return nil, fmt.Errorf("%s declares no Engine.Start", dir)
	}
	w := &loopsWalker{tree: tree, eng: pkg, seen: map[string]bool{}}
	top := w.frame(start, nil, nil)
	w.record(top, 0)
	w.walk(top, 0)
	if len(w.errs) > 0 {
		return w.rows, fmt.Errorf("%s", strings.Join(w.errs, "; "))
	}
	if len(w.rows) == 0 {
		return nil, fmt.Errorf("%s: Engine.Start starts no loop this generator can see", dir)
	}
	return w.rows, nil
}

func (w *loopsWalker) frame(fn *gosrcFunc, args []ast.Expr, parent *loopsFrame) *loopsFrame {
	fr := &loopsFrame{fn: fn, args: args, parent: parent}
	if r := fn.decl.Recv; r != nil && len(r.List) == 1 {
		if len(r.List[0].Names) == 1 {
			fr.recvName = r.List[0].Names[0].Name
		}
		fr.recvType = gosrcNamed{fn.file.pkg, fn.recv}
	}
	return fr
}

// loopsVisit walks a body in source order, calling onGo for each go statement
// and onCall for each other call, and does not enter closures.
func loopsVisit(body *ast.BlockStmt, onGo func(*ast.GoStmt), onCall func(*ast.CallExpr)) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.GoStmt:
			onGo(n)
			return false
		case *ast.CallExpr:
			onCall(n)
		}
		return true
	})
}

// record keeps every call Start and its helpers make.
func (w *loopsWalker) record(fr *loopsFrame, depth int) {
	loopsVisit(fr.fn.decl.Body, func(*ast.GoStmt) {}, func(c *ast.CallExpr) {
		w.calls = append(w.calls, loopsCall{c, fr})
		if depth == 0 {
			if h := w.helper(c, fr); h != nil {
				w.record(h, 1)
			}
		}
	})
}

// walk reads Start (depth 0) or a helper (depth 1).
func (w *loopsWalker) walk(fr *loopsFrame, depth int) {
	var lits []*ast.FuncLit
	loopsVisit(fr.fn.decl.Body,
		func(g *ast.GoStmt) {
			if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
				if loopsLoops(lit.Body) {
					lits = append(lits, lit)
				}
				return
			}
			c := w.callee(g.Call.Fun, fr)
			if c.fn == nil {
				w.errs = append(w.errs, fmt.Sprintf("%s: `go %s`: cannot resolve what it starts", w.name(fr.fn), gosrcExpr(g.Call.Fun)))
				return
			}
			w.goroutine(c.fn, g.Call.Args, fr)
		},
		func(c *ast.CallExpr) {
			if depth == 0 {
				if h := w.helper(c, fr); h != nil {
					w.walk(h, 1)
					return
				}
			}
			w.launcher(c, fr)
		})
	if len(lits) > 0 {
		starter := "Engine.Start"
		if fr.parent != nil {
			starter = w.name(fr.parent.fn)
		}
		w.add(loopsRow{
			name: w.name(fr.fn), file: fr.fn.file.path, startedBy: starter,
			cadence: w.litsCadence(lits, fr), doc: w.doc(fr.fn, nil),
		})
	}
}

// helper is the frame of an Engine method of this package that a call in Start
// reaches, or nil.
func (w *loopsWalker) helper(c *ast.CallExpr, fr *loopsFrame) *loopsFrame {
	callee := w.callee(c.Fun, fr)
	if callee.fn == nil || !w.isEngine(callee.fn) || callee.fn.decl.Name.Name == "Start" {
		return nil
	}
	return w.frame(callee.fn, c.Args, fr)
}

func (w *loopsWalker) isEngine(fn *gosrcFunc) bool {
	return fn.recv == "Engine" && fn.file.pkg == w.eng
}

// goroutine adds the loop fn, started by `go fn(args)` in the starter's frame.
func (w *loopsWalker) goroutine(fn *gosrcFunc, args []ast.Expr, starter *loopsFrame) {
	if !loopsLoops(fn.decl.Body) {
		return
	}
	fr := w.frame(fn, args, starter)
	w.add(loopsRow{
		name: w.name(fn), file: fn.file.path, startedBy: w.name(starter.fn),
		cadence: w.cadence(fr), doc: w.doc(fn, starter.fn),
	})
}

// launcher reads a call to something other than an Engine helper: a method of
// another type that launches goroutines, or an interface's Start/Run/Loop.
func (w *loopsWalker) launcher(c *ast.CallExpr, fr *loopsFrame) {
	callee := w.callee(c.Fun, fr)
	if callee.iface.ok() {
		if !loopsLauncherName(callee.method) {
			return
		}
		doc := gosrcDocSentence(callee.field.Doc)
		if doc == "" {
			doc = gosrcDocSentence(callee.iface.typeDecl().doc)
		}
		name := callee.iface.name + "." + callee.method
		if doc == "" {
			w.errs = append(w.errs, fmt.Sprintf("%s: neither the method nor the interface has a doc comment", name))
		}
		w.add(loopsRow{
			name: name, file: callee.iface.typeDecl().file.path, startedBy: w.name(fr.fn),
			cadence: "set by the implementation", doc: doc,
		})
		return
	}
	if callee.fn == nil || w.isEngine(callee.fn) {
		return
	}
	lf := w.frame(callee.fn, c.Args, fr)
	var lits []*ast.FuncLit
	loopsVisit(callee.fn.decl.Body,
		func(g *ast.GoStmt) {
			if lit, ok := g.Call.Fun.(*ast.FuncLit); ok {
				if loopsLoops(lit.Body) {
					lits = append(lits, lit)
				}
				return
			}
			// A goroutine whose target is a func value (a handler) is not
			// resolvable and not a background loop.
			if t := w.callee(g.Call.Fun, lf); t.fn != nil {
				w.goroutine(t.fn, g.Call.Args, lf)
			}
		},
		func(*ast.CallExpr) {})
	if len(lits) > 0 {
		w.add(loopsRow{
			name: w.name(callee.fn), file: callee.fn.file.path, startedBy: w.name(fr.fn),
			cadence: w.litsCadence(lits, lf), doc: w.doc(callee.fn, nil),
		})
	}
}

// loopsLauncherName is the interface method names that start a loop.
func loopsLauncherName(m string) bool {
	return strings.HasPrefix(m, "Start") || strings.HasPrefix(m, "Run") || strings.HasPrefix(m, "Loop")
}

func (w *loopsWalker) add(r loopsRow) {
	key := r.name + " " + r.file
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	w.rows = append(w.rows, r)
}

// name is how the table cites a function: an Engine method by its bare name,
// Start as Engine.Start, anything else as Recv.Name.
func (w *loopsWalker) name(fn *gosrcFunc) string {
	if w.isEngine(fn) && fn.decl.Name.Name != "Start" {
		return fn.decl.Name.Name
	}
	return fn.name()
}

// doc is the loop's doc sentence, else its starter's (never Engine.Start's,
// which describes everything). A loop with neither is an error: the inventory
// would carry a row that says nothing.
func (w *loopsWalker) doc(fn, starter *gosrcFunc) string {
	if s := gosrcDocSentence(fn.decl.Doc); s != "" {
		return s
	}
	if starter != nil && !(w.isEngine(starter) && starter.decl.Name.Name == "Start") {
		if s := gosrcDocSentence(starter.decl.Doc); s != "" {
			return s
		}
	}
	w.errs = append(w.errs, fmt.Sprintf("%s (%s) has no doc comment, and neither does what starts it", w.name(fn), fn.file.path))
	return ""
}

// loopsLoops reports whether a body runs for longer than one pass: an
// unbounded for, a keyless range (a channel's, `for range t.C`), or a timer,
// anywhere in it. A for with a condition or a range over a collection is one
// pass over a list, which is per-call work and not a background loop.
func loopsLoops(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ForStmt:
			found = x.Cond == nil
		case *ast.RangeStmt:
			found = x.Key == nil
		}
		if !found {
			if t, _ := loopsTimer(n); t != nil {
				found = true
			}
		}
		return !found
	})
	return found
}

// loopsTimerFuncs are the calls that set a loop's pace.
var loopsTimerFuncs = map[string]bool{"NewTicker": true, "Tick": true, "After": true, "NewTimer": true, "Sleep": true}

// loopsTimer is the node as a timer call, if it is one. After counts only as a
// channel receive (`<-x.After(d)`); time.Time's After is a comparison, which
// loopsFindTimer screens out by requiring the receive.
func loopsTimer(n ast.Node) (*ast.CallExpr, string) {
	c, ok := n.(*ast.CallExpr)
	if !ok || len(c.Args) == 0 {
		return nil, ""
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || !loopsTimerFuncs[sel.Sel.Name] {
		return nil, ""
	}
	return c, sel.Sel.Name
}

// loopsFindTimer is the first timer in node, in source order, and whether it
// repeats: a ticker always does, a one-shot timer does inside a loop.
func loopsFindTimer(node ast.Node) (*ast.CallExpr, bool) {
	var found *ast.CallExpr
	periodic := false
	var stack []ast.Node
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if found != nil {
			return false
		}
		if c, fn := loopsTimer(n); c != nil {
			parent := ast.Node(nil)
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			if u, ok := parent.(*ast.UnaryExpr); fn != "After" || (ok && u.Op == token.ARROW) {
				found = c
				periodic = fn == "NewTicker" || fn == "Tick"
				for _, a := range stack {
					switch a.(type) {
					case *ast.ForStmt, *ast.RangeStmt:
						periodic = true
					}
				}
				return false
			}
		}
		stack = append(stack, n)
		return true
	})
	return found, periodic
}

// cadence is a goroutine's pace: its own first timer, else the first timer of
// a loop it launches in turn (a Start that spawns its run).
func (w *loopsWalker) cadence(fr *loopsFrame) string {
	if c, periodic := loopsFindTimer(fr.fn.decl.Body); c != nil {
		return loopsPace(w.value(c.Args[0], fr, 0), periodic)
	}
	out := ""
	loopsVisit(fr.fn.decl.Body, func(g *ast.GoStmt) {
		if out != "" {
			return
		}
		if t := w.callee(g.Call.Fun, fr); t.fn != nil {
			tf := w.frame(t.fn, g.Call.Args, fr)
			if c, periodic := loopsFindTimer(t.fn.decl.Body); c != nil {
				out = loopsPace(w.value(c.Args[0], tf, 0), periodic)
			}
		}
	}, func(*ast.CallExpr) {})
	if out == "" {
		return "no ticker"
	}
	return out
}

// litsCadence is the first timer across a function's looping closures, read in
// the function's own frame (a closure shares its scope).
func (w *loopsWalker) litsCadence(lits []*ast.FuncLit, fr *loopsFrame) string {
	for _, lit := range lits {
		if c, periodic := loopsFindTimer(lit.Body); c != nil {
			return loopsPace(w.value(c.Args[0], fr, 0), periodic)
		}
	}
	return "no ticker"
}

func loopsPace(v string, periodic bool) string {
	if periodic {
		return v
	}
	return "once, after " + v
}

// ── resolving names ─────────────────────────────────────────────────────────

// loopsAssign is one assignment to a name: the right-hand side and, for a
// multi-value right-hand side, which result the name takes.
type loopsAssign struct {
	rhs    ast.Expr
	result int
	typ    ast.Expr // a `var x T` declaration's type
	define bool
}

// loopsAssignments is every assignment to name in fn's body, closures
// included, in source order.
func loopsAssignments(fn *ast.FuncDecl, name string) []loopsAssign {
	var out []loopsAssign
	if fn.Body == nil {
		return nil
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok || id.Name != name {
					continue
				}
				a := loopsAssign{define: n.Tok == token.DEFINE}
				if len(n.Rhs) == len(n.Lhs) {
					a.rhs = n.Rhs[i]
				} else if len(n.Rhs) == 1 {
					a.rhs, a.result = n.Rhs[0], i
				}
				out = append(out, a)
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Name != name {
					continue
				}
				a := loopsAssign{typ: n.Type, define: true}
				if i < len(n.Values) {
					a.rhs = n.Values[i]
				}
				out = append(out, a)
			}
		}
		return true
	})
	return out
}

// loopsParam is the index and type of fn's parameter name, or -1.
func loopsParam(fn *ast.FuncDecl, name string) (int, ast.Expr) {
	i := 0
	for _, f := range fn.Type.Params.List {
		if len(f.Names) == 0 {
			i++
			continue
		}
		for _, id := range f.Names {
			if id.Name == name {
				return i, f.Type
			}
			i++
		}
	}
	return -1, nil
}

// isLocal reports whether name is the frame's receiver, a parameter or a local,
// so that it shadows any import of the same name.
func (w *loopsWalker) isLocal(name string, fr *loopsFrame) bool {
	if name == fr.recvName {
		return true
	}
	if i, _ := loopsParam(fr.fn.decl, name); i >= 0 {
		return true
	}
	return len(loopsAssignments(fr.fn.decl, name)) > 0
}

// callee resolves what a call expression calls.
func (w *loopsWalker) callee(fun ast.Expr, fr *loopsFrame) loopsCallee {
	file := fr.fn.file
	switch f := fun.(type) {
	case *ast.Ident:
		if fn := file.pkg.funcs[f.Name]; fn != nil && fn.recv == "" {
			return loopsCallee{fn: fn}
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && !w.isLocal(id.Name, fr) {
			if ip, ok := file.imports[id.Name]; ok {
				if p := w.tree.pkgByImport(ip); p != nil {
					if fn := p.funcs[f.Sel.Name]; fn != nil && fn.recv == "" {
						return loopsCallee{fn: fn}
					}
				}
				return loopsCallee{}
			}
		}
		t := w.typeOf(f.X, fr, 0)
		if !t.ok() {
			return loopsCallee{}
		}
		if t.isInterface() {
			if m := t.interfaceMethod(f.Sel.Name); m != nil {
				return loopsCallee{iface: t, method: f.Sel.Name, field: m}
			}
			return loopsCallee{}
		}
		if fn := t.method(f.Sel.Name); fn != nil {
			return loopsCallee{fn: fn}
		}
	}
	return loopsCallee{}
}

// typeOf is the named type an expression has, as far as syntax can tell.
func (w *loopsWalker) typeOf(e ast.Expr, fr *loopsFrame, depth int) gosrcNamed {
	if depth > 8 {
		return gosrcNamed{}
	}
	file := fr.fn.file
	switch x := e.(type) {
	case *ast.ParenExpr:
		return w.typeOf(x.X, fr, depth+1)
	case *ast.StarExpr:
		return w.typeOf(x.X, fr, depth+1)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return w.typeOf(x.X, fr, depth+1)
		}
	case *ast.CompositeLit:
		return file.namedType(x.Type)
	case *ast.TypeAssertExpr:
		return file.namedType(x.Type)
	case *ast.CallExpr:
		return w.resultType(x, 0, fr)
	case *ast.Ident:
		if x.Name == fr.recvName {
			return fr.recvType
		}
		if i, typ := loopsParam(fr.fn.decl, x.Name); i >= 0 {
			return file.namedType(typ)
		}
		for _, a := range loopsAssignments(fr.fn.decl, x.Name) {
			if a.typ != nil {
				return file.namedType(a.typ)
			}
			if a.rhs == nil {
				continue
			}
			if a.result > 0 {
				if c, ok := a.rhs.(*ast.CallExpr); ok {
					return w.resultType(c, a.result, fr)
				}
				continue
			}
			if t := w.typeOf(a.rhs, fr, depth+1); t.ok() {
				return t
			}
		}
		if v := file.pkg.values[x.Name]; v != nil {
			return w.valueType(v, depth)
		}
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && !w.isLocal(id.Name, fr) {
			if ip, ok := file.imports[id.Name]; ok {
				if p := w.tree.pkgByImport(ip); p != nil {
					if v := p.values[x.Sel.Name]; v != nil {
						return w.valueType(v, depth)
					}
				}
				return gosrcNamed{}
			}
		}
		t := w.typeOf(x.X, fr, depth+1)
		if ft, ffile, _, ok := t.structField(x.Sel.Name); ok {
			return ffile.namedType(ft)
		}
	}
	return gosrcNamed{}
}

// valueType is a package-level value's type: declared, or its initializer's.
func (w *loopsWalker) valueType(v *gosrcValue, depth int) gosrcNamed {
	if v.typ != nil {
		return v.file.namedType(v.typ)
	}
	if c, ok := v.expr.(*ast.CompositeLit); ok {
		return v.file.namedType(c.Type)
	}
	if u, ok := v.expr.(*ast.UnaryExpr); ok && u.Op == token.AND {
		if c, ok := u.X.(*ast.CompositeLit); ok {
			return v.file.namedType(c.Type)
		}
	}
	return gosrcNamed{}
}

// resultType is the named type of a call's result i.
func (w *loopsWalker) resultType(c *ast.CallExpr, i int, fr *loopsFrame) gosrcNamed {
	callee := w.callee(c.Fun, fr)
	var ft *ast.FuncType
	var file *gosrcFile
	switch {
	case callee.fn != nil:
		ft, file = callee.fn.decl.Type, callee.fn.file
	case callee.field != nil:
		ft, _ = callee.field.Type.(*ast.FuncType)
		file = callee.iface.typeDecl().file
	}
	if ft == nil || ft.Results == nil {
		return gosrcNamed{}
	}
	n := 0
	for _, r := range ft.Results.List {
		k := len(r.Names)
		if k == 0 {
			k = 1
		}
		if i < n+k {
			return file.namedType(r.Type)
		}
		n += k
	}
	return gosrcNamed{}
}

// ── rendering a cadence ─────────────────────────────────────────────────────

// loopsUnits are time's duration constants and how a doc writes them.
var loopsUnits = map[string]string{
	"Nanosecond": "ns", "Microsecond": "µs", "Millisecond": "ms",
	"Second": "s", "Minute": "m", "Hour": "h",
}

// loopsDuration is a duration expression written as a value: `60 * time.Second`
// is 60s, `time.Minute` is 1m, and a constant is its value.
func (w *loopsWalker) loopsDuration(e ast.Expr, file *gosrcFile, depth int) (string, bool) {
	if depth > 8 {
		return "", false
	}
	switch x := e.(type) {
	case *ast.ParenExpr:
		return w.loopsDuration(x.X, file, depth+1)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		ip := file.imports[id.Name]
		if ip == "time" {
			if u, ok := loopsUnits[x.Sel.Name]; ok {
				return "1" + u, true
			}
			return "", false
		}
		if p := w.tree.pkgByImport(ip); p != nil {
			if v := p.values[x.Sel.Name]; v != nil && v.expr != nil {
				return w.loopsDuration(v.expr, v.file, depth+1)
			}
		}
	case *ast.Ident:
		if v := file.pkg.values[x.Name]; v != nil && v.expr != nil {
			return w.loopsDuration(v.expr, v.file, depth+1)
		}
	case *ast.BinaryExpr:
		if x.Op != token.MUL {
			return "", false
		}
		lit, unit := x.X, x.Y
		if _, ok := lit.(*ast.BasicLit); !ok {
			lit, unit = x.Y, x.X
		}
		bl, ok := lit.(*ast.BasicLit)
		if !ok || (bl.Kind != token.INT && bl.Kind != token.FLOAT) {
			return "", false
		}
		if u, ok := w.loopsDuration(unit, file, depth+1); ok && strings.HasPrefix(u, "1") {
			return bl.Value + strings.TrimPrefix(u, "1"), true
		}
	}
	return "", false
}

// value renders the duration expression e, read in frame fr.
func (w *loopsWalker) value(e ast.Expr, fr *loopsFrame, depth int) string {
	code := "`" + gosrcExpr(e) + "`"
	if depth > 8 || fr == nil {
		return code
	}
	file := fr.fn.file
	switch x := e.(type) {
	case *ast.ParenExpr:
		return w.value(x.X, fr, depth+1)
	case *ast.Ident:
		return w.identValue(x, fr, depth)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && !w.isLocal(id.Name, fr) {
			if _, ok := file.imports[id.Name]; ok {
				d, ok := w.loopsDuration(x, file, depth)
				switch {
				case !ok:
					return code
				case file.imports[id.Name] == "time":
					return d
				default:
					return d + " (" + code + ")"
				}
			}
		}
		if loopsChain(x) == nil {
			// A field of a call's result: name it by the result's type.
			if t := w.typeOf(x.X, fr, 0); t.ok() {
				return "`" + t.name + "." + x.Sel.Name + "`"
			}
			return code
		}
		return w.selectorValue(x, fr, depth)
	case *ast.CallExpr:
		// An accessor — a method of the receiver, or a closure held in a local —
		// is what it returns.
		switch f := x.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := f.X.(*ast.Ident); ok && id.Name == fr.recvName && fr.recvName != "" {
				if m := fr.recvType.method(f.Sel.Name); m != nil {
					if v := w.returnsValue(m.decl.Body, w.frame(m, x.Args, fr), depth); v != "" {
						return v
					}
				}
			}
		case *ast.Ident:
			for _, a := range loopsAssignments(fr.fn.decl, f.Name) {
				if lit, ok := a.rhs.(*ast.FuncLit); ok {
					if v := w.returnsValue(lit.Body, fr, depth); v != "" {
						return v
					}
				}
			}
		}
		if name := w.firstFuncCalled(x, fr); name != "" {
			return "computed by `" + name + "`"
		}
		return code
	}
	if d, ok := w.loopsDuration(e, file, depth); ok {
		return d
	}
	return code
}

// identValue renders a name: a parameter as its argument, a local as its
// assignment, a package constant as its value, each with its fallbacks.
func (w *loopsWalker) identValue(x *ast.Ident, fr *loopsFrame, depth int) string {
	fn := fr.fn.decl
	var parts []string
	assigns := loopsAssignments(fn, x.Name)
	if i, _ := loopsParam(fn, x.Name); i >= 0 {
		parts = append(parts, w.paramValue(i, fr, depth))
	} else if len(assigns) > 0 {
		if a := assigns[0]; a.rhs != nil && a.result == 0 {
			parts = append(parts, w.value(a.rhs, fr, depth+1))
		} else {
			parts = append(parts, "`"+x.Name+"`")
		}
		assigns = assigns[1:]
	} else if v := fr.fn.file.pkg.values[x.Name]; v != nil {
		if d, ok := w.loopsDuration(v.expr, v.file, depth); ok && v.expr != nil {
			return d + " (`" + x.Name + "`)"
		}
		return "`" + x.Name + "`"
	} else {
		return "`" + x.Name + "`"
	}
	for _, a := range assigns {
		if a.rhs == nil || a.result != 0 || a.define {
			continue
		}
		if p := w.value(a.rhs, fr, depth+1); !loopsHas(parts, p) {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", else ")
}

func loopsHas(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// paramValue is the argument a frame's parameter i was called with: the call
// that reached the frame, else the call in Start or a helper that called the
// function (a constructor whose result Start stored).
func (w *loopsWalker) paramValue(i int, fr *loopsFrame, depth int) string {
	name := "`" + loopsParamName(fr.fn.decl, i) + "`"
	if fr.parent != nil {
		if i < len(fr.args) {
			return w.value(fr.args[i], fr.parent, depth+1)
		}
		return name
	}
	for _, c := range w.calls {
		if callee := w.callee(c.call.Fun, c.fr); callee.fn == fr.fn && i < len(c.call.Args) {
			return w.value(c.call.Args[i], c.fr, depth+1)
		}
	}
	return name
}

func loopsParamName(fn *ast.FuncDecl, i int) string {
	n := 0
	for _, f := range fn.Type.Params.List {
		for _, id := range f.Names {
			if n == i {
				return id.Name
			}
			n++
		}
	}
	return "?"
}

// selectorValue renders a field read through the receiver: a config field as
// its yaml key, a field a constructor sets as what it sets it to, anything
// else as the field path.
func (w *loopsWalker) selectorValue(x *ast.SelectorExpr, fr *loopsFrame, depth int) string {
	chain := loopsChain(x)
	if len(chain) < 2 || chain[0] != fr.recvName || fr.recvName == "" {
		return "`" + gosrcExpr(x) + "`"
	}
	path := fr.recvType.name + "." + strings.Join(chain[1:], ".")
	if len(chain) == 2 {
		if v, vfr := w.fieldInit(fr.recvType, chain[1]); v != nil {
			return w.value(v, vfr, depth+1)
		}
		return "`" + path + "`"
	}
	if key, ok := w.yamlKey(fr.recvType, chain[1:]); ok {
		return "config `" + key + "`"
	}
	return "`" + path + "`"
}

// loopsChain is a selector chain's names, root first: e.cfg.A.B is
// [e cfg A B]. A chain that is not all names is empty.
func loopsChain(e ast.Expr) []string {
	switch x := e.(type) {
	case *ast.Ident:
		return []string{x.Name}
	case *ast.SelectorExpr:
		if c := loopsChain(x.X); c != nil {
			return append(c, x.Sel.Name)
		}
	}
	return nil
}

// yamlKey is the yaml path of fields[1:], read through the struct types from
// t.fields[0]: e.cfg.Staging.SweepInterval is staging.sweep_interval when
// cfg's type tags both fields. ok is false when any field carries no yaml tag.
func (w *loopsWalker) yamlKey(t gosrcNamed, fields []string) (string, bool) {
	ft, file, _, ok := t.structField(fields[0])
	if !ok {
		return "", false
	}
	cur := file.namedType(ft)
	var keys []string
	for _, f := range fields[1:] {
		typ, ffile, tag, ok := cur.structField(f)
		if !ok {
			return "", false
		}
		key := loopsYAMLName(tag)
		if key == "" {
			return "", false
		}
		keys = append(keys, key)
		cur = ffile.namedType(typ)
	}
	return strings.Join(keys, "."), true
}

// loopsYAMLName is a struct tag's yaml name, or "".
func loopsYAMLName(tag string) string {
	for _, part := range strings.Fields(tag) {
		if strings.HasPrefix(part, `yaml:"`) {
			name := strings.SplitN(strings.TrimSuffix(strings.TrimPrefix(part, `yaml:"`), `"`), ",", 2)[0]
			if name != "-" {
				return name
			}
		}
	}
	return ""
}

// fieldInit is the value a composite literal of type t gives field, and the
// frame of the function that writes the literal (a constructor).
func (w *loopsWalker) fieldInit(t gosrcNamed, field string) (ast.Expr, *loopsFrame) {
	if !t.ok() {
		return nil, nil
	}
	for _, f := range t.pkg.files {
		for _, d := range f.ast.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var found ast.Expr
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if found != nil {
					return false
				}
				cl, ok := n.(*ast.CompositeLit)
				if !ok || gosrcBaseTypeName(cl.Type) != t.name {
					return true
				}
				if _, isSel := cl.Type.(*ast.SelectorExpr); isSel {
					return true
				}
				for _, el := range cl.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
							found = kv.Value
							return false
						}
					}
				}
				return true
			})
			if found != nil {
				key := fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					key = gosrcBaseTypeName(fd.Recv.List[0].Type) + "." + key
				}
				return found, w.frame(t.pkg.funcs[key], nil, nil)
			}
		}
	}
	return nil, nil
}

// returnsValue renders what an accessor body returns: its last return
// statement's first result (for an accessor that guards and then returns its
// field, the field), else any earlier return that is a duration written as a
// value (the guard's default). An earlier return that is not a duration (a
// zero meaning "off") is a guard, not a cadence, and is left out.
func (w *loopsWalker) returnsValue(body *ast.BlockStmt, fr *loopsFrame, depth int) string {
	var rets []ast.Expr
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) > 0 {
			rets = append(rets, r.Results[0])
		}
		return true
	})
	if len(rets) == 0 {
		return ""
	}
	parts := []string{w.value(rets[len(rets)-1], fr, depth+1)}
	for _, r := range rets[:len(rets)-1] {
		if d, ok := w.loopsDuration(r, fr.fn.file, depth); ok && !loopsHas(parts, d) {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, ", else ")
}

// firstFuncCalled is the first package-level function of the frame's package
// that an expression calls, which names a cadence that is computed.
func (w *loopsWalker) firstFuncCalled(e ast.Expr, fr *loopsFrame) string {
	name := ""
	ast.Inspect(e, func(n ast.Node) bool {
		if name != "" {
			return false
		}
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok {
				if fn := fr.fn.file.pkg.funcs[id.Name]; fn != nil && fn.recv == "" {
					name = id.Name
				}
			}
		}
		return true
	})
	return name
}
