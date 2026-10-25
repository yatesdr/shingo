package dispatch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"shingocore/internal/testdb"
)

// scanCoreSources returns every non-test .go file in shingo-core, as
// (path relative to the module root, contents).
//
// This used to list six directories by hand. That made the census answer a
// narrower question than it claimed to: not "who creates orders" but "who
// creates orders in the six places we already knew about." A writer added
// anywhere else — fulfillment, messaging, material, a new package — was
// invisible, and the test would have gone on passing while reporting
// completeness. Widening it to the whole module found a real file the hand
// list had missed on the first run.
//
// Package dir is the test's CWD, so ".." is the module root regardless of
// where `go test` ran from.
func scanCoreSources(t *testing.T) map[string]string {
	t.Helper()
	const root = ".."
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under these creates an order, and walking them costs
			// more than the rest of the tree put together.
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", "build", "deploy", "docs", "scripts":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		// Shared test fixtures live in ordinary .go files so several packages
		// can import them, so the _test.go suffix does not catch them. They are
		// not doors: nothing in production reaches them. internal/testdb is the
		// one today, and widening the walk is what surfaced it. Importing
		// "testing" is the reliable tell — production code never does.
		if strings.Contains(string(b), "\"testing\"") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestCensus_OrdersTableInsertStatements pins that exactly ONE SQL statement
// inserts into the orders table.
//
// There used to be two. The shared writer carried 21 data columns and a second
// statement inside CreateCompoundChildren carried 16, so five columns were
// silently dropped on the path that writes compound children — the rows the
// demand grain counts. Nothing enforced the two lists staying in step; it
// depended on whoever added the next column having read a comment.
//
// Now there is one statement, so a column added to it cannot be missed by
// another. If this test fails because you added a second INSERT, the question to
// answer first is whether it can go through orders.Create instead — it takes a
// QueryRower, so it works inside a transaction.
func TestCensus_OrdersTableInsertStatements(t *testing.T) {
	t.Parallel()
	const want = 1
	var sites []string
	for p, src := range scanCoreSources(t) {
		for i, line := range strings.Split(src, "\n") {
			// Skip comments: store/orders.go carries a prose warning about the
			// second INSERT, and counting it would report a hazard that is
			// actually the documentation of the hazard.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "INSERT INTO orders ") {
				sites = append(sites, p+":"+strconv.Itoa(i+1))
			}
		}
	}
	slices.Sort(sites)
	if len(sites) != want {
		t.Errorf("INSERT INTO orders statements = %d, want %d.\nEvery column on the orders table must be added to ALL of them or deliberately omitted from all.\nSites:\n  %s",
			len(sites), want, strings.Join(sites, "\n  "))
	}
}

// ── The census of issuers: who brings an order row into existence ─────────────
//
// THE UNIT IS A CALLER, found on go/ast, not a file or a grepped symbol. The
// census this replaced listed doors by file and asked "does this file call a
// writer symbol". It could not see a second caller of a door that already
// existed, and two arrived that way: stage 2's pull reaches the person's bin
// move (engine.CreateBinMove), and the cancel's return reaches the carried-bin
// recovery (engine.orderCarriedBinDown). Both stood on no list. What this asks
// instead is the call graph above the one INSERT: every function on the walk up
// from orders.Create is named here, with its place on the walk, so a new caller
// anywhere on it fails until somebody names it.
//
// Calls are matched by name and by how the receiver is spelt (callGraph), not
// by type: the tree has more than one function called birth, admit or Build, and
// the spelling is what tells them apart. A walk entry that names its callee's
// spelling wrongly reports itself as unreached, never as silently passed.
//
// What each door's order HOLDS at its issue is the docker arm's question
// (TestIssueCensus_AtCommit in this package and in engine), read from the one
// table both arms share: testdb.IssueDoors.

// issueRole is a function's place on the walk up from the INSERT.
type issueRole int

