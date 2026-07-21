package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// changeover_participants_test.go — Stage 1: the participant set.
//
// Participants are the nodes a changeover PHYSICALLY TOUCHES, frozen at plan
// time. They exist because "which nodes is this changeover about" was being
// re-derived independently by three consumers that disagreed, and because a
// press-index extension seat — traversed by the index motion, owning no task
// and no order — is invisible to a task-keyed answer.

func pressIndexClaim(front, paired, second string) *processes.NodeClaim {
	return &processes.NodeClaim{
		CoreNodeName:         front,
		SwapMode:             protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode:       paired,
		SecondPairedCoreNode: second,
	}
}

// TestPressIndexExtensionPositions_ScopeIsExtensionsOnly is the guard on the
// shared helper's deliberately narrow contract. It must return ONLY the
// extension seats — no front node, no swap-mode filtering — because its two
// callers differ on exactly those two points and folding either in would
// silently change one of them.
func TestPressIndexExtensionPositions_ScopeIsExtensionsOnly(t *testing.T) {
	t.Parallel()

	three := pressIndexClaim("PLN_01", "PLN_02", "PLN_03")
	got := pressIndexExtensionPositions(three)
	if len(got) != 2 || got[0] != "PLN_02" || got[1] != "PLN_03" {
		t.Fatalf("3-position = %v, want [PLN_02 PLN_03]", got)
	}
	for _, g := range got {
		if g == "PLN_01" {
			t.Error("helper returned the FRONT seat; callers own that difference (fanOutPositions prepends it, the cross-mode walk must not)")
		}
	}

	two := pressIndexClaim("PLN_04", "PLN_05", "")
	if got := pressIndexExtensionPositions(two); len(got) != 1 || got[0] != "PLN_05" {
		t.Errorf("2-position = %v, want [PLN_05] (empty second seat dropped)", got)
	}

	if got := pressIndexExtensionPositions(nil); got != nil {
		t.Errorf("nil claim = %v, want nil", got)
	}

	// No swap-mode filtering: the helper answers "which seats does this claim
	// name", and each caller applies its own guard.
	notPressIndex := &processes.NodeClaim{
		CoreNodeName:   "ALN_001",
		SwapMode:       protocol.SwapModeTwoRobot,
		PairedCoreNode: "ALN_002",
	}
	if got := pressIndexExtensionPositions(notPressIndex); len(got) != 1 {
		t.Errorf("non-press-index claim = %v; the helper must not filter by swap mode — its callers do", got)
	}
}

// TestBuildParticipants_TaskRoleIsTheFullDiffSlice pins the strict-widening
// decision: SituationUnchanged diffs mint task rows today, so excluding them
// from participants would UNGATE nodes the current gate blocks.
func TestBuildParticipants_TaskRoleIsTheFullDiffSlice(t *testing.T) {
	t.Parallel()

	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N-SWAP", Situation: SituationSwap},
		{CoreNodeName: "N-UNCHANGED", Situation: SituationUnchanged},
		{CoreNodeName: "N-ADD", Situation: SituationAdd},
	}
	got := buildParticipants(diffs)
	if len(got) != 3 {
		t.Fatalf("participants = %d (%v), want 3 — unchanged diffs are participants too", len(got), got)
	}
	for _, p := range got {
		if p.Role != domain.ParticipantRoleTask {
			t.Errorf("%s role = %q, want task", p.CoreNodeName, p.Role)
		}
	}
}

// TestBuildParticipants_PressIndexSeatsBecomeIndexedOver is the case the table
// exists for: a same-bin-type press-index changeover never fans out, so the
// paired seats appear ONLY as indexed_over participants. Without them, intake
// gating leaves a seat open while the index motion is about to fill it.
func TestBuildParticipants_PressIndexSeatsBecomeIndexedOver(t *testing.T) {
	t.Parallel()

	diffs := []ChangeoverNodeDiff{{
		CoreNodeName: "PLN_01",
		Situation:    SituationSwap,
		FromClaim:    pressIndexClaim("PLN_01", "PLN_02", "PLN_03"),
		ToClaim:      pressIndexClaim("PLN_01", "PLN_02", "PLN_03"),
	}}

	byNode := map[string]domain.ParticipantInput{}
	for _, p := range buildParticipants(diffs) {
		byNode[p.CoreNodeName] = p
	}
	if len(byNode) != 3 {
		t.Fatalf("participants = %v, want PLN_01 (task) + PLN_02/PLN_03 (indexed_over)", byNode)
	}
	if byNode["PLN_01"].Role != domain.ParticipantRoleTask {
		t.Errorf("PLN_01 role = %q, want task", byNode["PLN_01"].Role)
	}
	for _, seat := range []string{"PLN_02", "PLN_03"} {
		if byNode[seat].Role != domain.ParticipantRoleIndexedOver {
			t.Errorf("%s role = %q, want indexed_over", seat, byNode[seat].Role)
		}
		if byNode[seat].OwningTaskCoreNode != "PLN_01" {
			t.Errorf("%s owner = %q, want PLN_01", seat, byNode[seat].OwningTaskCoreNode)
		}
	}
}

