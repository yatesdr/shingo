package orders

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// creation_funnel_census_test.go — the Edge's order-creation funnel, counted
// from the tree: every creation site → the three constructors → the two wire
// shapes Core receives.
//
// At the base (ba7afb21): 25 creation sites → 9 exported Create*Order methods →
// 3 constructors (createRetrieveOrder, createMoveOrder, createComplexOrder) → 2
// wire shapes (OrderRequest, ComplexOrderRequest).
//
// Core's door census (shingo-core/dispatch/order_writer_census_test.go) counts
// the Edge as ONE door, "Edge wire intake", plus complex intake. That is true on
// Core's side of the wire and says nothing about how many ways the Edge has of
// reaching it. The unification plan moves destination choice into one planner
// and later decides where the choreography lives on a measured number; both
// need the Edge's side counted, not estimated (two reviews counted 12 and 22).
//
// Pinned three ways, each by parsing:
//   - the wire shapes: the only composite literals of protocol.OrderRequest and
//     protocol.ComplexOrderRequest, and the constructor each sits in;
//   - the funnel: every Create*Order method on Manager, and which constructor
//     it ends in;
//   - the creation sites: every call to a Create*Order method outside this
//     package, by file and enclosing function.

// scanEdgeSources returns every non-test .go file in the Edge module, keyed by
// module-root-relative path with forward slashes. The package dir is the test's
// CWD, so ".." is the module root.
func scanEdgeSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == "testdata" || (strings.HasPrefix(n, ".") && n != "..") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("..", path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("scan edge sources: %v", err)
	}
	return out
}

// edgeSites walks every function in sources and reports "file:Func" for each
// node match accepts.
func edgeSites(t *testing.T, sources map[string]string, match func(ast.Node) string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	for path, src := range sources {
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(x ast.Node) bool {
				if what := match(x); what != "" {
					key := path + ":" + fn.Name.Name
					if !slices.Contains(out[key], what) {
						out[key] = append(out[key], what)
						slices.Sort(out[key])
					}
				}
				return true
			})
		}
	}
	return out
}

func sitesEqual(t *testing.T, what string, got map[string][]string, want map[string]string) {
	t.Helper()
	gotFlat := map[string]string{}
	for k, v := range got {
		gotFlat[k] = strings.Join(v, ",")
	}
	var diffs []string
	for k, v := range gotFlat {
		if w, ok := want[k]; !ok {
			diffs = append(diffs, "in the tree, not named: "+k+" → "+v)
		} else if w != v {
			diffs = append(diffs, "changed: "+k+" → "+v+" (pinned "+w+")")
		}
	}
	for k, w := range want {
		if _, ok := gotFlat[k]; !ok {
			diffs = append(diffs, "named, not in the tree: "+k+" → "+w)
		}
	}
	slices.Sort(diffs)
	if len(diffs) > 0 {
		t.Errorf("%s moved:\n  %s", what, strings.Join(diffs, "\n  "))
	}
}

