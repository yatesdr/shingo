package testharness

// core_guard_test.go — NewCore against the plant binary's composition, by
// reading both: cmd/shingocore/main.go and routers.go, and core.go here.
//
// The harness is a copy: main is package main and its tables are built there,
// so nothing outside it can call them (design round 4: they stay). A copy
// drifts unless something compares it, so this does, statement by statement:
//   - the tables: every registration routers.go's buildSubjectRouter,
//     buildProtocolRouter and buildAskServer make, and core.go's functions of
//     the same names, as text; and coreRoutes against routers.go's routed
//     types and subjects;
//   - the composition: every statement of main after eng.Start() that names a
//     part of it (the handler, the data service, the doors, the ask server,
//     the tables, the ingestor, a hook on the engine), up to and with the web
//     layer's router, and NewCore's, through its option checks and the
//     helpers it calls in core.go;
//   - NewTables's statements, read as NewCore's are, against NewCore's.
// A statement on one side and not the other fails, unless mainOnly or
// harnessOnly (coreOnly or tablesOnly) says why, with what to change; and so
// do the statements both make coming in another order (the order is the
// program: the bins feed's subscriptions before the screens',
// TestSubscriberCensus).

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"slices"
	"sort"
	"strings"
	"testing"
)

// mainOnly are main's statements NewCore does not make: a statement's start
// -> why not.
var mainOnly = map[string]string{
	"coreHandler := messaging.NewCoreHandler(db, msgClient,": "no Kafka client: the harness's Bus carries what the outbox holds (harnessOnly has NewCore's line)",
	"coreHandler.DebugLog =":                                 "no debug log",
	"coreHandler.StaleEdgeThreshold =":                       "the stale-edge pass is not run (coreHandler.Start)",
	"coreHandler.Start()":                                    "the stale-edge pass: a loop on a wall-clock ticker",
	"defer coreHandler.Stop()":                               "coreHandler.Start's pair",
	"subjectRouter, err := buildSubjectRouter(":              "NewCore's takes the cell's routes (harnessOnly)",
	"coreDataService.StartHeartbeatMaintenance()":            "a 24-hour loop with no stop of its own: a test would leak it",
	"ingestor.DebugLog =":                                    "no debug log",
	"coreDataService.SetEphemeralTopic(msgClient.":           "the ephemeral topic is the Kafka client's (robot_feed_test sets its own)",
	"eng.SetRobotPositionsPublisher(msgClient.":              "the ephemeral topic is the Kafka client's (robot_feed_test sets its own)",
	"msgClient.SubscribeEphemeral(":                          "the ephemeral topic is the Kafka client's",
	"protoRouter, err := buildProtocolRouter(":               "NewCore's takes the cell's routes and dedup, and no debug log (harnessOnly)",
	"protoRouter.LogRegistration(":                           "a log line",
}

// harnessOnly are NewCore's statements main does not make: a statement's start
// -> why.
var harnessOnly = map[string]string{
	"coreHandler := messaging.NewCoreHandler(db, nil,":                              "main's, with no Kafka client",
	"subjectRouter := buildSubjectRouter(coreDataService, askServer, o)":            "main's, with the cell's routes",
	"protoRouter := buildProtocolRouter(coreHandler, subjectRouter, dedup, nil, o)": "main's, with the cell's routes and dedup option, and no debug log",
	"return &Core{": "the harness's result",
}

// coreOnly are NewCore's statements NewTables does not make, with no engine
// under it: a statement's start -> why.
var coreOnly = map[string]string{
	"coreHandler := messaging.NewCoreHandler(db, nil, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, eng.Dispatcher())": "over the engine's dispatcher (tablesOnly has NewTables's line)",
	"coreDataService.Set": "a hook: each ties the data service to the engine, its config or the ask server",
	"eng.":                "a hook on the engine",
	"if sm := eng.":       "a hook on the engine",
	"doors :=":            "the doors are the engine's",
	"askServer, err :=":   "the ask server answers through the doors",
	"subjectRouter := buildSubjectRouter(coreDataService, askServer, o)": "routes edge.ask to the ask server (tablesOnly has NewTables's line)",
	"ingestor.OnExpired =":                    "no ask server to answer an expired ask",
	"if cfg.Messaging.SigningKey":             "Core's default config signs nothing",
	"handler, stopWeb, err := www.NewRouter(": "the web layer is the engine's",
	"return &Core{":                           "NewCore's result",
}

