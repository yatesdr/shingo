//go:build docker

package service

import (
	"testing"

	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// seedProcessStyle is a local helper for changeover tests — inserts a
// process and one style and returns their ids. Inlined here (not
// imported from store/store_coverage_test.go) because that helper is
// package-private to package store.
func seedProcessStyle(t *testing.T, db *store.DB, procName, styleName string) (int64, int64) {
	t.Helper()
	pid, err := db.CreateProcess(procName, "desc", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateStyle(styleName, "style desc", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	return pid, sid
}

// TestChangeover_CreateAtomic exercises ChangeoverService.Create in
// the full-fixture path: existing process node reused, new node auto-
// created, station tasks inserted, runtime rows ensured, target
// style + production state flipped on the process.
//
// Phase 6.4a moved the transaction body in from
// store/process_changeovers.go's deleted (db *DB).CreateChangeover;
// this test was previously TestProcessChangeovers_CreateAtomic in
// store/store_coverage_test.go.

func TestChangeover_CreateAtomic(t *testing.T) {
	db := testdb.Open(t)
	svc := NewChangeoverService(db)

	pid, fromStyle := seedProcessStyle(t, db, "P", "S-FROM")
	toStyle, _ := db.CreateStyle("S-TO", "", pid)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})
	// Seed an existing process node (the atomic create should reuse it
	// instead of auto-creating a duplicate).
	existNode, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "N-EXIST",
		Code: "NE", Name: "NE", Sequence: 1, Enabled: true,
	})
	existingNodes, _ := db.ListProcessNodesByProcess(pid)

	cid, err := svc.Create(
		pid, &fromStyle, toStyle, "alice", "swap A",
		[]int64{sid},
		[]processes.NodeTaskInput{
			{ProcessID: pid, CoreNodeName: "N-EXIST", Situation: "refill", State: "waiting"},
			{ProcessID: pid, CoreNodeName: "N-NEW", Situation: "introduce", State: "waiting"},
		},
		existingNodes,
	)
	if err != nil {
		t.Fatalf("create changeover: %v", err)
	}
	if cid == 0 {
		t.Fatal("expected nonzero id")
	}

	// Process target style + production state updated.
	proc, _ := db.GetProcess(pid)
	if proc.TargetStyleID == nil || *proc.TargetStyleID != toStyle {
		t.Errorf("target style = %v, want %d", proc.TargetStyleID, toStyle)
	}
	if proc.ProductionState != "changeover_active" {
		t.Errorf("production_state = %q", proc.ProductionState)
	}

	// Station tasks created.
	stTasks, _ := db.ListChangeoverStationTasks(cid)
	if len(stTasks) != 1 || stTasks[0].OperatorStationID != sid || stTasks[0].State != "waiting" {
		t.Errorf("station tasks: %+v", stTasks)
	}

	// Node tasks: one reuses existing, one auto-creates.
	nodeTasks, _ := db.ListChangeoverNodeTasks(cid)
	if len(nodeTasks) != 2 {
		t.Fatalf("node tasks len = %d", len(nodeTasks))
	}
	var reusedExisting bool
	for _, nt := range nodeTasks {
		if nt.ProcessNodeID == existNode {
			reusedExisting = true
		}
	}
	if !reusedExisting {
		t.Error("expected the existing process node to be reused")
	}

	// Runtime rows created for every node task.
	for _, nt := range nodeTasks {
		if _, err := db.GetProcessNodeRuntime(nt.ProcessNodeID); err != nil {
			t.Errorf("runtime missing for node %d: %v", nt.ProcessNodeID, err)
		}
	}

	// Active changeover lookup.
	active, err := db.GetActiveProcessChangeover(pid)
	if err != nil || active.ID != cid {
		t.Errorf("active: %v %+v", err, active)
	}
	if active.FromStyleName != "S-FROM" || active.ToStyleName != "S-TO" {
		t.Errorf("joined style names: %+v", active)
	}
}