// TestCensus_EdgeWireShapes pins the two shapes an Edge order reaches Core in,
// and the three constructors that build them.
func TestCensus_EdgeWireShapes(t *testing.T) {
	t.Parallel()
	got := edgeSites(t, scanEdgeSources(t), func(x ast.Node) string {
		lit, ok := x.(*ast.CompositeLit)
		if !ok {
			return ""
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "protocol" {
			return ""
		}
		if sel.Sel.Name == "OrderRequest" || sel.Sel.Name == "ComplexOrderRequest" {
			return sel.Sel.Name
		}
		return ""
	})
	sitesEqual(t, "the Edge's wire shapes", got, map[string]string{
		"orders/manager_create.go:createRetrieveOrder": "OrderRequest",
		"orders/manager_create.go:createMoveOrder":     "OrderRequest",
		"orders/manager_create.go:createComplexOrder":  "ComplexOrderRequest",
	})
}

// constructors are the three Manager methods that build a wire shape. Every
// exported Create*Order on Manager is a wrapper that ends in one of them.
var constructors = []string{"createRetrieveOrder", "createMoveOrder", "createComplexOrder"}

func isCreateOrderName(n string) bool {
	return (strings.HasPrefix(n, "Create") || strings.HasPrefix(n, "create")) && strings.Contains(n, "Order")
}

// TestCensus_EdgeConstructorFunnel pins every Create*Order method on Manager
// and the constructor it ends in.
func TestCensus_EdgeConstructorFunnel(t *testing.T) {
	t.Parallel()
	src := map[string]string{}
	for p, s := range scanEdgeSources(t) {
		if strings.HasPrefix(p, "orders/") {
			src[p] = s
		}
	}
	// Direct calls from each Create*Order method to another Create*Order method.
	calls := edgeSites(t, src, func(x ast.Node) string {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isCreateOrderName(sel.Sel.Name) {
			return ""
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "m" {
			return ""
		}
		return sel.Sel.Name
	})
	// Resolve each method to the constructor it ends in.
	next := map[string]string{}
	for k, v := range calls {
		fn := k[strings.LastIndex(k, ":")+1:]
		if isCreateOrderName(fn) && len(v) == 1 {
			next[fn] = v[0]
		}
	}
	got := map[string][]string{}
	for k := range calls {
		fn := k[strings.LastIndex(k, ":")+1:]
		if !isCreateOrderName(fn) {
			continue
		}
		end, hops := fn, 0
		for !slices.Contains(constructors, end) && hops < 10 {
			end, hops = next[end], hops+1
		}
		got[fn] = []string{end}
	}
	for _, c := range constructors {
		got[c] = []string{c}
	}
	sitesEqual(t, "the Edge's constructor funnel", got, map[string]string{
		"CreateRetrieveOrder":                  "createRetrieveOrder",
		"CreateMoveOrder":                      "createMoveOrder",
		"CreateMoveOrderWithPayloadCode":       "createMoveOrder",
		"CreateMoveOrderWithUOP":               "createMoveOrder",
		"CreateMoveOrderCarrying":              "createMoveOrder",
		"CreateMoveOrderCarryingTo":            "createMoveOrder",
		"CreateMoveOrderForBin":                "createMoveOrder",
		"CreateHoldMoveOrder":                  "createMoveOrder",
		"CreateMoveOrderWithPayloadCodeForBin": "createMoveOrder",
		"CreateComplexOrder":                   "createComplexOrder",
		"CreateComplexOrderWithAutoConfirm":    "createComplexOrder",
		"CreateComplexOrderWithPayload":        "createComplexOrder",
		"CreateComplexOrderSibling":            "createComplexOrder",
		"CreateComplexOrderPaired":             "createComplexOrder",
		"createRetrieveOrder":                  "createRetrieveOrder",
		"createMoveOrder":                      "createMoveOrder",
		"createComplexOrder":                   "createComplexOrder",
	})
}

// TestCensus_EdgeCreationSites pins every call site outside this package that
// creates an order, by file and enclosing function, with the Manager methods it
// calls. This is the Edge's list of doors, for the planner phase to inherit.
func TestCensus_EdgeCreationSites(t *testing.T) {
	t.Parallel()
	src := map[string]string{}
	for p, s := range scanEdgeSources(t) {
		if !strings.HasPrefix(p, "orders/") {
			src[p] = s
		}
	}
	got := edgeSites(t, src, func(x ast.Node) string {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isCreateOrderName(sel.Sel.Name) {
			return ""
		}
		if strings.HasPrefix(sel.Sel.Name, "Create") {
			return sel.Sel.Name
		}
		return ""
	})
	sitesEqual(t, "the Edge's order-creation sites", got, edgeCreationSites)
}

// edgeCreationSites is every Edge site that creates an order, and the Manager
// method it calls. 25 at the base of the unification (ba7afb21), 28 on main at
// 6a590181 (quality containment added three), 32 on main at 264147c1 (the
// keep-staged spot, a cancelled changeover's parked trips, and the bare-line
// prime). Three fewer since a person's full at a consume window, the
// unloader's U1 and the manual retrieve at a loader window are asked of Core
// (the loader retrieve ask) and made there. A new site is a new way an Edge
// order reaches Core; name it here.
var edgeCreationSites = map[string]string{
	// the HTTP API and the Edge UI
	"engine/api_retrieve.go:createRetrieveDirect":      "CreateRetrieveOrder",
	"www/handlers_api_orders.go:apiCreateComplexOrder": "CreateComplexOrder",
	"www/handlers_api_orders.go:apiCreateMoveOrder":    "CreateMoveOrder",
	// consume, produce and release (the planned doors)
	"engine/operator_stations.go:applyConsumePlan":      "CreateMoveOrder",
	"engine/operator_produce.go:applyProduceEmptyLine":  "CreateMoveOrderCarryingTo,CreateRetrieveOrder",
	"engine/operator_produce.go:dispatchPairedLeg":      "CreateComplexOrderPaired",
	"engine/operator_produce.go:createClearsThenPrimes": "CreateMoveOrder,CreateMoveOrderForBin,CreateRetrieveOrder",
	// the release layer (main, 2026-09-30): a release's material move
	"engine/release_commit.go:commitMaterial": "CreateMoveOrderWithUOP",
	// changeover
	"engine/changeover_applier.go:createComplexFromSpec":                 "CreateComplexOrderPaired",
	"engine/changeover_applier.go:createRetrieveFromSpec":                "CreateRetrieveOrder",
	"engine/operator_node_changeover.go:DeliverNewMaterialForChangeover": "CreateComplexOrder",
	"engine/operator_node_changeover.go:EvacuateNode":                    "CreateComplexOrderWithAutoConfirm",
	"engine/operator_node_changeover.go:StageNodeChangeoverMaterial":     "CreateComplexOrder,CreateRetrieveOrder",
	"engine/wiring_status_changed.go:handleSequentialBackfill":           "CreateComplexOrder",
	// loader and unloader side-cycles
	"engine/loader_outbound_guard.go:fileLoadedOutbound": "CreateMoveOrderWithPayloadCodeForBin",
	"engine/operator_bin_ops.go:createUnloaderEmptyOut":  "CreateMoveOrderForBin",
	"engine/operator_window_pullback.go:PullFromMarket":  "CreateMoveOrderWithPayloadCodeForBin",
	// keep-staged spot and a cancelled changeover's parked trips (main, 2026-10-03/04)
	"engine/keep_staged_spot.go:refillSpot":              "CreateRetrieveOrder",
	"engine/keep_staged_spot.go:returnSpare":             "CreateMoveOrderForBin",
	"engine/changeover_cancel_park.go:clearStrandedPark": "CreateMoveOrderCarrying",
	"engine/changeover_cancel_park.go:finishParkedTrips": "CreateMoveOrderCarrying",
	// quality containment (main, 2026-09-25)
	"engine/operator_quality_hold.go:RecallContainedPayload": "CreateHoldMoveOrder",
	"engine/operator_quality_hold.go:ReleaseFromContainment": "CreateMoveOrderWithPayloadCode",
	"engine/operator_quality_hold.go:SendBinToQualityHold":   "CreateComplexOrderWithPayload,CreateHoldMoveOrder",
}