// tablesOnly are NewTables's statements NewCore does not make: a statement's
// start -> why.
var tablesOnly = map[string]string{
	"coreHandler := messaging.NewCoreHandler(db, nil, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, d)": "NewCore's, over the cell's dispatcher (or none)",
	"subjectRouter := buildSubjectRouter(coreDataService, nil, o)":                                              "NewCore's, with no ask server",
	"return &Tables{": "NewTables's result",
}

// compositionNames are what makes a statement of main's part of the
// composition NewCore copies.
var compositionNames = []string{
	"coreHandler", "coreDataService", "askServer", "subjectRouter", "protoRouter", "ingestor",
	"eng.Set", "eng.Subscribe", "eng.Emit", "doors",
}

func TestNewCore_MakesWhatMainMakes(t *testing.T) {
	t.Parallel()
	routers := parseGo(t, "../cmd/shingocore/routers.go")
	mainFile := parseGo(t, "../cmd/shingocore/main.go")
	harness := parseGo(t, "core.go")

	var diffs []string
	for _, fn := range []string{"buildSubjectRouter", "buildProtocolRouter", "buildAskServer"} {
		want := registrations(t, routers, fn)
		got := registrations(t, harness, fn)
		for _, s := range want {
			if !slices.Contains(got, s) {
				diffs = append(diffs, fmt.Sprintf("routers.go's %s makes\n    %s\n  and core.go's does not: add it there, behind its route", fn, s))
			}
		}
		for _, s := range got {
			if !slices.Contains(want, s) {
				diffs = append(diffs, fmt.Sprintf("core.go's %s makes\n    %s\n  and routers.go's does not: remove it, or make it in routers.go", fn, s))
			}
		}
	}

	routed := routedNames(t, routers)
	listed := coreRouteNames(t, harness)
	for _, n := range routed {
		if !slices.Contains(listed, n) {
			diffs = append(diffs, fmt.Sprintf("routers.go routes %s and coreRoutes (core.go) does not list it: add it, so RoutesOnly can name it", n))
		}
	}
	for _, n := range listed {
		if !slices.Contains(routed, n) {
			diffs = append(diffs, fmt.Sprintf("coreRoutes (core.go) lists %s and routers.go routes no such type or subject: remove it", n))
		}
	}

	mainStmts := mainComposition(t, mainFile)
	coreStmts := statementsOf(t, harness, "NewCore")
	tableStmts := statementsOf(t, harness, "NewTables")
	for _, s := range mainStmts {
		if slices.Contains(coreStmts, s) || reason(mainOnly, s) != "" {
			continue
		}
		diffs = append(diffs, fmt.Sprintf("main makes\n    %s\n  and NewCore does not: make it in NewCore (core.go), behind a Hook if a cell may leave "+
			"it out, or list it in mainOnly with why a harness does not", s))
	}
	for _, s := range coreStmts {
		if slices.Contains(mainStmts, s) || reason(harnessOnly, s) != "" {
			continue
		}
		diffs = append(diffs, fmt.Sprintf("NewCore makes\n    %s\n  and main does not: remove it, or list it in harnessOnly with why", s))
	}
	diffs = append(diffs, staleRows("mainOnly", mainOnly, "main", mainStmts)...)
	diffs = append(diffs, staleRows("harnessOnly", harnessOnly, "NewCore", coreStmts)...)
	if d := orderDiff("main", "NewCore", mainStmts, coreStmts, "for main's order is the bus's (TestSubscriberCensus)"); d != "" {
		diffs = append(diffs, d)
	}
	diffs = append(diffs, tablesDiffs(coreStmts, tableStmts)...)
	if len(mainStmts) == 0 || len(coreStmts) == 0 || len(tableStmts) == 0 {
		t.Fatalf("read %d of main's statements, %d of NewCore's and %d of NewTables's: a guard that reads nothing compares nothing",
			len(mainStmts), len(coreStmts), len(tableStmts))
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("testharness.NewCore or NewTables and the plant binary's composition have parted:\n\n%s", strings.Join(diffs, "\n\n"))
	}
	t.Logf("%d of main's statements, %d made by NewCore, %d main only; %d NewCore only",
		len(mainStmts), len(mainStmts)-countReasons(mainStmts, mainOnly), countReasons(mainStmts, mainOnly), countReasons(coreStmts, harnessOnly))
	t.Logf("%d of NewCore's statements, %d made by NewTables, %d NewCore only; %d NewTables only",
		len(coreStmts), len(coreStmts)-countReasons(coreStmts, coreOnly), countReasons(coreStmts, coreOnly), countReasons(tableStmts, tablesOnly))
}

