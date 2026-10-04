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
	"sim_operator.go:runRelease":                   "sim: the auto-operator's per-order arm (-tags sim)",
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
	"api_retrieve.go:CreateRetrieveForAPI":                        "API retrieve (external caller)",
	"api_retrieve.go:createRetrieveDirect":                        "API retrieve, direct arm",
	"changeover_applier.go:createComplexFromSpec":                 "changeover plan: complex legs (the planner's waits; S7 adds one before the per-position swap's pickup)",
	"changeover_applier.go:createRetrieveFromSpec":                "changeover plan: simple retrieves (no steps)",
	"loader_outbound_guard.go:createLoaderOutbound":               "loader outbound move (no wait)",
	"operator_bin_ops.go:RequestFullBin":                          "operator: full bin to a loader window",
	"operator_bin_ops.go:createUnloaderEmptyOut":                  "unloader empty-out (no wait)",
	"operator_bin_ops.go:requestEmptyAtManualSwapLoader":          "operator: empty to a manual-swap loader",
	"operator_bin_ops.go:requestEmptyForSwapModes":                "operator: empty for a swap-mode node",
	"operator_demand_loader.go:stageOperatorEmpty":                "loader demand: stage an empty",
	"operator_demand_unloader.go:fireUnloaderFulls":               "unloader demand: fulls",
	"operator_home_consolidation.go:ClearLoaderHome":              "loader home consolidation, order A",
	"operator_home_consolidation.go:dispatchBufferConsolidation":  "loader home consolidation, order B",
	"operator_node_changeover.go:DeliverNewMaterialForChangeover": "changeover node: deliver new material (no wait: a drop finishes, S7)",
	"operator_node_changeover.go:EvacuateNode":                    "changeover node: evacuate (a wait in front of a curtained pickup, S7) — door 4 on its fallback arm",
	"operator_node_changeover.go:StageNodeChangeoverMaterial":     "changeover node: stage new material",
	"operator_produce.go:applyProducePlan":                        "produce REQUEST: the swap legs (the builders' waits)",
	"operator_produce.go:applyProduceEmptyLine":                   "produce REQUEST and REQUEST EMPTY BIN: an empty to a single-robot line Core reports bare (no wait: a drop finishes)",
	"operator_produce.go:dispatchPairedLeg":                       "produce REQUEST: a paired leg",
	"keep_staged_spot.go:refillSpot":                              "keep-staged spot: a plain retrieve from the inbound source (no wait: it lands on staging)",
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
	"material_orders.go:BuildTwoRobotSwapSteps":           "two_robot: the supply holds at staging",
	"material_orders.go:BuildTwoRobotSwapFromSpare":       "keep-staged two_robot: the supply holds at the spot, before the lift",
	"material_orders.go:twoRobotEvac":                     "two_robot: the evac holds at the node",
	"material_orders.go:singleRobotTail":                  "single_robot swap (full or from the spare): hold at the node",
	"material_orders.go:buildPressIndexChangeoverSwap":    "press-index changeover: ready, and tooling done on R1",
	"material_orders.go:buildSequentialPerPositionSwap":   "sequential changeover: hold at the position",
	"material_orders.go:singleRobotChangeoverTail":        "single_robot changeover (full or from the spare): ready, and tooling done",
	"material_orders.go:buildToolingEvacSteps":            "tooling evac: the tooling-done gate",
	"material_orders.go:buildTwoRobotChangeoverSwap":      "two_robot changeover: the shared ready, supply at staging",
	"material_orders.go:buildTwoRobotChangeoverFromSpare": "keep-staged two_robot changeover: the shared ready at the spot",
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

// ── Cited tests exist ────────────────────────────────────────────────────

var citedTest = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

