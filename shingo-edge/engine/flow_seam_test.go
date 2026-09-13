package engine

import (
	"errors"
	"reflect"
	"testing"

	"shingoedge/store"
	"shingoedge/store/processes"
)

// flow_seam_test.go — the planner seam.
//
// planChangeover reads the two styles' claims and delegates to
// planChangeoverFrom, which takes them. The composer previews a DRAFT — claims
// that exist only in memory — through the second door, and the whole point of
// the seam is that the two doors lead to the same room: every gate, every
// post-processor and the tooling decoration run identically whether the claims
// came from the database or from a request body.

// seamScenario is one seeded changeover the seam must plan identically through
// both doors. The seed functions live in the files that own the shape they
// build; this table only names them.
type seamScenario struct {
	name string
	// seed writes the process, styles and claims and returns (processID,
	// toStyleID). It may set the engine's Core client when the shape needs one
	// (press-index diffs are refused without Core).
	seed func(t *testing.T, db *store.DB, eng *Engine) (int64, int64)
}

func seamScenarios() []seamScenario {
	withCore := func(eng *Engine) { eng.coreClient = NewCoreClient(testCoreURL) }
	return []seamScenario{
		{"simple swap with staging", func(t *testing.T, db *store.DB, _ *Engine) (int64, int64) {
			p, _, _, to, _, _ := seedChangeoverScenario(t, db)
			return p, to
		}},
		{"multi-node swap/unchanged/drop/add", func(t *testing.T, db *store.DB, _ *Engine) (int64, int64) {
			p, _, _, to := seedMultiNodeScenario(t, db)
			return p, to
		}},
		{"drop", func(t *testing.T, db *store.DB, _ *Engine) (int64, int64) {
			p, _, _, to := seedDropScenario(t, db)
			return p, to
		}},
		{"add", func(t *testing.T, db *store.DB, _ *Engine) (int64, int64) {
			p, _, _, to := seedAddNodeScenario(t, db)
			return p, to
		}},
		{"drop with evacuate-on-changeover", func(t *testing.T, db *store.DB, _ *Engine) (int64, int64) {
			p, _, _, to := seedDropScenarioEvacuate(t, db)
			return p, to
		}},
		{"marked press (tooling decoration)", func(t *testing.T, db *store.DB, eng *Engine) (int64, int64) {
			withCore(eng)
			p, _, to := seedMarkedPressScenario(t, db)
			return p, to
		}},
		{"disjoint press (cross-mode fan-out)", func(t *testing.T, db *store.DB, eng *Engine) (int64, int64) {
			withCore(eng)
			p, to := seedDisjointPressScenario(t, db)
			return p, to
		}},
	}
}

// TestPlanChangeoverFrom_AgreesWithPlanChangeover is the seam's one pin:
// planChangeover(p, s, m) and planChangeoverFrom(p, s, fromDB, toDB, m) build
// reflect.DeepEqual plans for every seeded shape in both materialize modes.
//
// materialize=true writes the marked press's paired position on the FIRST
// call, so that arm runs the DB door first: the second door then finds the row
// where the first left it, and both read the same node list — which is exactly
// what a Start after a Start sees.
func TestPlanChangeoverFrom_AgreesWithPlanChangeover(t *testing.T) {
	t.Parallel()
	for _, sc := range seamScenarios() {
		for _, materialize := range []bool{false, true} {
			name := sc.name + "/materialize=false"
			if materialize {
				name = sc.name + "/materialize=true"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				db := testEngineDB(t)
				eng := testEngine(t, db)
				processID, toStyleID := sc.seed(t, db, eng)

				viaDB, err := eng.planChangeover(processID, toStyleID, materialize)
				if err != nil {
					t.Fatalf("planChangeover: %v", err)
				}
				process, err := db.GetProcess(processID)
				if err != nil {
					t.Fatalf("get process: %v", err)
				}
				var fromClaims []processes.NodeClaim
				if process.ActiveStyleID != nil {
					fromClaims, err = db.ListStyleNodeClaims(*process.ActiveStyleID)
					if err != nil {
						t.Fatalf("list from claims: %v", err)
					}
				}
				toClaims, err := db.ListStyleNodeClaims(toStyleID)
				if err != nil {
					t.Fatalf("list to claims: %v", err)
				}
				viaArgs, err := eng.planChangeoverFrom(processID, toStyleID, fromClaims, toClaims, materialize)
				if err != nil {
					t.Fatalf("planChangeoverFrom: %v", err)
				}
				if !reflect.DeepEqual(viaDB, viaArgs) {
					t.Errorf("the two doors planned differently.\n via planChangeover:     %+v\n via planChangeoverFrom: %+v", viaDB, viaArgs)
				}
			})
		}
	}
}

