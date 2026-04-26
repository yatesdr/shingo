package www

import (
	"testing"

	"shingoedge/service"
	"shingoedge/store/processes"
)

// ═══════════════════════════════════════════════════════════════════════
// Test coverage — handlers_manual_order.go and handlers_changeover.go.
//
// Manual order: handleManualOrder unconditionally calls renderTemplate
// (no escape branch) and is admin-gated. Its admin-gate redirect is
// already covered by TestAdminPages_AdminGate_Redirects in
// handlers_admin_pages_test.go, so no new test is added here.
//
// Changeover: handleChangeover and handleChangeoverPartial both render
// templates and are public routes (no admin gate). Neither is directly
// testable. The package-level helper buildChangeoverViewData encapsulates
// nearly all of the DB call sites of both handlers and IS testable; we
// drive it across the full state space (nil process, no active
// changeover, active changeover with mixed-state node tasks).
// ═══════════════════════════════════════════════════════════════════════

func TestBuildChangeoverViewData_NilProcess(t *testing.T) {
	h, _ := newTestHandlers(t)

	d := h.buildChangeoverViewData(nil)
	if !d.AllNodesComplete {
		t.Errorf("AllNodesComplete: got false, want true (nil process short-circuits)")
	}
	if d.ActiveChangeover != nil {
		t.Errorf("ActiveChangeover: got %+v, want nil", d.ActiveChangeover)
	}
	if d.NodeTaskMap == nil {
		t.Error("NodeTaskMap: got nil, want initialised empty map")
	}
}

func TestBuildChangeoverViewData_ProcessWithoutActiveChangeover(t *testing.T) {
	h, _ := newTestHandlers(t)

	pid := seedProcess(t, "ChangeoverNoActive")
	_ = seedStyle(t, "S-A", pid)
	_ = seedStyle(t, "S-B", pid)
	process := &processes.Process{ID: pid}

	d := h.buildChangeoverViewData(process)
	// Both styles loaded.
	if len(d.Styles) != 2 {
		t.Errorf("Styles: got %d, want 2", len(d.Styles))
	}
	if d.ActiveChangeover != nil {
		t.Errorf("ActiveChangeover: got %+v, want nil (none seeded)", d.ActiveChangeover)
	}
	// No tasks → AllNodesComplete defaults to true.
	if !d.AllNodesComplete {
		t.Error("AllNodesComplete: got false, want true (no tasks pending)")
	}
}

func TestBuildChangeoverViewData_ActiveChangeoverWithPendingNodeTasks(t *testing.T) {
	h, _ := newTestHandlers(t)

	pid := seedProcess(t, "ChangeoverActive")
	fromStyleID := seedStyle(t, "From-Style", pid)
	toStyleID := seedStyle(t, "To-Style", pid)

	// Set the active style on the process so CurrentStyleName populates.
	activeStyle := fromStyleID
	if err := testDB.SetActiveStyle(pid, &activeStyle); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}

	// Seed a station + process node so the node task can attach to a station.
	stationID := seedOperatorStation(t, pid, "CO-CODE-1", "ChangeoverStation")
	_ = seedProcessNode(t, pid, stationID, "CO-NODE-A")

	// Create the changeover with one pending node task referencing the same
	// core node that we just seeded as a process node — CreateChangeover
	// matches by core_node_name and reuses the existing process_node row.
	existing, err := testDB.ListProcessNodesByProcess(pid)
	if err != nil {
		t.Fatalf("ListProcessNodesByProcess: %v", err)
	}
	from := fromStyleID
	_, err = service.NewChangeoverService(testDB).Create(
		pid,
		&from,
		toStyleID,
		"test",
		"test changeover",
		[]int64{stationID},
		[]processes.NodeTaskInput{{
			ProcessID:    pid,
			CoreNodeName: "CO-NODE-A",
			Situation:    "switch",
			State:        "pending",
		}},
		existing,
	)
	if err != nil {
		t.Fatalf("CreateChangeover: %v", err)
	}

	process := &processes.Process{ID: pid, ActiveStyleID: &fromStyleID}
	d := h.buildChangeoverViewData(process)

	if d.ActiveChangeover == nil {
		t.Fatal("ActiveChangeover: got nil, want loaded changeover")
	}
	if d.ActiveChangeover.State != "active" {
		t.Errorf("ActiveChangeover.State: got %q, want active", d.ActiveChangeover.State)
	}
	if d.CurrentStyleName != "From-Style" {
		t.Errorf("CurrentStyleName: got %q, want From-Style", d.CurrentStyleName)
	}
	if len(d.StationTasks) != 1 {
		t.Errorf("StationTasks: got %d, want 1", len(d.StationTasks))
	}
	if len(d.NodeTaskMap[stationID]) != 1 {
		t.Errorf("NodeTaskMap[stationID]: got %d, want 1", len(d.NodeTaskMap[stationID]))
	}
	// Pending node task → AllNodesComplete must be false.
	if d.AllNodesComplete {
		t.Error("AllNodesComplete: got true, want false (pending task present)")
	}
}