// phantomCitations are comments that cite a test which does not exist, known at
// the base. A comment that names a test is read as "this is pinned"; when the
// test is missing it is a false comment, and one of them sent a review looking
// for coverage that was not there (the curtain review's error 11).
var phantomCitations = map[string]string{
	"TestApplyCoreStatus_Queued_UnchangedBehavior @ orders/manager_db_test.go":                                     "outside the release layer: reported, not fixed here",
	"TestCaptureReactivatesInactive @ store/lineside/lineside_test.go":                                             "outside the release layer: reported, not fixed here",
	"TestCellPicture_BinWordComesFromTheBinType @ domain/cell_picture_test.go":                                     "outside the release layer: reported, not fixed here",
	"TestChangeoverFlow_ChangeoverRole @ engine/changeover_flow_test.go":                                           "outside the release layer: reported, not fixed here",
	"TestChangeover_StateTransitionsAndListing @ service/changeover_service_test.go":                               "outside the release layer: reported, not fixed here",
	"TestCompleteCutover_LeavesEveryPileActiveAndDraining @ engine/lineside_bucket_pins_test.go":                   "outside the release layer: reported, not fixed here",
	"TestComputeSwapReady_SupplyMustBeReleasable @ store/station_views_test.go":                                    "outside the release layer: reported, not fixed here",
	"TestConfirmUnloaderU1OnClear @ engine/operator_bin_ops.go":                                                    "outside the release layer: reported, not fixed here",
	"TestCoreAPI @ www/handlers_api_config_test.go":                                                                "outside the release layer: reported, not fixed here",
	"TestDrainAcrossStyleCutover @ store/lineside/lineside_test.go":                                                "outside the release layer: reported, not fixed here",
	"TestFindLoaderForDemand_RoutesToSignaledCoreNode @ engine/payloads_for_loader_test.go":                        "outside the release layer: reported, not fixed here",
	"TestFlipGuard_ConsumeWantsTheINCOMINGStylesMaterial @ engine/changeover_test.go":                              "outside the release layer: reported, not fixed here",
	"TestFlowCarryThrough_UnlockedSaveLeavesEveryUnspokenColumnAlone @ domain/flow.go,engine/flow_compose_test.go": "outside the release layer: reported, not fixed here",
	"TestFlowExpand_LockedCellIsThePriorRestamped @ domain/flow_test.go":                                           "outside the release layer: reported, not fixed here",
	"TestFlowShape_StripPayloadLeavesEverythingElse @ domain/flow_preset_shape_test.go":                            "outside the release layer: reported, not fixed here",
	"TestFlowspecHasNoFieldTheComposerCannotWrite @ domain/flow_flowspec_totality_test.go":                         "outside the release layer: reported, not fixed here",
	"TestHandleOrderRelease_RemainingUOPZero @ engine/operator_release_uop_test.go":                                "outside the release layer: reported, not fixed here",
	"TestHandleUnloaderFullInCompletion_FiresU2 @ engine/operator_release_test.go":                                 "outside the release layer: reported, not fixed here",
	"TestKafka @ www/handlers_api_config_test.go":                                                                  "outside the release layer: reported, not fixed here",
	"TestLinesideStarved @ service/station_starvation_test.go":                                                     "outside the release layer: reported, not fixed here",
	"TestMaybeCreateLoaderEmptyIn_CreatesL1WhenDemandSignalFires @ engine/operator_release_test.go":                "outside the release layer: reported, not fixed here",
	"TestOperatorStations_SetStationNodes @ service/station_service_test.go,store/store_test.go":                   "outside the release layer: reported, not fixed here",
	"TestPinAutoPushGate_ClearIsTheOnlyTrigger @ engine/wiring_completion.go":                                      "outside the release layer: reported, not fixed here",
	"TestPinAutoPushGate_PushEmptyFillsASibling @ engine/wiring_completion.go":                                     "outside the release layer: reported, not fixed here",
	"TestPin_EmptySlotBindOfADepartedCarrierTakesItsOldStamp @ engine/epoch_monotonic_test.go":                     "outside the release layer: reported, not fixed here",
	"TestPin_JumpChargedAtConfirmTime @ engine/plc_truth_test.go":                                                  "outside the release layer: reported, not fixed here",
	"TestPin_JumpIsHeldAtThePoll @ plc/plc_truth_test.go":                                                          "outside the release layer: reported, not fixed here",
	"TestPin_OlderAdjustmentArrivingLateOverwritesTheNewer @ engine/count_fence_pins_test.go":                      "outside the release layer: reported, not fixed here",
	"TestPin_P0a_CountRowDeadLettersAfterTenFailures @ messaging/outbox_count_subject_test.go":                     "outside the release layer: reported, not fixed here",
	"TestPin_P0a_PurgeDeletesAnExhaustedCountRow @ store/outbox_count_subject_test.go":                             "outside the release layer: reported, not fixed here",
	"TestPin_P0f_EdgeTakesCoresNumberOverItsInFlightTicks @ engine/count_fence_pins_test.go":                       "outside the release layer: reported, not fixed here",
	"TestPin_ResetDeltaMovesNoCount @ engine/plc_truth_test.go":                                                    "outside the release layer: reported, not fixed here",
	"TestPin_ResetIsEmittedButNotShipped @ plc/plc_truth_test.go":                                                  "outside the release layer: reported, not fixed here",
	"TestPlanNodeAction_PressPosition @ engine/changeover_planner_test.go":                                         "outside the release layer: reported, not fixed here",
	"TestProcessChangeovers_CreateAtomic @ service/changeover_service_test.go,store/store_test.go":                 "outside the release layer: reported, not fixed here",
	"TestProcessesJSClaimEditorCharacterization @ www/composer_model_characterization_test.go":                     "outside the release layer: reported, not fixed here",
	"TestRegression_CaptureDeltaRejectedOnTrueMismatch @ engine/wiring_release_test.go":                            "outside the release layer: reported, not fixed here",
	"TestRegression_NegativeRuntimeFromOverpack @ engine/wiring_counter_delta_test.go":                             "outside the release layer: reported, not fixed here",
	"TestRegression_ReconciliationSelfHeal @ engine/wiring_concurrent_tick_test.go":                                "outside the release layer: reported, not fixed here",
	"TestRegression_RemovalOrderClearsBinAndZeroesUOP @ engine/uop_regression_test.go":                             "outside the release layer: reported, not fixed here",
	"TestRegression_RuntimeUOPNegativeNoReorderRefire @ engine/wiring_counter_delta_test.go":                       "outside the release layer: reported, not fixed here",
	"TestShapeFieldWordsAreTheClaimsOwnNames @ domain/flow_preset_word_drift_test.go":                              "outside the release layer: reported, not fixed here",
	"TestShapeFieldWordsMatchTheModel @ domain/flow_preset_word_drift_test.go":                                     "outside the release layer: reported, not fixed here",
	"TestStationView_CarriesNoCellPicture @ service/composer_budget_pins_test.go":                                  "outside the release layer: reported, not fixed here",
	"TestStyleNodeClaims_ManualSwapRequiresOutboundDestination @ store/store_test.go":                              "outside the release layer: reported, not fixed here",
	"TestUniqueActivePerNodeStylePart @ store/lineside/lineside_test.go":                                           "outside the release layer: reported, not fixed here",
	"TestValidateNodeClaim_ManualSwapNeedsNoPayload @ domain/claim_validation_test.go":                             "outside the release layer: reported, not fixed here",
	"TestWiring_CounterDelta_ConsumeFloorsAtZero @ engine/wiring_test.go":                                          "outside the release layer: reported, not fixed here",
	"TestWiring_IngestCompletion_ResetsProduceUOP @ engine/wiring_test.go":                                         "outside the release layer: reported, not fixed here",
	"TestWiring_RetrieveCompletion_ConsumePartialBin_ResetsToBinUOP @ engine/wiring_test.go":                       "outside the release layer: reported, not fixed here",
	"TestWithLoaderBudget_EmitDuringReservation @ engine/operator_demand_loader.go":                                "outside the release layer: reported, not fixed here",
	"TestWithLoaderBudget_FiresWhenOccupancyReadFails @ engine/loader_reservation_seam_test.go":                    "outside the release layer: reported, not fixed here",
}