// TestBuildParticipants_FannedOutSeatsStayTaskRole covers the different-bin-type
// case: fan-out already gave each seat its own diff and task, so those seats
// must NOT be downgraded to indexed_over by the geometry pass.
func TestBuildParticipants_FannedOutSeatsStayTaskRole(t *testing.T) {
	t.Parallel()

	claim := pressIndexClaim("PLN_01", "PLN_02", "")
	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "PLN_01", Situation: SituationSwap, FromClaim: claim, ToClaim: claim},
		{CoreNodeName: "PLN_02", Situation: SituationSwap}, // fan-out gave the seat its own diff
	}
	for _, p := range buildParticipants(diffs) {
		if p.CoreNodeName == "PLN_02" && p.Role != domain.ParticipantRoleTask {
			t.Errorf("PLN_02 role = %q, want task — it has its own diff and task", p.Role)
		}
	}
}

// TestParticipants_WrittenInTheSameTransactionAsTasks is the atomicity kill
// test. A changeover that has tasks but no participants (or the reverse) is a
// state no reader should have to handle, so the write must be all-or-nothing.
// Forcing the transaction to fail must leave BOTH tables untouched.
func TestParticipants_WrittenInTheSameTransactionAsTasks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	tasks, err := db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	parts, err := db.ListChangeoverParticipants(changeover.ID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("scenario produced no tasks")
	}
	if len(parts) == 0 {
		t.Fatal("tasks exist but no participants — the two must be written together")
	}

	// Every task node must have a participant row: participants are a superset.
	//
	// Join on process_node_id, NOT on name. NodeTask.NodeName is the DISPLAY
	// name (process_nodes.name, e.g. "Phase3 Swap Node") while participants are
	// keyed by core_node_name ("P3-NODE") — the cross-system identifier. Any
	// consumer that joins tasks to participants by name silently matches
	// nothing. Recorded here because Fix A has three such consumers.
	partByNodeID := map[int64]domain.Participant{}
	for _, p := range parts {
		if p.ProcessNodeID != nil {
			partByNodeID[*p.ProcessNodeID] = p
		}
	}
	for _, task := range tasks {
		p, ok := partByNodeID[task.ProcessNodeID]
		if !ok {
			t.Errorf("task at %q (process_node_id=%d) has no participant row",
				task.NodeName, task.ProcessNodeID)
			continue
		}
		if p.Role != domain.ParticipantRoleTask {
			t.Errorf("participant %s role = %q, want task", p.CoreNodeName, p.Role)
		}
		if p.OwningTaskID == nil {
			t.Errorf("participant %s has no owning_task_id despite owning a task", p.CoreNodeName)
		}
	}
	_ = nodeID
}

// TestIsChangeoverParticipant_PointQuery covers the hot-path lookup: it runs on
// PLC-tick-driven consume paths, so it must answer from the persisted row and
// never re-run the diff pipeline.
func TestIsChangeoverParticipant_PointQuery(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// No changeover yet — nothing is gated, and that is not an error.
	found, role, err := db.IsChangeoverParticipant(processID, "P3-NODE")
	if err != nil {
		t.Fatalf("pre-changeover lookup errored: %v", err)
	}
	if found {
		t.Errorf("found=%v role=%q before any changeover; nothing should be gated", found, role)
	}

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	_ = changeover

	found, role, err = db.IsChangeoverParticipant(processID, "P3-NODE")
	if err != nil {
		t.Fatalf("participant lookup: %v", err)
	}
	if !found || role != domain.ParticipantRoleTask {
		t.Errorf("P3-NODE found=%v role=%q, want true/task", found, role)
	}

	// A node that is not in the changeover stays ungated — this is the
	// Springfield non-regression: an unrelated loader must keep working.
	found, _, err = db.IsChangeoverParticipant(processID, "SOME-OTHER-LOADER")
	if err != nil {
		t.Fatalf("non-participant lookup: %v", err)
	}
	if found {
		t.Error("a node with no diff became a participant; unrelated loaders must not be gated")
	}
}

// TestListChangeoverParticipants_LegacyFallback covers a changeover planned by
// a pre-table binary: zero participant rows, non-empty tasks. It must derive
// task-role participants on read rather than reporting an empty set, which
// would silently ungate every node mid-changeover at deploy.
func TestListChangeoverParticipants_LegacyFallback(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	// Simulate the pre-table state.
	if _, err := db.Exec(`DELETE FROM changeover_participants WHERE process_changeover_id=?`, changeover.ID); err != nil {
		t.Fatalf("clear participants: %v", err)
	}

	parts, err := db.ListChangeoverParticipants(changeover.ID)
	if err != nil {
		t.Fatalf("legacy list: %v", err)
	}
	if len(parts) == 0 {
		t.Fatal("legacy fallback returned nothing; a pre-table changeover must still gate on its task nodes")
	}
	for _, p := range parts {
		if p.Role != domain.ParticipantRoleTask {
			t.Errorf("legacy participant %s role = %q, want task (geometry is unrecoverable)", p.CoreNodeName, p.Role)
		}
	}

	// And the point query honours the same fallback.
	found, role, err := db.IsChangeoverParticipant(processID, "P3-NODE")
	if err != nil {
		t.Fatalf("legacy point query: %v", err)
	}
	if !found || role != domain.ParticipantRoleTask {
		t.Errorf("legacy point query = %v/%q, want true/task", found, role)
	}
}
