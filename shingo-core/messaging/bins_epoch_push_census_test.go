package messaging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// bins_epoch_push_census_test.go — every generation bump reaches a bins push,
// or is named here as one that does not.
//
// WHY. The bins row carries the resident carrier's epoch (bins.delta_epoch).
// A bump that reaches no push leaves every watching Edge on the old epoch until
// its next heartbeat compare, and a write that carries that epoch as its
// assumption is refused for a change the Edge was never told about. So each
// bump is followed from its site up the call graph to the push that tells the
// Edges, or to the entry point where the act ends with no push.
//
// WHAT COUNTS AS A PUSH, each wired to the feed in cmd/shingocore (pinned by
// TestWireBinsFeed_EventsReachTheHooks and SetBinChangedFunc):
//   - EventBinUpdated emitted on the bus (BinsChanged by bin and nodes);
//   - the bin manifest service's notifyBinChanged, or NotifyBinChanged for a
//     writer outside it (BinsChanged by bin);
//   - EventBinEnteredTransit emitted on the bus at the lift (BinsChanged by
//     bin and the node it left);
//   - EmitOrderDispatched (EventOrderDispatched -> BinsOrderStarted by the
//     order's source and delivery names: the claimed bin stands at the source).
//
// HOW. The module's non-test sources are parsed; a call or a method value is
// resolved by name: pkg.F to that package's F, a bare F to the same package's
// F, x.M to every method named M (so an interface call reaches every
// implementation, an over-approximation that can only add callers). Every
// non-terminal node's production callers must be exactly the ones named. A new
// bump site, a new caller on a path, or a door that stops pushing fails here.
// What it cannot see: the order of the push and the bump inside one door body.

// bumpSites is every function that bumps a bin's generation, with how many
// times: the s.bumpEpoch call sites in service/bin_manifest.go, and the lift,
// whose own UPDATE carries the bump (store/audit TestTheLiftIsTheOneUnannouncedBump).
var bumpSites = map[string]int{
	"service:BinManifestService.ClearForReuseTx":           1,
	"service:BinManifestService.RebaseAfterEdgeRollbackTx": 1,
	"service:BinManifestService.writeProductionTx":         1,
	"service:BinManifestService.clearAndClaimTx":           1,
	"service:BinManifestService.syncUOPAndClaimTx":         1,
	"service:BinManifestService.syncOrClearForReleased":    2, // released-empty and released-partial, unfenced only
	"store/bins:LiftToTransitTx":                           1,
}

// censusNode is one function on a path from a bump to its end. Exactly one of
// callers, push, entry or unreached is set.
type censusNode struct {
	callers []string // the production callers, each a node of its own
	push    string   // a door: the push kind it makes, directly or through via
	via     string   // the helper holding the push, "" when the door's own body does
	entry   string   // an entry point whose act ends with no bins push: why
	unreach string   // no production caller: what calls it
}