const (
	// issueFace writes the row or is the issue's own face: the builder and the
	// store's writers. The walk goes on from it.
	issueFace issueRole = iota
	// issueSite calls the issue: the census row (testdb.IssueDoors names the
	// doors it serves). The walk goes on from it.
	issueSite
	// issueThrough is a door's body between its entry and the issue. The walk
	// goes on from it.
	issueThrough
	// issueOpener is where a door is entered: a person's page, the wire, a
	// timer, a trigger. The walk stops; who reaches an opener is the door's own
	// business (TestCensus_PersonDoors names the pages).
	issueOpener
)

// issueEntry is one function on the walk: its place, how a call to it is spelt
// (the receiver's last name, "" for a bare call), and what it is.
type issueEntry struct {
	role  issueRole
	spelt []string
	what  string
}

// insertCallers are the production functions calling orders.Create. The end
// state of the one issue is one: the issue itself.
var insertCallers = map[string]string{
	"dispatch/builder.go:issue":      "the issue: a door's need and the planner's plan, as the row and its holds",
	"store/orders.go:DB.CreateOrder": "the store's own writer, in a transaction of its own: the tests' fixtures, no door's",
}

// issueWalk is every function on the walk up from orders.Create.
var issueWalk = map[string]issueEntry{
	// The faces: the builder and the store's writers.
	"dispatch/builder.go:issue":                 {issueFace, []string{""}, "the issue over the one INSERT, on the caller's transaction"},
	"dispatch/builder.go:issueOwnTx":            {issueFace, []string{""}, "the issue in a transaction of its own"},
	"dispatch/builder.go:Dispatcher.IssueOrder": {issueFace, []string{"d", "dispatcher"}, "the issue's face: issueOwnTx on the Dispatcher's database"},
	"store/orders.go:DB.CreateOrder":            {issueFace, []string{"db"}, "the store's writer in a transaction of its own"},

	// The sites: each calls the issue, and testdb.IssueDoors says what its
	// doors' orders hold at commit.
	"dispatch/lifecycle_service.go:LifecycleService.issue":            {issueSite, []string{"s", "lifecycle"}, "admission's issue: the wire's orders and the Core asks"},
	"dispatch/complex_intake.go:Dispatcher.handleComplexOrderRequest": {issueSite, []string{"d"}, "complex intake"},
	"dispatch/compound.go:Dispatcher.issueLegsTx":                     {issueSite, []string{"d"}, "a dig's legs, on the compound's transaction"},
	"dispatch/compound.go:Dispatcher.writeCompoundChildren":           {issueThrough, []string{"d"}, "the dig's issue: its transaction and its after-commit tail"},
	"dispatch/loader_window.go:Dispatcher.windowMove":                 {issueSite, []string{"d"}, "a window pull's move: the drain's and stage 2's"},
	"engine/bin_move.go:Engine.CreateBinMove":                         {issueSite, []string{"e", "orchestration"}, "a bin move"},
	"engine/carried_bin_recovery.go:Engine.issueAndDispatchRecovery":  {issueSite, []string{"e"}, "the carried-bin recovery's unit"},
	"dispatch/clear_home.go:Dispatcher.IssueClearTx":                  {issueSite, []string{"dispatcher"}, "the home clear: its parent and the partial's move"},
	"dispatch/clear_home.go:Dispatcher.issueClearSpot":                {issueSite, []string{"d"}, "the home clear: the empty's move to the open spot"},
	"dispatch/clear_home.go:Dispatcher.issueClearTrade":               {issueSite, []string{"d"}, "the home clear: the trade's two moves"},

	// A door's body on the way: the walk goes on through it.
	"dispatch/loader_replenish.go:Dispatcher.AdmitCoreAsk":      {issueThrough, []string{"d", "dispatcher"}, "the Core ask through admission"},
	"engine/carried_bin_recovery.go:Engine.orderCarriedBinDown": {issueThrough, []string{"e"}, "the recovery's door body, which two openers reach"},
	"dispatch/loader_window.go:Dispatcher.pullInto":             {issueThrough, []string{"d"}, "the window pulls' loop: a move per free window"},

	// The openers: where a door is entered.
	"dispatch/lifecycle_service.go:LifecycleService.CreateInboundOrder": {issueOpener, nil, "the wire intake (a plain order or a named move)"},
	"dispatch/loader_replenish.go:Dispatcher.admitReplenishOrder":       {issueOpener, nil, "loader replenish: the loader's ask"},
	"engine/maintainer.go:Maintainer.createAsks":                        {issueOpener, nil, "the maintained-group level keeper's ask"},
	"dispatch/complex_intake.go:Dispatcher.HandleComplexOrderRequest":   {issueOpener, nil, "complex intake's door, under the scan lock"},
	"dispatch/compound.go:Dispatcher.createCompoundOrder":               {issueOpener, nil, "a dig's compound, every dig planner's"},
	"dispatch/loader_drain.go:Dispatcher.drainLocked":                   {issueOpener, nil, "the loader drain's pass, under the loader's lock"},
	"www/handlers_orders.go:Handlers.fileBinMove":                       {issueOpener, nil, "a person's bin move: the Orders and Bins pages"},
	"www/handlers_test_orders_direct.go:Handlers.apiDirectOrderSubmit":  {issueOpener, nil, "a person's bin move: the /test-orders direct form"},
	"dispatch/loader_window.go:Dispatcher.PullStage2":                   {issueOpener, nil, "stage 2's pull, under stage 2's loader lock"},
	"engine/carried_bin_recovery.go:Engine.RecoverCarriedBin":           {issueOpener, nil, "the Recover button on an anomaly"},
	"engine/cancel_return.go:Engine.tryReturnCarriedBin": {issueOpener, nil, "THE CANCEL'S RETURN: an order cancelled while its robot " +
		"carries a bin opens the recovery's door with nobody pressing it"},
	"engine/clear_home.go:Engine.issueClear":              {issueOpener, nil, "the home clear's door, in the station's transaction"},
	"dispatch/clear_home.go:Dispatcher.ChooseClearLocked": {issueOpener, nil, "the home clear's choice at confirm: the spot arm"},
	"dispatch/clear_home.go:Dispatcher.chooseClearTrade":  {issueOpener, nil, "the home clear's choice at confirm: the trade"},
}