func TestBuildChangeoverViewData_AllSwitchedTasksMarkComplete(t *testing.T) {
	h, _ := newTestHandlers(t)

	pid := seedProcess(t, "ChangeoverAllSwitched")
	from := seedStyle(t, "From2", pid)
	to := seedStyle(t, "To2", pid)
	activeStyle := from
	if err := testDB.SetActiveStyle(pid, &activeStyle); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}
	stationID := seedOperatorStation(t, pid, "CO-CODE-2", "Station2")
	_ = seedProcessNode(t, pid, stationID, "CO-NODE-B")

	existing, _ := testDB.ListProcessNodesByProcess(pid)
	fromCopy := from
	cid, err := service.NewChangeoverService(testDB).Create(
		pid, &fromCopy, to, "tester", "",
		[]int64{stationID},
		[]processes.NodeTaskInput{{
			ProcessID:    pid,
			CoreNodeName: "CO-NODE-B",
			Situation:    "switch",
			State:        "switched", // already considered complete by the helper
		}},
		existing,
	)
	if err != nil {
		t.Fatalf("CreateChangeover: %v", err)
	}
	_ = cid

	process := &processes.Process{ID: pid, ActiveStyleID: &from}
	d := h.buildChangeoverViewData(process)
	if !d.AllNodesComplete {
		t.Error("AllNodesComplete: got false, want true (only switched tasks)")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Central node tasks bucket — when a node task's process node has no
// operator station, it lands in CentralNodeTasks instead of NodeTaskMap.
// ═══════════════════════════════════════════════════════════════════════

func TestBuildChangeoverViewData_CentralNodeTasksWhenNoStation(t *testing.T) {
	h, _ := newTestHandlers(t)

	pid := seedProcess(t, "ChangeoverCentral")
	from := seedStyle(t, "FromC", pid)
	to := seedStyle(t, "ToC", pid)
	activeStyle := from
	if err := testDB.SetActiveStyle(pid, &activeStyle); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}
	// Process node with stationID=0 → OperatorStationID nil.
	_ = seedProcessNode(t, pid, 0, "CO-CENTRAL-A")

	existing, _ := testDB.ListProcessNodesByProcess(pid)
	fromCopy := from
	if _, err := service.NewChangeoverService(testDB).Create(
		pid, &fromCopy, to, "tester", "",
		nil, // no station tasks
		[]processes.NodeTaskInput{{
			ProcessID:    pid,
			CoreNodeName: "CO-CENTRAL-A",
			Situation:    "switch",
			State:        "pending",
		}},
		existing,
	); err != nil {
		t.Fatalf("CreateChangeover: %v", err)
	}

	process := &processes.Process{ID: pid, ActiveStyleID: &from}
	d := h.buildChangeoverViewData(process)

	if len(d.CentralNodeTasks) != 1 {
		t.Errorf("CentralNodeTasks: got %d, want 1 (unstationed node task)",
			len(d.CentralNodeTasks))
	}
	if d.AllNodesComplete {
		t.Error("AllNodesComplete: got true, want false (pending central task)")
	}
}