// tablesDiffs is NewTables against NewCore, as the test holds NewCore against
// main: a statement one makes and the other does not, unless coreOnly or
// tablesOnly says why; a listed row whose statement is gone; and the
// statements both make in another order.
func tablesDiffs(coreStmts, tableStmts []string) []string {
	var diffs []string
	for _, s := range coreStmts {
		if !slices.Contains(tableStmts, s) && reason(coreOnly, s) == "" {
			diffs = append(diffs, fmt.Sprintf("NewCore makes\n    %s\n  and NewTables does not: make it in NewTables (core.go), "+
				"or list it in coreOnly with why a Core with no engine does not", s))
		}
	}
	for _, s := range tableStmts {
		if !slices.Contains(coreStmts, s) && reason(tablesOnly, s) == "" {
			diffs = append(diffs, fmt.Sprintf("NewTables makes\n    %s\n  and NewCore does not: make it as NewCore makes it, or list it in tablesOnly with why", s))
		}
	}
	diffs = append(diffs, staleRows("coreOnly", coreOnly, "NewCore", coreStmts)...)
	diffs = append(diffs, staleRows("tablesOnly", tablesOnly, "NewTables", tableStmts)...)
	if d := orderDiff("NewCore", "NewTables", coreStmts, tableStmts, "for NewCore's order is main's"); d != "" {
		diffs = append(diffs, d)
	}
	return diffs
}

// staleRows are the rows of list whose statement maker no longer makes: a
// listed reason whose statement is gone is a stale list.
func staleRows(list string, rows map[string]string, maker string, stmts []string) []string {
	var out []string
	for start := range rows {
		if !slices.ContainsFunc(stmts, func(s string) bool { return strings.HasPrefix(s, start) }) {
			out = append(out, fmt.Sprintf("%s lists %q and %s makes no such statement: remove the row", list, start, maker))
		}
	}
	return out
}

type goFile struct {
	fset *token.FileSet
	file *ast.File
}

func parseGo(t *testing.T, path string) goFile {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return goFile{fset, f}
}

func (g goFile) fn(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range g.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Body != nil {
			return fd
		}
	}
	t.Fatalf("%s has no func %s", g.fset.File(g.file.Pos()).Name(), name)
	return nil
}

// text is a node as gofmt prints it, on one line.
func (g goFile) text(n ast.Node) string {
	var b bytes.Buffer
	if err := printer.Fprint(&b, g.fset, n); err != nil {
		return fmt.Sprintf("<%v>", err)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// registrations are the calls of a table builder that register something:
// router.Register*, a UseFor, an ask server's Register.
func registrations(t *testing.T, g goFile, fn string) []string {
	t.Helper()
	var out []string
	ast.Inspect(g.fn(t, fn).Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (strings.HasPrefix(sel.Sel.Name, "Register") || sel.Sel.Name == "UseFor") {
			out = append(out, g.text(call))
			return false
		}
		return true
	})
	return out
}

// routedNames are the types and subjects routers.go registers a handler that
// does something for: not the data envelope (the subject table's door) and
// not a no-op (a reply type Core only sends).
func routedNames(t *testing.T, g goFile) []string {
	t.Helper()
	var out []string
	for _, fn := range []string{"buildSubjectRouter", "buildProtocolRouter"} {
		ast.Inspect(g.fn(t, fn).Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || !strings.HasPrefix(sel.Sel.Name, "Register") {
				return true
			}
			if lit, ok := call.Args[2].(*ast.FuncLit); ok && len(lit.Body.List) == 0 {
				return false // a no-op
			}
			if name := g.text(call.Args[1]); name != "protocol.TypeData" {
				out = append(out, name)
			}
			return false
		})
	}
	return out
}