var bumpPushCensus = map[string]censusNode{
	// Clear for reuse.
	"service:BinManifestService.ClearForReuseTx": {callers: []string{
		"engine:Engine.ClearForReuseStampAndBookDeparture", "engine:Engine.issueClear",
		"service:BinManifestService.ClearForReuseResolved", "uop:InventoryDeltaService.ApplyBinUOPDelta"}},
	"engine:Engine.ClearForReuseStampAndBookDeparture": {push: pushBinsChanged},
	"engine:Engine.issueClear":                         {callers: []string{"engine:Engine.clearLoaderHomeLocked"}},
	"engine:Engine.clearLoaderHomeLocked":              {push: pushBinsChanged},
	"service:BinManifestService.ClearForReuseResolved": {callers: []string{
		"engine:Engine.ClearForReuseStampAndBookDeparture", "service:BinManifestService.ClearForReuse"}},
	"service:BinManifestService.ClearForReuse":   {callers: []string{"www:Handlers.binClear"}},
	"www:Handlers.binClear":                      {push: pushBinUpdated, via: "www:Handlers.emitBinUpdate"},
	"uop:InventoryDeltaService.ApplyBinUOPDelta": {push: pushBinsChanged},

	// The rebase after an Edge's count stream went backward.
	"service:BinManifestService.RebaseAfterEdgeRollbackTx": {callers: []string{"uop:InventoryDeltaService.recordEdgeRollback"}},
	"uop:InventoryDeltaService.recordEdgeRollback":         {push: pushBinsChanged},

	// Contents written: the Edge's LOAD, the admin Load Payload, an ingest.
	"service:BinManifestService.writeProductionTx": {callers: []string{
		"service:BinManifestService.recordProduced", "service:BinManifestService.setForProductionTx"}},
	"service:BinManifestService.recordProduced": {callers: []string{
		"service:BinManifestService.LoadAtLine", "service:BinManifestService.RecordProducedBin"}},
	"service:BinManifestService.LoadAtLine":         {push: pushBinsChanged},
	"service:BinManifestService.RecordProducedBin":  {push: pushBinsChanged},
	"service:BinManifestService.setForProductionTx": {callers: []string{"service:BinManifestService.SetForProduction"}},
	"service:BinManifestService.SetForProduction":   {callers: []string{"service:BinManifestService.SetFromTemplate"}},
	"service:BinManifestService.SetFromTemplate":    {callers: []string{"service:BinService.LoadPayload"}},
	"service:BinService.LoadPayload":                {callers: []string{"www:Handlers.binLoadPayload"}},
	"www:Handlers.binLoadPayload":                   {push: pushBinUpdated, via: "www:Handlers.emitBinUpdate"},

	// The dispatch claim: the bin is cleared or synced as it is claimed.
	"service:BinManifestService.clearAndClaimTx": {callers: []string{
		"service:BinManifestService.ClearAndClaim", "service:BinManifestService.claimUnderReservationTx"}},
	"service:BinManifestService.syncUOPAndClaimTx": {callers: []string{
		"service:BinManifestService.SyncUOPAndClaim", "service:BinManifestService.claimUnderReservationTx"}},
	"service:BinManifestService.ClearAndClaim":           {unreach: "tests only"},
	"service:BinManifestService.SyncUOPAndClaim":         {unreach: "tests only"},
	"service:BinManifestService.claimUnderReservationTx": {callers: []string{"service:BinManifestService.ConfirmClaimTx"}},
	"service:BinManifestService.ConfirmClaimTx": {callers: []string{
		"dispatch:Dispatcher.takeHandTx", "service:BinManifestService.claimAndConfirm"}},
	"service:BinManifestService.claimAndConfirm": {callers: []string{
		"service:BinManifestService.ClaimForDispatch", "service:BinManifestService.ConfirmClaim"}},
	"service:BinManifestService.ClaimForDispatch": {unreach: "tests only"},
	"service:BinManifestService.ConfirmClaim":     {callers: []string{"dispatch:Allocator.confirmComplexPlan"}},
	"dispatch:Allocator.confirmComplexPlan":       {unreach: "tests only"},
	"dispatch:Dispatcher.takeHandTx":              {callers: []string{"dispatch:Dispatcher.acquireHands"}},
	"dispatch:Dispatcher.acquireHands": {callers: []string{
		"dispatch:Dispatcher.acquireHand", "dispatch:Dispatcher.commitPairToFleet"}},
	"dispatch:Dispatcher.acquireHand": {callers: []string{"dispatch:Dispatcher.commitToFleet"}},
	"dispatch:Dispatcher.commitToFleet": {callers: []string{
		"dispatch:Dispatcher.dispatchComplexToFleet", "dispatch:Dispatcher.dispatchGated",
		"dispatch:Dispatcher.dispatchHeldTrade", "dispatch:Dispatcher.dispatchToFleetCore"}},
	"dispatch:Dispatcher.commitPairToFleet":      {push: pushOrderDispatched, via: "dispatch:Dispatcher.afterComplexCreate"},
	"dispatch:Dispatcher.dispatchComplexToFleet": {push: pushOrderDispatched, via: "dispatch:Dispatcher.afterComplexCreate"},
	"dispatch:Dispatcher.dispatchGated":          {push: pushOrderDispatched, via: "dispatch:Dispatcher.afterGatedCreate"},
	"dispatch:Dispatcher.dispatchHeldTrade":      {push: pushOrderDispatched},
	"dispatch:Dispatcher.dispatchToFleetCore":    {push: pushOrderDispatched},

	// The release of a carrier from an Edge that sends no stream fence.
	"service:BinManifestService.syncOrClearForReleased": {push: pushBinsChanged},

	// The lift: the pickup ends a streamed bin's generation.
	"store/bins:LiftToTransitTx":               {callers: []string{"store:DB.LiftBinToTransitTx"}},
	"store:DB.LiftBinToTransitTx":              {callers: []string{"service:BinService.Lift"}},
	"service:BinService.Lift":                  {callers: []string{"engine:Engine.handlePickupBlockCompleted", "service:BinService.MoveToTransit"}},
	"service:BinService.MoveToTransit":         {unreach: "tests only"},
	"engine:Engine.handlePickupBlockCompleted": {push: pushBinEnteredTransit},
}