func TestChangeover_StateTransitionsAndListing(t *testing.T) {
	db := testdb.Open(t)
	svc := NewChangeoverService(db)

	pid, fromStyle := seedProcessStyle(t, db, "P", "F")
	toStyle, _ := db.CreateStyle("T", "", pid)

	cid, err := svc.Create(pid, &fromStyle, toStyle, "x", "", nil, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// In-flight state transitions.
	if err := db.UpdateProcessChangeoverState(cid, "phase_3"); err != nil {
		t.Fatalf("update state: %v", err)
	}

	// Second changeover to test listing and exclusion-by-state.
	cid2, _ := svc.Create(pid, &fromStyle, toStyle, "y", "", nil, nil, nil)
	_ = cid2

	list, _ := db.ListProcessChangeovers(pid)
	if len(list) != 2 {
		t.Errorf("list len = %d", len(list))
	}

	// Mark first as completed → GetActive returns the remaining one only.
	if err := db.UpdateProcessChangeoverState(cid, "completed"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	active, err := db.GetActiveProcessChangeover(pid)
	if err != nil {
		t.Fatalf("get active after complete: %v", err)
	}
	if active.ID == cid {
		t.Error("active still points at completed changeover")
	}

	// Completed changeover has completed_at populated.
	histList, _ := db.ListProcessChangeovers(pid)
	var completed *processes.Changeover
	for i := range histList {
		if histList[i].ID == cid {
			completed = &histList[i]
		}
	}
	if completed == nil || completed.CompletedAt == nil {
		t.Errorf("expected completed_at to be populated: %+v", completed)
	}
}

func TestChangeover_NodeAndStationTaskMutations(t *testing.T) {
	db := testdb.Open(t)
	svc := NewChangeoverService(db)

	pid, fromStyle := seedProcessStyle(t, db, "P", "F")
	toStyle, _ := db.CreateStyle("T", "", pid)
	sid, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "S"})

	// We also want node tasks filtered by station.
	nid, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "N",
		Code: "N", Name: "N", Sequence: 1, Enabled: true,
	})
	existingNodes, _ := db.ListProcessNodesByProcess(pid)

	cid, err := svc.Create(pid, &fromStyle, toStyle, "x", "",
		[]int64{sid},
		[]processes.NodeTaskInput{{ProcessID: pid, CoreNodeName: "N", Situation: "refill", State: "waiting"}},
		existingNodes)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Station task lookup + update.
	st, err := db.GetChangeoverStationTaskByStation(cid, sid)
	if err != nil {
		t.Fatalf("get station task: %v", err)
	}
	if err := db.UpdateChangeoverStationTaskState(st.ID, "in_progress"); err != nil {
		t.Fatalf("update station task: %v", err)
	}
	st2, _ := db.GetChangeoverStationTaskByStation(cid, sid)
	if st2.State != "in_progress" {
		t.Errorf("station task state = %q", st2.State)
	}

	// Node task lookup + update + link orders.
	nt, err := db.GetChangeoverNodeTaskByNode(cid, nid)
	if err != nil {
		t.Fatalf("get node task: %v", err)
	}
	if err := db.UpdateChangeoverNodeTaskState(nt.ID, "in_progress"); err != nil {
		t.Fatalf("update node task: %v", err)
	}

	// Link material orders.
	orderA, _ := db.CreateOrder("next", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	orderB, _ := db.CreateOrder("old", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	if err := db.LinkChangeoverNodeOrders(nt.ID, &orderA, &orderB); err != nil {
		t.Fatalf("link orders: %v", err)
	}
	nt2, _ := db.GetChangeoverNodeTaskByNode(cid, nid)
	if nt2.State != "in_progress" {
		t.Errorf("node task state = %q", nt2.State)
	}
	if nt2.NextMaterialOrderID == nil || *nt2.NextMaterialOrderID != orderA {
		t.Errorf("next material = %v", nt2.NextMaterialOrderID)
	}
	if nt2.OldMaterialReleaseOrderID == nil || *nt2.OldMaterialReleaseOrderID != orderB {
		t.Errorf("old material = %v", nt2.OldMaterialReleaseOrderID)
	}

	// Partial link (only old) — COALESCE keeps previous next.
	orderC, _ := db.CreateOrder("old2", "retrieve", &nid, false, 1, "", "", "", "", false, "")
	if err := db.LinkChangeoverNodeOrders(nt.ID, nil, &orderC); err != nil {
		t.Fatalf("partial link: %v", err)
	}
	nt3, _ := db.GetChangeoverNodeTaskByNode(cid, nid)
	if nt3.NextMaterialOrderID == nil || *nt3.NextMaterialOrderID != orderA {
		t.Errorf("partial link: next should be preserved: %v", nt3.NextMaterialOrderID)
	}
	if nt3.OldMaterialReleaseOrderID == nil || *nt3.OldMaterialReleaseOrderID != orderC {
		t.Errorf("partial link: old should be updated: %v", nt3.OldMaterialReleaseOrderID)
	}

	// ListChangeoverNodeTasksByStation — only node tasks for nodes attached to sid.
	byStation, err := db.ListChangeoverNodeTasksByStation(cid, sid)
	if err != nil {
		t.Fatalf("list by station: %v", err)
	}
	if len(byStation) != 1 {
		t.Errorf("by station = %d, want 1", len(byStation))
	}
}