// coreRouteNames are coreRoutes' elements as written.
func coreRouteNames(t *testing.T, g goFile) []string {
	t.Helper()
	for _, d := range g.file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "coreRoutes" || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatal("coreRoutes is not a slice literal")
			}
			var out []string
			for _, e := range lit.Elts {
				out = append(out, g.text(e))
			}
			return out
		}
	}
	t.Fatal("core.go declares no coreRoutes")
	return nil
}

// mainComposition is every statement of main's body after eng.Start() that
// names a part of the composition, up to and with the router's build, but the
// error checks.
func mainComposition(t *testing.T, g goFile) []string {
	t.Helper()
	var out []string
	in := false
	for _, st := range g.fn(t, "main").Body.List {
		s := g.text(st)
		if s == "eng.Start()" {
			in = true
			continue
		}
		if in && !errCheck(st) && names(s) {
			out = append(out, s)
		}
		if in && strings.HasPrefix(s, "handler, stopWeb, err := www.NewRouter(") {
			return out
		}
	}
	t.Fatal("main.go: no eng.Start() followed by the router's build")
	return nil
}

// statementsOf is the statements of fn (NewCore, NewTables) that name a part
// of the composition, through its option checks (an if on the options is read
// as its body) and the helpers it calls in core.go (a call that is a statement
// of its own is read as the helper's body), but the error checks.
func statementsOf(t *testing.T, g goFile, fn string) []string {
	t.Helper()
	var out []string
	var walk func(list []ast.Stmt)
	walk = func(list []ast.Stmt) {
		for _, st := range list {
			if ifs, ok := st.(*ast.IfStmt); ok && ifs.Init == nil && ifs.Else == nil && optionCheck(g.text(ifs.Cond)) {
				walk(ifs.Body.List)
				continue
			}
			if fd := g.helperCall(st); fd != nil {
				walk(fd.Body.List)
				continue
			}
			if s := g.text(st); !errCheck(st) && names(s) {
				out = append(out, s)
			}
		}
	}
	walk(g.fn(t, fn).Body.List)
	return out
}

// helperCall is the func of this file a statement calls, when the statement is
// that call alone; nil otherwise.
func (g goFile) helperCall(st ast.Stmt) *ast.FuncDecl {
	es, ok := st.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return nil
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil
	}
	for _, d := range g.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == id.Name && fd.Body != nil {
			return fd
		}
	}
	return nil
}

// orderDiff is "" when the statements ref and its copy both make come in the
// same order on both sides, else the first that does not, with what to change
// and why the order matters.
func orderDiff(refName, copyName string, refStmts, copyStmts []string, why string) string {
	var inRef, inCopy []string
	for _, s := range refStmts {
		if slices.Contains(copyStmts, s) {
			inRef = append(inRef, s)
		}
	}
	for _, s := range copyStmts {
		if slices.Contains(refStmts, s) {
			inCopy = append(inCopy, s)
		}
	}
	for i := range inRef {
		if i >= len(inCopy) || inRef[i] != inCopy[i] {
			got := "nothing"
			if i < len(inCopy) {
				got = inCopy[i]
			}
			return fmt.Sprintf("%s makes\n    %s\n  where %s makes\n    %s\n  %s makes their statements in another order: "+
				"move it in %s (core.go) to %s's place, %s", refName, inRef[i], copyName, got, copyName, copyName, refName, why)
		}
	}
	return ""
}

func optionCheck(cond string) bool {
	return strings.HasPrefix(cond, "o.") || strings.HasPrefix(cond, "!o.")
}

func errCheck(st ast.Stmt) bool {
	ifs, ok := st.(*ast.IfStmt)
	if !ok {
		return false
	}
	b, ok := ifs.Cond.(*ast.BinaryExpr)
	return ok && b.Op == token.NEQ && fmt.Sprint(b.X) == "err"
}

func names(s string) bool {
	return slices.ContainsFunc(compositionNames, func(n string) bool { return strings.Contains(s, n) })
}

func reason(list map[string]string, s string) string {
	for start, why := range list {
		if strings.HasPrefix(s, start) {
			return why
		}
	}
	return ""
}

func countReasons(stmts []string, list map[string]string) int {
	n := 0
	for _, s := range stmts {
		if reason(list, s) != "" {
			n++
		}
	}
	return n
}