// TestCitedTestsExist: every Test… a comment in shingo-edge names must be a test
// function somewhere in the repository (Edge comments cite Core's tests too).
func TestCitedTestsExist(t *testing.T) {
	t.Parallel()
	root := ".."
	defined := testsDefinedUnder(t, filepath.Join("..", ".."))
	cited := map[string][]string{} // test name -> where cited
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, cg := range f.Comments {
			for _, m := range citedTest.FindAllString(cg.Text(), -1) {
				cited[m] = append(cited[m], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	var phantoms []string
	for name, where := range cited {
		if defined[name] || isTestPattern(name) {
			continue
		}
		key := name + " @ " + strings.Join(dedupe(where), ",")
		if _, known := phantomCitations[key]; known {
			continue
		}
		phantoms = append(phantoms, key)
	}
	sort.Strings(phantoms)
	for _, p := range phantoms {
		t.Errorf("a comment cites a test that does not exist: %s", p)
	}
	for key := range phantomCitations {
		name, _, _ := strings.Cut(key, " @ ")
		if defined[name] {
			t.Errorf("%s now exists — drop it from phantomCitations", key)
		}
	}
}

// testsDefinedUnder collects every top-level Test function in the _test.go
// files under root.
func testsDefinedUnder(t *testing.T, root string) map[string]bool {
	t.Helper()
	defined := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				defined[fn.Name.Name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return defined
}

// isTestPattern keeps a -run prefix pattern or TestMain from counting as a
// citation of one test.
func isTestPattern(name string) bool {
	return strings.HasSuffix(name, "_") || name == "TestMain"
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