// TestPlanChangeoverFrom_RefusesTheRunningStyleWithTheSameWords: a draft
// preview against the running style must be refused by the seam itself, in
// the words the desktop preview has always used — the composer shows them.
func TestPlanChangeoverFrom_RefusesTheRunningStyleWithTheSameWords(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, _, fromStyleID, _, _, _ := seedChangeoverScenario(t, db)

	viaDB := eng.mustFailPlan(t, func() error {
		_, err := eng.planChangeover(processID, fromStyleID, false)
		return err
	})
	viaArgs := eng.mustFailPlan(t, func() error {
		_, err := eng.planChangeoverFrom(processID, fromStyleID, nil, nil, false)
		return err
	})
	if viaDB.Error() != viaArgs.Error() {
		t.Errorf("refusal words differ: planChangeover %q, planChangeoverFrom %q", viaDB, viaArgs)
	}
	if !errors.Is(viaArgs, ErrStyleAlreadyRunning) {
		t.Errorf("planChangeoverFrom refusal %q is not ErrStyleAlreadyRunning — the composer maps that to 409", viaArgs)
	}
}

func (e *Engine) mustFailPlan(t *testing.T, plan func() error) error {
	t.Helper()
	err := plan()
	if err == nil {
		t.Fatal("planning the running style succeeded; it must be refused")
	}
	return err
}

// TestBuildChangeoverPlanReporting_NamesTheNodesItSkipped: BuildChangeoverPlan
// silently drops a diff whose node has no process_nodes row. The reporting
// sibling returns those names, and the plan it returns is the plan
// BuildChangeoverPlan returns — the existing callers see nothing new.
func TestBuildChangeoverPlanReporting_NamesTheNodesItSkipped(t *testing.T) {
	t.Parallel()
	from := processes.NodeClaim{ID: 1, CoreNodeName: "KNOWN", Role: "consume", SwapMode: "two_robot", PayloadCode: "OLD",
		InboundStaging: "STG", OutboundDestination: "DST", InboundSource: "SRC"}
	to := from
	to.ID, to.PayloadCode = 2, "NEW"
	ghostFrom, ghostTo := from, to
	ghostFrom.CoreNodeName, ghostTo.CoreNodeName = "GHOST", "GHOST"
	diffs := DiffStyleClaims([]processes.NodeClaim{from, ghostFrom}, []processes.NodeClaim{to, ghostTo})
	nodes := []processes.Node{{ID: 7, CoreNodeName: "KNOWN", Name: "Known"}}

	plan, skipped := BuildChangeoverPlanReporting(diffs, nodes, false, nil, toolingChangeover{})
	if !reflect.DeepEqual(skipped, []string{"GHOST"}) {
		t.Errorf("skipped = %v, want [GHOST]", skipped)
	}
	if !reflect.DeepEqual(plan, BuildChangeoverPlan(diffs, nodes, false, nil, toolingChangeover{})) {
		t.Error("the reporting sibling and BuildChangeoverPlan returned different plans")
	}
	if len(plan.Actions) != 1 || plan.Actions[0].CoreNodeName != "KNOWN" {
		t.Errorf("plan actions = %+v, want one for KNOWN", plan.Actions)
	}
	// An unchanged node without a row is not a skip: it would get no action
	// either way, and naming it would send the engineer after a node the
	// changeover never touches.
	same := from
	same.CoreNodeName = "IDLE"
	diffs = DiffStyleClaims([]processes.NodeClaim{same}, []processes.NodeClaim{same})
	if _, skipped := BuildChangeoverPlanReporting(diffs, nil, false, nil, toolingChangeover{}); len(skipped) != 0 {
		t.Errorf("an unchanged node was reported as skipped: %v", skipped)
	}
}
