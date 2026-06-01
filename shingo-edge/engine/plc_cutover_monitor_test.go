package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// TestApplyEdge_StateMachine drives the debounce state machine through
// the canonical sequences. The first reading is always a baseline
// (no fire). After that, only a 1→0 transition followed by 2s of
// continuous-zero produces a fire signal.
func TestApplyEdge_StateMachine(t *testing.T) {
	t.Parallel()
	type step struct {
		value    int64
		ok       bool
		advance  time.Duration
		wantFire bool
		wantPend bool // true if pendingFall should be non-nil after this step
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{
			name: "first reading establishes baseline, no fire",
			steps: []step{
				{value: 1, ok: true, wantFire: false, wantPend: false},
			},
		},
		{
			name: "stable 1 then 1 — no edge, no fire",
			steps: []step{
				{value: 1, ok: true},
				{value: 1, ok: true},
			},
		},
		{
			name: "1→0 starts debounce, full 2s confirms",
			steps: []step{
				{value: 1, ok: true},
				{value: 0, ok: true, wantPend: true},
				{value: 0, ok: true, advance: 2 * time.Second, wantFire: true, wantPend: false},
			},
		},
		{
			name: "1→0 then rebound to 1 inside window cancels",
			steps: []step{
				{value: 1, ok: true},
				{value: 0, ok: true, wantPend: true},
				{value: 1, ok: true, advance: 500 * time.Millisecond, wantPend: false},
				{value: 0, ok: true, wantPend: true}, // new edge fresh after rebound
			},
		},
		{
			name: "1→0→0→0 within sub-debounce window does not fire yet",
			steps: []step{
				{value: 1, ok: true},
				{value: 0, ok: true, wantPend: true},
				{value: 0, ok: true, advance: 500 * time.Millisecond, wantPend: true},
				{value: 0, ok: true, advance: 500 * time.Millisecond, wantPend: true},
				{value: 0, ok: true, advance: 500 * time.Millisecond, wantPend: true},
				{value: 0, ok: true, advance: 600 * time.Millisecond, wantFire: true, wantPend: false},
			},
		},
		{
			name: "PLC disconnect (ok=false) resets tracking",
			steps: []step{
				{value: 1, ok: true},
				{value: 0, ok: false, wantPend: false}, // unreadable; reset
				{value: 0, ok: true, wantPend: false},  // first valid reading after recovery is baseline
				{value: 0, ok: true, wantPend: false},  // stable 0 — no edge to detect
			},
		},
		{
			name: "0→0 with no rising history does not fire (no falling edge)",
			steps: []step{
				{value: 0, ok: true},
				{value: 0, ok: true, advance: 3 * time.Second, wantPend: false},
			},
		},
		{
			name: "rising edge 0→1 does not fire",
			steps: []step{
				{value: 0, ok: true},
				{value: 1, ok: true, wantPend: false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &cutoverProcessState{}
			now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
			for i, s := range tc.steps {
				now = now.Add(s.advance)
				gotFire := applyEdge(st, s.value, s.ok, now)
				if gotFire != s.wantFire {
					t.Errorf("step %d: fire = %v, want %v (state=%+v)", i, gotFire, s.wantFire, st)
				}
				gotPend := st.pendingFall != nil
				if gotPend != s.wantPend {
					t.Errorf("step %d: pendingFall present = %v, want %v", i, gotPend, s.wantPend)
				}
			}
		})
	}
}

// TestPLCTagInt64 covers the WarLink → int64 coercion. WarLink JSON-
// decodes integer tag values as float64 by default; the helper has to
// accept that path plus the typed Go integer types.
func TestPLCTagInt64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   interface{}
		want int64
		ok   bool
	}{
		{int(5), 5, true},
		{int32(7), 7, true},
		{int64(42), 42, true},
		{float32(3), 3, true},
		{float64(0), 0, true},
		{float64(1.0), 1, true},
		{"not a number", 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := plcTagInt64(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("plcTagInt64(%v) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCutoverFromPLC_GateBlockedDoesNotMutate replicates the spurious-
// trigger safety net: the PLC-driven entry point lands on the same
// canCompleteChangeover gate as the operator HMI path, so a falling
// edge with non-terminal tasks returns an error and leaves the
// changeover row untouched. This is what makes auto-cutover safe to
// ship without waiting on Nate to confirm tag semantics.
func TestCutoverFromPLC_GateBlockedDoesNotMutate(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)

	err := eng.CompleteProcessProductionCutoverFromPLC(processID)
	if err == nil {
		t.Fatal("expected gate to block PLC-driven cutover with non-terminal tasks")
	}
	co, _ := db.GetActiveProcessChangeover(processID)
	if co == nil {
		t.Fatal("expected changeover to still be active after gate-blocked PLC cutover")
	}
	if co.ID != changeover.ID || co.State == domain.ChangeoverCompleted {
		t.Fatalf("changeover should be unchanged in_progress, got %+v", co)
	}
	if co.TriggeredBy != "" {
		t.Errorf("triggered_by should remain empty on a gate-blocked attempt, got %q", co.TriggeredBy)
	}
}

// TestCutoverFromPLC_RecordsTriggeredBy verifies the audit column
// captures "plc-auto" when the PLC-driven entry point completes the
// cutover successfully.
func TestCutoverFromPLC_RecordsTriggeredBy(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, _ := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)

	// Drive linked orders + task to terminal so the gate passes.
	for _, orderIDPtr := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if orderIDPtr == nil {
			continue
		}
		db.UpdateOrderStatus(*orderIDPtr, string(orders.StatusSubmitted))
		db.UpdateOrderStatus(*orderIDPtr, string(orders.StatusInTransit))
		db.UpdateOrderStatus(*orderIDPtr, string(orders.StatusDelivered))
		db.UpdateOrderStatus(*orderIDPtr, string(orders.StatusConfirmed))
	}
	testutil.MustNoErr(t, db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskReleased), "update task state")

	testutil.MustNoErr(t, eng.CompleteProcessProductionCutoverFromPLC(processID), "PLC cutover")

	// Look up the now-completed changeover row by its ID directly via
	// ListChangeovers (GetActiveProcessChangeover filters out completed).
	rows, err := db.ListProcessChangeovers(processID)
	if err != nil {
		t.Fatalf("list changeovers: %v", err)
	}
	var got *processes.Changeover
	for i := range rows {
		if rows[i].ID == changeover.ID {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatal("changeover row not found")
	}
	if got.State != domain.ChangeoverCompleted {
		t.Errorf("state = %q, want completed", got.State)
	}
	if got.TriggeredBy != "plc-auto" {
		t.Errorf("triggered_by = %q, want plc-auto", got.TriggeredBy)
	}
}