const (
	pushBinUpdated        = "EventBinUpdated"
	pushBinsChanged       = "BinsChanged"
	pushOrderDispatched   = "EventOrderDispatched"
	pushBinEnteredTransit = "EventBinEnteredTransit"
)

func TestBumpEpoch_EverySiteReachesABinsPushOrIsNamed(t *testing.T) {
	t.Parallel()
	g := parseCoreCallGraph(t, "..")

	if got := g.bumpSites(t); !equalCounts(got, bumpSites) {
		t.Errorf("generation bumps by function = %v\nwant %v\n\nA new bump must be followed to the push that "+
			"tells the watching Edges (or named here as reaching none) before it lands.", got, bumpSites)
	}
	reached := map[string]bool{}
	var walk func(key string)
	walk = func(key string) {
		if reached[key] {
			return
		}
		reached[key] = true
		node, ok := bumpPushCensus[key]
		if !ok {
			t.Errorf("%s is on a path from a generation bump and is not in bumpPushCensus", key)
			return
		}
		g.check(t, key, node)
		for _, c := range node.callers {
			walk(c)
		}
	}
	for site := range bumpSites {
		walk(site)
	}
	for key := range bumpPushCensus {
		if !reached[key] {
			t.Errorf("%s is in bumpPushCensus but on no path from a bump", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(bumpPushCensus)) {
		if n := bumpPushCensus[key]; n.entry != "" {
			t.Logf("no bins push: %s — %s", key, n.entry)
		}
	}
}

// coreCallGraph is the module's non-test functions, by key ("dir:Recv.Name"
// or "dir:Name"), and what each one's body calls or takes as a value.
type coreCallGraph struct {
	funcs   map[string]*censusFunc
	methods map[string][]string // method name -> every method key of that name
	callers map[string]map[string]bool
}

type censusFunc struct {
	key, dir string
	body     *ast.BlockStmt
	imports  map[string]string // the file's module imports: local name -> dir
	push     string            // the push primitive the body makes, if any
	bumpSQL  bool              // the body carries delta_epoch = delta_epoch + …
	bumps    int               // s.bumpEpoch calls in the body
}

func parseCoreCallGraph(t *testing.T, root string) *coreCallGraph {
	t.Helper()
	g := &coreCallGraph{funcs: map[string]*censusFunc{}, methods: map[string][]string{}, callers: map[string]map[string]bool{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		g.addFile(filepath.ToSlash(rel), f)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v (this census is the only thing holding every bump to a push; repoint it if the module moved)", root, err)
	}
	if len(g.funcs) < 1000 {
		t.Fatalf("parsed %d functions; a census that reads nothing reports nothing", len(g.funcs))
	}
	for _, f := range g.funcs {
		for _, target := range g.targets(f) {
			if target == f.key {
				continue
			}
			if g.callers[target] == nil {
				g.callers[target] = map[string]bool{}
			}
			g.callers[target][f.key] = true
		}
	}
	return g
}

var bumpSQL = regexp.MustCompile(`delta_epoch\s*=\s*delta_epoch\s*\+`)

func (g *coreCallGraph) addFile(dir string, f *ast.File) {
	imports := map[string]string{}
	for _, im := range f.Imports {
		path := strings.Trim(im.Path.Value, "\"") // an import path is always a plain quoted literal
		if !strings.HasPrefix(path, "shingocore/") {
			continue
		}
		d := strings.TrimPrefix(path, "shingocore/")
		name := d[strings.LastIndex(d, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = d
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		key := dir + ":" + fd.Name.Name
		if r := recvName(fd); r != "" {
			key = dir + ":" + r + "." + fd.Name.Name
			g.methods[fd.Name.Name] = append(g.methods[fd.Name.Name], key)
		}
		cf := &censusFunc{key: key, dir: dir, body: fd.Body, imports: imports}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.BasicLit:
				cf.bumpSQL = cf.bumpSQL || (e.Kind == token.STRING && bumpSQL.MatchString(e.Value))
			case *ast.CallExpr:
				if s, ok := e.Fun.(*ast.SelectorExpr); ok {
					cf.notePrimitive(s.Sel.Name, e)
				}
			}
			return true
		})
		g.funcs[key] = cf
	}
}

func (cf *censusFunc) notePrimitive(name string, call *ast.CallExpr) {
	switch {
	case name == "bumpEpoch":
		cf.bumps++
	case name == "notifyBinChanged" || name == "NotifyBinChanged":
		cf.push = pushBinsChanged
	case name == "EmitOrderDispatched":
		cf.push = pushOrderDispatched
	case name == "Emit" && mentionsIdent(call, "EventBinUpdated"):
		cf.push = pushBinUpdated
	case name == "Emit" && mentionsIdent(call, "EventBinEnteredTransit"):
		cf.push = pushBinEnteredTransit
	}
}

func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if ix, ok := t.(*ast.IndexExpr); ok {
		t = ix.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// targets is every function f's body calls or takes as a value, resolved by
// name (see the file header).
func (g *coreCallGraph) targets(f *censusFunc) []string {
	var out []string
	ast.Inspect(f.body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok {
				if dir, ok := f.imports[id.Name]; ok {
					if _, ok := g.funcs[dir+":"+e.Sel.Name]; ok {
						out = append(out, dir+":"+e.Sel.Name)
					}
					return false
				}
			}
			out = append(out, g.methods[e.Sel.Name]...)
		case *ast.Ident:
			if _, ok := g.funcs[f.dir+":"+e.Name]; ok {
				out = append(out, f.dir+":"+e.Name)
			}
		}
		return true
	})
	return out
}

func (g *coreCallGraph) bumpSites(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for key, f := range g.funcs {
		switch {
		case f.bumps > 0:
			out[key] = f.bumps
		case f.bumpSQL && key != "service:bumpEpochRaw":
			out[key]++
		}
	}
	return out
}

// check holds one node to what the census says of it.
func (g *coreCallGraph) check(t *testing.T, key string, n censusNode) {
	t.Helper()
	f, ok := g.funcs[key]
	if !ok {
		t.Errorf("%s: no such function in the module", key)
		return
	}
	got := slices.Sorted(maps.Keys(g.callers[key]))
	switch {
	case n.push != "":
		holder := f
		if n.via != "" {
			holder = g.funcs[n.via]
			if holder == nil || !slices.Contains(g.targets(f), n.via) {
				t.Errorf("%s: no longer calls %s, the helper named as its push", key, n.via)
				return
			}
		}
		if holder.push != n.push {
			t.Errorf("%s: pushes %q, census says %q — a door that stopped telling the watching Edges", key, holder.push, n.push)
		}
	case n.entry != "":
		if p := g.pushOf(f); p != "" {
			t.Errorf("%s now pushes (%s): move it from the no-push entries to a door", key, p)
		}
	case n.unreach != "":
		if len(got) != 0 {
			t.Errorf("%s has production callers %v, census says none (%s)", key, got, n.unreach)
		}
	default:
		want := append([]string{}, n.callers...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: production callers %v, census names %v. Follow the new one to its push.", key, got, want)
		}
	}
}

// pushOf is the push a body makes itself or through one function it calls.
func (g *coreCallGraph) pushOf(f *censusFunc) string {
	if f.push != "" {
		return f.push
	}
	for _, target := range g.targets(f) {
		if p := g.funcs[target].push; p != "" && target != f.key {
			return p
		}
	}
	return ""
}

func mentionsIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		if id, ok := m.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