// callee is one call in a function's body: the receiver's last name ("" for a
// bare call) and the called name.
type callee struct{ recv, name string }

// callGraph is every declared function, "file:Recv.Func", and the calls in its
// body, closures included.
type callGraph map[string][]callee

func newCallGraph(t *testing.T, sources map[string]string) callGraph {
	t.Helper()
	g := callGraph{}
	fset := token.NewFileSet()
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := path + ":" + fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) > 0 {
				rt := fd.Recv.List[0].Type
				if st, ok := rt.(*ast.StarExpr); ok {
					rt = st.X
				}
				if id, ok := rt.(*ast.Ident); ok {
					key = path + ":" + id.Name + "." + fd.Name.Name
				}
			}
			ast.Inspect(fd.Body, func(x ast.Node) bool {
				call, ok := x.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					g[key] = append(g[key], callee{lastName(fn.X), fn.Sel.Name})
				case *ast.Ident:
					g[key] = append(g[key], callee{"", fn.Name})
				}
				return true
			})
		}
	}
	return g
}

// callersOf is every function calling name spelt through one of recvs.
func (g callGraph) callersOf(name string, recvs []string) []string {
	var out []string
	for k, calls := range g {
		if slices.ContainsFunc(calls, func(c callee) bool { return c.name == name && slices.Contains(recvs, c.recv) }) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// funcName is a walk key's bare function name: "issue" for "dispatch/builder.go:issue".
func funcName(key string) string {
	return key[strings.LastIndexAny(key, ":.")+1:]
}

// walkIssuers walks up from orders.Create and reports every function reached
// that the walk does not name, and every named function the walk did not reach.
func walkIssuers(g callGraph, walk map[string]issueEntry) (unnamed, unreached []string) {
	reached := map[string]bool{}
	queue := g.callersOf("Create", []string{"orders"})
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if reached[k] {
			continue
		}
		reached[k] = true
		e, ok := walk[k]
		if !ok {
			unnamed = append(unnamed, k)
			continue
		}
		if e.role != issueOpener {
			queue = append(queue, g.callersOf(funcName(k), e.spelt)...)
		}
	}
	for k := range walk {
		if !reached[k] {
			unreached = append(unreached, k)
		}
	}
	slices.Sort(unnamed)
	slices.Sort(unreached)
	return unnamed, unreached
}

// TestCensus_Issuers pins every way an order comes to exist, by its callers.
//
// Three things are asserted: who calls the one INSERT (insertCallers; the end
// state of the one issue is one caller, the issue); that every function on the
// walk up from it is named, and every name is still on the walk (issueWalk);
// and that the census table both arms read (testdb.IssueDoors) names the walk's
// sites and nothing else.
func TestCensus_Issuers(t *testing.T) {
	t.Parallel()
	g := newCallGraph(t, scanCoreSources(t))

	sameSites(t, "the callers of the one INSERT (orders.Create)",
		g.callersOf("Create", []string{"orders"}), insertCallers,
		"The issue is the one way a row is written. A new caller of orders.Create is a second issue: route it through the one there is.")

	// THE STORE'S OWN WRITER HAS NO PRODUCTION CALLER: every door issues through
	// the issue. Tests write their fixtures' rows with it (testdb.CreateOrder),
	// and those are not doors.
	if callers := g.callersOf("CreateOrder", []string{"db"}); len(callers) != 0 {
		t.Errorf("store.DB.CreateOrder is called by %v: a door writing its row through the store, not the issue. "+
			"Issue it (IssueOrder, or issue on the caller's transaction).", callers)
	}

	unnamed, unreached := walkIssuers(g, issueWalk)
	if len(unnamed) > 0 {
		t.Errorf("these reach the issue and are not named on the walk: %s\n"+
			"Name each in issueWalk with its place (a site that issues, a door's body, or the opener where the door is entered).\n"+
			"A new site is a census row: add it to testdb.IssueDoors with what its order holds at commit, and answer three questions in the same commit:\n"+
			"  1. Does it project to the Edge? (only what routes through admission does)\n"+
			"  2. Does it need the dropoff-capacity gate?\n"+
			"  3. What origin_class do its rows carry? A blank is a row nothing can explain later.",
			strings.Join(unnamed, ", "))
	}
	if len(unreached) > 0 {
		t.Errorf("these are named on the walk and no longer reach the issue: %s\n"+
			"If a door moved, name where; if it merged into another, delete the entry and widen that one.",
			strings.Join(unreached, ", "))
	}

	sites := map[string]bool{}
	for k, e := range issueWalk {
		if e.role == issueSite {
			sites[k] = true
		}
	}
	served := map[string]bool{}
	for _, d := range testdb.IssueDoors {
		if !sites[d.Site] {
			t.Errorf("census door %q names site %s, which the walk does not name as a site", d.Name, d.Site)
		}
		served[d.Site] = true
	}
	for s := range sites {
		if !served[s] {
			t.Errorf("site %s issues orders and no census door names it (testdb.IssueDoors)", s)
		}
	}
}

// TestCensus_IssuersSeeANewCaller is the census pointed at itself: a function
// that starts calling the issue, or a door's entry, must fail the walk until it
// is named. Against the real tree that cannot be shown (every caller there is
// named), so the walk runs here over a synthetic tree.
func TestCensus_IssuersSeeANewCaller(t *testing.T) {
	t.Parallel()
	walk := map[string]issueEntry{
		"dispatch/builder.go:Build":  {issueFace, []string{""}, "the builder"},
		"dispatch/door.go:openIt":    {issueSite, []string{""}, "a site"},
		"www/page.go:Handlers.press": {issueOpener, nil, "an opener"},
	}
	sources := map[string]string{
		"dispatch/builder.go": "package dispatch\n\nfunc Build() { orders.Create(nil, nil) }\n",
		"dispatch/door.go":    "package dispatch\n\nfunc openIt() { Build() }\n",
		"www/page.go":         "package www\n\nfunc (h *Handlers) press() { openIt() }\n",
		"dispatch/new.go":     "package dispatch\n\nfunc brandNew() { Build() }\n",
		"engine/timer.go":     "package engine\n\nfunc tick() { openIt() }\n",
	}
	unnamed, unreached := walkIssuers(newCallGraph(t, sources), walk)
	want := []string{"dispatch/new.go:brandNew", "engine/timer.go:tick"}
	if !slices.Equal(unnamed, want) || len(unreached) != 0 {
		t.Errorf("unnamed = %v, unreached = %v; want unnamed %v and none unreached.\n"+
			"A new caller of the issue, and a new caller of a site, must both be reported.", unnamed, unreached, want)
	}
}

// TestCensus_OrderReceivedAtBirth asks every census door one more question:
// does the order report its receipt (EmitOrderReceived) when it is issued?
// Everything that hears an order arrive hangs off that event — the bins feed's
// push of the node it delivers to (cmd/shingocore/bins_feed.go), the audit row,
// Core's own pages — so a door that does not emit is seen by them at dispatch,
// not at its issue. The comment that said "a Core-originated order emits its own
// event" was false for every Core door until AdmitCoreAsk emitted
// (TestPinCoreAsk_TheAskIsReceivedAtBirth). The answer per door is stated here;
// a new door must give one.
func TestCensus_OrderReceivedAtBirth(t *testing.T) {
	t.Parallel()
	// door -> the file whose source must (emitsIn != "") or must not (emitsIn
	// == "") carry the emit, and why. A door without an emit is asked of its
	// site's file.
	answers := map[string]struct{ emitsIn, why string }{
		"wire plain order":              {"dispatch/lifecycle_service.go", "the Edge wire intake: receipt of a request, with the sender's values"},
		"wire named move":               {"dispatch/lifecycle_service.go", "the same receipt"},
		"complex intake":                {"dispatch/complex_intake.go", "a multi-leg Edge request: the same receipt"},
		"loader replenish":              {"dispatch/loader_replenish.go", "AdmitCoreAsk, the loader replenishment's ask: Core's part and window"},
		"maintained-group level keeper": {"dispatch/loader_replenish.go", "the keeper reaches the orders table through AdmitCoreAsk"},
		"dig legs":                      {"", "a dig's children: internal moves under a parent already received; seen at dispatch"},
		"person's bin move":             {"", "a person moving one bin from Core's pages; no Edge asked; seen at dispatch"},
		"stage 2's pull":                {"", "a pull Core makes; no Edge asked; seen at dispatch"},
		"carried-bin recovery":          {"", "Core putting a carried bin down; no Edge asked; seen at dispatch"},
		"the cancel's return":           {"", "the same, with nobody pressing anything"},
		"home clear":                    {"", "the home clear: the door's own bin push (engine/clear_home.go notifyBinChanged) rebuilds the home's watchers at its issue"},
		"home clear's child":            {"", "the same"},
		"home clear's spot move":        {"", "the same"},
		"home clear's trade":            {"", "the same"},
		"loader drain":                  {"", "a drain's move: Core-born, no Edge asked, as the bin move is; seen at dispatch"},
	}
	sources := scanCoreSources(t)
	for _, d := range testdb.IssueDoors {
		a, ok := answers[d.Name]
		if !ok {
			t.Errorf("census door %q does not say whether its order is received at its issue", d.Name)
			continue
		}
		site := d.Site[:strings.Index(d.Site, ":")]
		emits := strings.Contains(sources[a.emitsIn], "EmitOrderReceived(")
		switch {
		case a.emitsIn != "" && !emits:
			t.Errorf("%s: %s no longer emits EmitOrderReceived (%s)", d.Name, a.emitsIn, a.why)
		case a.emitsIn == "" && strings.Contains(sources[site], "EmitOrderReceived("):
			t.Errorf("%s: %s now emits EmitOrderReceived; move it to the emitting answers (was: %s)", d.Name, site, a.why)
		}
	}
}
