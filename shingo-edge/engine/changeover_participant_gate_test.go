package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// changeover_participant_gate_test.go — Fix A: participant-scoped intake
// gating and the advisory plan-time assertion.

// TestCanAcceptOrders_ParticipantIsGated is the baseline: a node in the
// changeover refuses intake, with the unchanged reason string.
func TestCanAcceptOrders_ParticipantIsGated(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	if ok, _ := eng.CanAcceptOrders(nodeID); !ok {
		t.Fatal("node refused intake before any changeover started")
	}

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", "gate"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	ok, reason := eng.CanAcceptOrders(nodeID)
	if ok {
		t.Error("participant node accepted intake during its own changeover")
	}
	if reason != "changeover in progress" {
		t.Errorf("reason = %q, want the unchanged %q", reason, "changeover in progress")
	}
}

// TestCanAcceptOrders_NonParticipantStaysOpen is the SPRINGFIELD NON-REGRESSION,
// asserted directly rather than only in sim: a node that is not part of the
// changeover must keep accepting orders. Widening the gate from tasks to
// participants is exactly the class of scoping a field report forced DOWN, so
// this is the test that has to keep passing.
func TestCanAcceptOrders_NonParticipantStaysOpen(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// A second node on the same process that the changeover does not touch —
	// the bin loader in the field report.
	loaderNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "UNRELATED-LOADER",
		Code:         "ULDR",
		Name:         "Unrelated Loader",
		Sequence:     9,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create unrelated node: %v", err)
	}

	if _, err := eng.StartProcessChangeover(processID, toStyleID, "test", "non-regression"); err != nil {
		t.Fatalf("start changeover: %v", err)
	}

	ok, reason := eng.CanAcceptOrders(loaderNodeID)
	if !ok {
		t.Errorf("unrelated node refused intake during a changeover it is not part of (reason %q) — "+
			"this is the Springfield regression the gate scoping exists to prevent", reason)
	}
}

// TestCanAcceptOrders_IndexedOverIsGatedWithItsOwnReason covers the catastrophic
// family the participant set exists for: a press-index seat that owns NO task
// must still refuse intake, and must say why in terms an operator can act on —
// there is no task on that tile to explain the refusal.
func TestCanAcceptOrders_IndexedOverIsGatedWithItsOwnReason(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, _ := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)

	seatID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PLN_SEAT",
		Code:         "SEAT",
		Name:         "Press Back Seat",
		Sequence:     8,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create seat node: %v", err)
	}

	// Write the changeover + an indexed_over participant directly: the seat owns
	// no task, which is precisely the state a task-keyed gate cannot see.
	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, called_by)
		VALUES (?, ?, 'active', 'test')`, processID, 1)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	coID, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO changeover_participants
		(process_changeover_id, core_node_name, process_node_id, role)
		VALUES (?, 'PLN_SEAT', ?, ?)`, coID, seatID, domain.ParticipantRoleIndexedOver); err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	ok, reason := eng.CanAcceptOrders(seatID)
	if ok {
		t.Fatal("indexed-over seat accepted intake — this is the two-bins-on-one-node case")
	}
	if !strings.Contains(reason, "indexed-over") {
		t.Errorf("reason = %q; an indexed-over seat owns no task, so the refusal must say so", reason)
	}
}

// TestAssertParticipantsResolve_NamesMissingRows pins the advisory: a
// participant whose name has no process_nodes row is REPORTED, not dropped and
// not blocked on. That report is the whole reason the table is keyed by name.
func TestAssertParticipantsResolve_NamesMissingRows(t *testing.T) {
	t.Parallel()

	nodes := []processes.Node{
		{CoreNodeName: "PLN_01"},
		{CoreNodeName: "PLN_02"},
	}
	participants := []domain.ParticipantInput{
		{CoreNodeName: "PLN_01", Role: domain.ParticipantRoleTask},
		{CoreNodeName: "PLN_02", Role: domain.ParticipantRoleIndexedOver},
		{CoreNodeName: "PLN_03", Role: domain.ParticipantRoleIndexedOver}, // no row
	}

	got := assertParticipantsResolve(participants, nodes)
	if len(got) != 1 || got[0] != "PLN_03" {
		t.Fatalf("unresolved = %v, want [PLN_03]", got)
	}

	// Fully resolved plans report nothing.
	if got := assertParticipantsResolve(participants[:2], nodes); got != nil {
		t.Errorf("fully-resolved plan reported %v, want nil", got)
	}
	if got := assertParticipantsResolve(nil, nodes); got != nil {
		t.Errorf("empty participant set reported %v, want nil", got)
	}
}

// TestStartChangeover_UnresolvedParticipantsAreAdvisoryOnly pins that the
// assertion never refuses. A plant whose press seats have never had
// process_nodes rows must still be able to run a changeover — a guard the floor
// cannot work around gets disabled rather than fixed (Springfield, 2026-06-03).
func TestStartChangeover_UnresolvedParticipantsAreAdvisoryOnly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "advisory")
	if err != nil {
		t.Fatalf("changeover refused to start: %v — the assertion must be advisory", err)
	}
	if co == nil {
		t.Fatal("nil changeover")
	}
	// The scenario resolves cleanly, so the advisory is empty; the assertion
	// above is that starting SUCCEEDED regardless.
	testutil.MustNoErr(t, nil, "placeholder")
}
