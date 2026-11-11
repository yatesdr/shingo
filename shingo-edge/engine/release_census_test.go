package engine

// release_census_test.go — who sends, who calls, who creates, who waits.
//
// The release layer's failures have one mechanism in common: a door nobody
// listed. The curtain landed on the doors its author had in mind and missed
// the automatic re-fires; the line-pull guard landed on one door and the sim
// walked in through a third. A census cannot say whether a door is right, but
// it can make a NEW door impossible to add without somebody deciding what it
// is. Each table below is the complete set at the base; a call site that is
// not in its table fails, and so does a table row with no call site left.
//
// It reads the package's own source with go/ast — call sites by the name of
// the function called, attributed to the function they sit in — so a renamed
// helper or a moved call is caught the same way a new one is.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// callSites returns, for every non-test file in dir, the functions that call
// any function whose name matches want, as "file:func" -> number of calls.
func callSites(t *testing.T, dir string, want *regexp.Regexp) map[string]int {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var called string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					called = fun.Name
				case *ast.SelectorExpr:
					called = fun.Sel.Name
				}
				if want.MatchString(called) {
					out[name+":"+fn.Name.Name]++
				}
				return true
			})
		}
	}
	return out
}

// checkCensus compares the call sites found against the table, both ways.
func checkCensus(t *testing.T, what string, found map[string]int, table map[string]string) {
	t.Helper()
	var missing, stale []string
	for site := range found {
		if _, ok := table[site]; !ok {
			missing = append(missing, site)
		}
	}
	for site := range table {
		if _, ok := found[site]; !ok {
			stale = append(stale, site)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, s := range missing {
		t.Errorf("%s: %s is not in the census — decide what it is and add it with its reason", what, s)
	}
	for _, s := range stale {
		t.Errorf("%s: %s is in the census but no longer calls — remove the row", what, s)
	}
}

// releaseEnvelopeSenders are the functions that put an OrderRelease envelope on
// the outbox (orders.Manager.ReleaseOrder / ReleaseOrderWithDisposition). Every
// release reaches Core through one of these, so every gate has to stand in
// front of all of them.
var releaseEnvelopeSenders = map[string]string{
	"release_commit.go:applyLeg":                     "the trunk's plain releases: no process node, no claim, produce role",
	"release_commit.go:releaseOrderDropFastPath":     "a drop-situation evac: disposition straight through",
	"release_commit.go:releaseOrderWithFullLineside": "the lineside release, after capture, finalize, task state and flush",
}

// releaseTrunkCallers are the callers of the act every release runs through —
// runAct, and its door-facing name ReleaseOrderWithLineside — and whose act
// each one carries.
var releaseTrunkCallers = map[string]string{
	"release_doors.go:ReleaseOrderWithLineside":    "operator: the per-order click (door 2)",
	"release_doors.go:ReleaseStagedOrders":         "operator: the pair click's two legs, one act (door 1)",
	"release_doors.go:releaseChangeoverWaitScoped": "operator: the changeover sweep and per-node click, one act (doors 5 and 6)",
	"release_doors.go:fireIntents":                 "the intent worker: a node's held intents, re-planned on a wake",
	"sim_operator.go:release":                      "sim: the auto-operator's per-order arm, and the changeover's purposed release where the tile shows one (-tags sim)",
}

// releasePlanCallers are the callers of the gate table's plans. Every one is a
// door in release_doors.go: a decision taken anywhere else is a door with its
// own gates, which is the failure the layer exists to make impossible.
var releasePlanCallers = map[string]string{
	"release_doors.go:runAct":                      "every act's legs (PlanAct: scope, the trunk, G1, G3, G6, G7)",
	"release_doors.go:ReleaseStagedOrders":         "door 1's own gates (PlanPair)",
	"release_doors.go:releaseChangeoverWaitScoped": "doors 5 and 6's tasks (PlanChangeover)",
	"release_doors.go:releaseNodeWithClaim":        "doors 3 and 4 (PlanMaterial)",
}

// orderCreationSites are the functions that create an order at the Edge. A
// creation that carries a bin across a curtained node without a station wait is
// a release nobody can gate (the no-wait population); this is where the next
// one shows up.
var orderCreationSites = map[string]string{
	"api_retrieve.go:createRetrieveDirect":                        "API retrieve, direct arm",
	"changeover_applier.go:createComplexFromSpec":                 "changeover plan: complex legs (the planner's waits; S7 adds one before the per-position swap's pickup)",
	"changeover_applier.go:createRetrieveFromSpec":                "changeover plan: simple retrieves (no steps)",
	"loader_outbound_guard.go:fileLoadedOutbound":                 "loader outbound move, filed at the LOAD (no wait)",
	"operator_bin_ops.go:createUnloaderEmptyOut":                  "unloader empty-out (no wait)",
	"operator_node_changeover.go:DeliverNewMaterialForChangeover": "changeover node: deliver new material (no wait: a drop finishes, S7)",
	"operator_node_changeover.go:EvacuateNode":                    "changeover node: evacuate (a wait in front of a curtained pickup, S7) — door 4 on its fallback arm",
	"operator_node_changeover.go:StageNodeChangeoverMaterial":     "changeover node: stage new material",
	"operator_produce.go:applyProduceEmptyLine":                   "produce REQUEST and REQUEST EMPTY BIN: an empty to a line Core reports bare (no wait: a drop finishes)",
	"operator_produce.go:createClearsThenPrimes":                  "every request button: a press's chain ahead of its swap, a named-bin move of a wrong carrier to the outbound (no wait: it lifts at a dependent position, never while a leg places there)",
	"operator_produce.go:carrierToPosition":                       "a carrier to a position other than the line, a prime to a bare dependent position or a keep-staged refill: a plain retrieve from the inbound source (no wait: a drop finishes, at a position no operator stands at)",
	"operator_produce.go:dispatchPairedLeg":                       "produce REQUEST: a paired leg",
	"operator_send_back.go:SendBack":                              "SEND BACK: the outside cart by name to the outbound, and tops & bottoms to the outside (no wait: the operator's tap is the release, and a drop finishes)",
	"keep_staged_spot.go:returnSpare":                             "keep-staged spot: a wrong or unwanted spare back to its inbound source (no wait)",
	"changeover_cancel_park.go:clearStrandedPark":                 "single-robot REQUEST: a bin left on outbound staging after a cancel, on to the outbound destination (no wait)",
	"changeover_cancel_park.go:finishParkedTrips":                 "changeover cancel: the line's bin an aborted leg parked on staging, on to that leg's destination, and the incoming bin its stage left, back to its source (no wait)",
	"operator_quality_hold.go:RecallContainedPayload":             "quality: recall a contained payload",
	"operator_quality_hold.go:ReleaseFromContainment":             "quality: release from containment (a creation, not a release door)",
	"operator_quality_hold.go:SendBinToQualityHold":               "quality: send a bin to hold (a wait in front of a curtained pickup, S7)",
	"operator_stations.go:applyConsumePlan":                       "consume REQUEST: the swap legs (the builders' waits)",
	"release_commit.go:commitMaterial":                            "Material page RELEASE (door 3) and the position evac (door 4): a move created at the release",
	"operator_window_pullback.go:PullFromMarket":                  "loader window: pull from market",
	"wiring_status_changed.go:handleSequentialBackfill":           "sequential backfill B, minted on Order A's in_transit (no wait: a drop finishes, S7)",
}

// stationWaitSites are the step builders that place a station-owned wait.
var stationWaitSites = map[string]string{
	"changeover_applier.go:createComplexFromSpec":         "S7: the per-position swap's pickup at a curtained position (system-created)",
	"operator_node_changeover.go:EvacuateNode":            "S7: the evac's pickup on a curtained node (the operator's press)",
	"operator_quality_hold.go:SendBinToQualityHold":       "S7: the hold's pickup on a curtained node (the operator's press)",
	"changeover_tooling.go:holdComplexInbound":            "tooling: the staging hold before a leg sets its bin on a press position",
	"changeover_tooling.go:holdInbound":                   "tooling: the staging hold on an Add's retrieve",
	"changeover_tooling.go:setCarryoverRoundTrip":         "tooling: the carried-over bin's return hold",
	"material_orders.go:BuildSequentialRemovalSteps":      "sequential Order A: hold at the position",
	"material_orders.go:BuildStagedReleaseSteps":          "drop: hold at the node for the count",
	"material_orders.go:BuildTwoRobotPressIndexSwapSteps": "press-index R1 and R2: hold at the front / paired position",
	"material_orders.go:BuildTwoRobotSwapSteps":           "two_robot: the supply holds at staging (on a keep-staged spot, before the lift)",
	"material_orders.go:twoRobotEvac":                     "two_robot: the evac holds at the node",
	"material_orders.go:singleRobotTail":                  "single_robot swap: hold at the node",
	"material_orders.go:buildPressIndexChangeoverSwap":    "press-index changeover: ready on R1 and R2",
	"material_orders.go:buildPressIndexPerPositionSwap":   "press-index per-position changeover: ready at the position, before its lift",
	"material_orders.go:buildSequentialPerPositionSwap":   "sequential changeover: hold at the position",
	"material_orders.go:singleRobotChangeoverTail":        "single_robot changeover: ready at the node",
	"material_orders.go:buildToolingEvacSteps":            "the tool change's leg: the Tooling done hold at staging",
	"material_orders.go:buildTwoRobotChangeoverSwap":      "two_robot changeover: the shared ready, supply at staging",
	"material_orders.go:twoRobotChangeoverLegs":           "two_robot changeover: the shared ready, evac at the node",
}

func TestReleaseEnvelopeSendersAreCensused(t *testing.T) {
	t.Parallel()
	found := callSites(t, ".", regexp.MustCompile(`^ReleaseOrder(WithDisposition)?$`))
	checkCensus(t, "envelope sender", found, releaseEnvelopeSenders)
}

func TestReleaseTrunkCallersAreCensused(t *testing.T) {
	t.Parallel()
	found := callSites(t, ".", regexp.MustCompile(`^(ReleaseOrderWithLineside|runAct)$`))
	checkCensus(t, "trunk caller", found, releaseTrunkCallers)
}

func TestReleasePlanCallersAreCensused(t *testing.T) {
	t.Parallel()
	found := callSites(t, ".", regexp.MustCompile(`^Plan(Act|Leg|Pair|Changeover|Material|Flip)$`))
	checkCensus(t, "plan caller", found, releasePlanCallers)
}

// TestReleaseDoorsDecideNothing holds release_doors.go to being adapters: no
// refusal is constructed there (the plans construct every one), and nothing
// is read or written there directly (the loader reads, the commit writes).
func TestReleaseDoorsDecideNothing(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("release_doors.go")
	if err != nil {
		t.Fatalf("read release_doors.go: %v", err)
	}
	for _, banned := range []string{"fmt.Errorf(", "errors.New(", "Error{", "e.db.", "e.orderMgr.", "e.coreClient.", "e.plcMgr."} {
		if strings.Contains(string(src), banned) {
			t.Errorf("release_doors.go contains %q: a door builds its act, loads, plans and commits — "+
				"a refusal belongs in a plan (package release), a read in release_load.go, an effect in release_commit.go", banned)
		}
	}
}

func TestOrderCreationSitesAreCensused(t *testing.T) {
	t.Parallel()
	found := callSites(t, ".", regexp.MustCompile(`^Create\w*Order\w*$`))
	checkCensus(t, "order creation", found, orderCreationSites)
}

func TestStationWaitSitesAreCensused(t *testing.T) {
	t.Parallel()
	found := callSites(t, ".", regexp.MustCompile(`^stationWait$`))
	checkCensus(t, "station wait", found, stationWaitSites)
}

// TestStationWaitsNameAPurpose: every station wait a builder places names its
// purpose with a release.Purpose constant (the closed set), never a computed
// value or a literal, so the button that releases it and the act that covers
// it are decided where the wait is written.
func TestStationWaitsNameAPurpose(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	consts := map[string]bool{"PurposeSwap": true, "PurposeReady": true, "PurposeToolingDone": true}
	calls := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "stationWait" {
				return true
			}
			calls++
			sel, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr)
			pkg, pok := func() (*ast.Ident, bool) {
				if !ok {
					return nil, false
				}
				id, ok := sel.X.(*ast.Ident)
				return id, ok
			}()
			if !ok || !pok || pkg.Name != "release" || !consts[sel.Sel.Name] {
				t.Errorf("%s: stationWait must name a release.Purpose constant (release.PurposeSwap, PurposeReady, PurposeToolingDone)",
					fset.Position(call.Pos()))
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatal("no stationWait calls found")
	}
}
