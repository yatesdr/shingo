// changeover_door_test.go — the one door every completion of a changeover goes
// through (completeCutover), its force mode, the admin switch dispatched onto
// it, the pokes that replaced the completion chain's own completions, and the
// restore.
package engine

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// seedRigManager is a PLC manager with no WarLink: publishing is a no-op.
func seedRigManager() *plc.Manager {
	return plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, nil)
}

// openGreenChangeover opens a changeover on a fresh process with no node tasks
// and no orders, so its gate is green.
func openGreenChangeover(t *testing.T, eng *Engine, counterTag string) (processID, fromID, toID, coID int64) {
	t.Helper()
	db := eng.db
	plcName := ""
	if counterTag != "" {
		plcName = "plc"
	}
	processID, err := db.CreateProcess("CO-PROC", "", plcName, counterTag, counterTag != "")
	testutil.MustNoErr(t, err, "create process")
	fromID, err = db.CreateStyle("CO-FROM", "", processID)
	testutil.MustNoErr(t, err, "create from style")
	toID, err = db.CreateStyle("CO-TO", "", processID)
	testutil.MustNoErr(t, err, "create to style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromID), "active style")
	coID, err = eng.changeoverService.Create(processID, &fromID, toID, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "create changeover")
	return processID, fromID, toID, coID
}

// mustProcess reads a process row or fails the test.
func mustProcess(t *testing.T, db *store.DB, id int64) *processes.Process {
	t.Helper()
	p, err := db.GetProcess(id)
	testutil.MustNoErr(t, err, "get process")
	return p
}

func changeoverRow(t *testing.T, db *store.DB, id int64) *processes.Changeover {
	t.Helper()
	co, err := db.GetProcessChangeoverByID(id)
	testutil.MustNoErr(t, err, "get changeover")
	if co == nil {
		t.Fatalf("changeover %d is gone", id)
	}
	return co
}

// countStateWrites counts the terminal and non-terminal state writes to
// process_changeovers among the statements the counter saw.
func countStateWrites(c *store.QueryCounter) int {
	n := 0
	for _, s := range c.Statements() {
		if strings.HasPrefix(strings.TrimSpace(s), "UPDATE process_changeovers SET state=") {
			n++
		}
	}
	return n
}

// TestDoor_WrongChangeoverIDIsANoOp: a decision taken for one changeover never
// completes another, and the CUTOVER on a process with nothing open is a
// logged no-op rather than a raw "sql: no rows" to the operator.
func TestDoor_WrongChangeoverIDIsANoOp(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, fromID, _, coID := openGreenChangeover(t, eng, "")

	if _, err := eng.completeCutover(processID, coID+1, triggeredAutoCATID, CutoverGated); !errors.Is(err, errNotTheOpenChangeover) {
		t.Fatalf("door with a stale changeover id: %v, want the no-op reported", err)
	}
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Fatalf("a decision for changeover %d completed changeover %d", coID+1, coID)
	}
	if p := mustProcess(t, db, processID); p.ActiveStyleID == nil || *p.ActiveStyleID != fromID {
		t.Fatalf("a stale id flipped the style: %v", p.ActiveStyleID)
	}

	other, err := db.CreateProcess("NO-CO", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	if err := eng.CompleteProcessProductionCutover(other); err != nil {
		t.Errorf("CUTOVER with no changeover open = %v, want a no-op", err)
	}
}

// TestFire_ANoOpDoorNeitherLogsCutOverNorNotifies: the monitor decided on a
// changeover the operator then completed before the fire. The door finds it
// not open and does nothing; the fire says so and raises no automatic-cutover
// notice. Before, the door's no-op returned as a completion: the fire logged
// "cut over" and the station showed an automatic cutover nobody made.
func TestFire_ANoOpDoorNeitherLogsCutOverNorNotifies(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, _, _, coID := openGreenChangeover(t, eng, "")
	mon := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	eng.catidMon = mon
	notices := 0
	eng.Events.SubscribeTypes(func(Event) { notices++ }, EventCATIDAutoCutover)
	var lines []string
	eng.logFn = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(processID), "the operator's CUTOVER")
	mon.fireAutoCutover(autoCutoverIntent{processID: processID, processName: "CO-PROC", changeoverID: coID,
		catid: "1002", mode: domain.ChangeoverAutoArmAuto})
	if notices != 0 {
		t.Errorf("automatic-cutover notices = %d, want none: the operator completed it", notices)
	}
	for _, l := range lines {
		if strings.HasSuffix(l, "— cut over") {
			t.Errorf("the fire logged a cutover it did not make: %q", l)
		}
	}
}

// TestDoor_SerializesOperatorAndMonitor: the operator's CUTOVER and the
// monitor's fire for the same changeover queue on the door's mutex; one
// completes the row, the other finds it ended and does nothing.
func TestDoor_SerializesOperatorAndMonitor(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	processID, _, toID, coID := openGreenChangeover(t, eng, "")
	mon := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	eng.catidMon = mon

	counter.Reset()
	eng.cutoverMu.Lock() // a third caller is inside the door
	var wg sync.WaitGroup
	var opErr error
	wg.Add(2)
	go func() { defer wg.Done(); opErr = eng.CompleteProcessProductionCutover(processID) }()
	go func() {
		defer wg.Done()
		mon.fireAutoCutover(autoCutoverIntent{processID: processID, processName: "CO-PROC", changeoverID: coID,
			catid: "1002", mode: domain.ChangeoverAutoArmAuto})
	}()
	time.Sleep(100 * time.Millisecond)
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Fatal("a completion ran while another caller held the door")
	}
	eng.cutoverMu.Unlock()
	wg.Wait()

	if opErr != nil {
		t.Fatalf("operator CUTOVER: %v", opErr)
	}
	co := changeoverRow(t, db, coID)
	if co.State != domain.ChangeoverCompleted || (co.TriggeredBy != triggeredManual && co.TriggeredBy != triggeredAutoCATID) {
		t.Fatalf("row = %s by %q, want completed by one of the two", co.State, co.TriggeredBy)
	}
	if n := countStateWrites(counter); n != 1 {
		t.Errorf("the row's state was written %d times, want once", n)
	}
	if p := mustProcess(t, db, processID); p.ActiveStyleID == nil || *p.ActiveStyleID != toID {
		t.Errorf("active style = %v, want the target %d", p.ActiveStyleID, toID)
	}
}

// styleOf renders an active-style pointer for a failure line.
func styleOf(p *int64) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// TestCancel_InsideTheDoorsFlipWaitsForTheDoor: a Cancel started while the door
// is flipping (from the plant-claims publish inside the flip) waits for the
// door: the changeover completes on its target and the Cancel finds nothing
// open, so the style and the row agree. Before, the Cancel ran inside the
// flip: the row ended cancelled with the process on the cancelled target.
func TestCancel_InsideTheDoorsFlipWaitsForTheDoor(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, fromID, toID, coID := openGreenChangeover(t, eng, "")
	cancelled := make(chan error, 1)
	var once sync.Once
	eng.SetPlantClaimsFunc(func(int64) {
		once.Do(func() {
			go func() { cancelled <- eng.CancelProcessChangeover(processID) }()
			select {
			case err := <-cancelled: // the Cancel ran inside the flip
				cancelled <- err
			case <-time.After(time.Second): // it waits on the door
			}
		})
	})
	doorErr := eng.CompleteProcessProductionCutover(processID)
	cancelErr := <-cancelled
	co := changeoverRow(t, db, coID)
	active := mustProcess(t, db, processID).ActiveStyleID
	agree := (co.State == domain.ChangeoverCompleted && styleOf(active) == fmt.Sprint(toID)) ||
		(co.State == domain.ChangeoverCancelled && styleOf(active) == fmt.Sprint(fromID))
	if !agree {
		t.Fatalf("row %s with the process on style %s (from %d, to %d): the style and the row disagree (door: %v, cancel: %v)",
			co.State, styleOf(active), fromID, toID, doorErr, cancelErr)
	}
	if doorErr != nil || co.State != domain.ChangeoverCompleted {
		t.Errorf("door = %v, row %s: the door, already inside its flip, completes", doorErr, co.State)
	}
	if cancelErr == nil {
		t.Error("the Cancel after the completion found a changeover to cancel")
	}
}

// TestStart_RacingTheSwitchNeverInsertsAFromStyleNoLongerActive: an admin
// switch (here clearing the running style) fired while a start is planning
// (from the planner's log line, after its read of the active style) waits for
// the start, which inserts its row from the style still running; the switch
// then finds a changeover open and is refused. Before, a switch ran inside the
// start, and the row from A was inserted with A no longer running: a
// changeover no predicate completes.
func TestStart_RacingTheSwitchNeverInsertsAFromStyleNoLongerActive(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	pid, nodeID, from, to, _, _ := seedChangeoverScenario(t, db)
	// A queued order at the changing node: the start cancels it after the
	// planner has read the active style, and logs it before the insert.
	seedOrderAt(t, db, nodeID, "CO-NODE", "uuid-race-queued", "PART-OLD", orders.StatusQueued)
	switched := make(chan error, 1)
	var once sync.Once
	logFn := eng.logFn
	eng.logFn = func(format string, args ...any) {
		if strings.Contains(format, "pre-dispatch order(s) at start") {
			once.Do(func() {
				go func() { switched <- eng.SetProcessActiveStyle(pid, nil) }()
				select {
				case err := <-switched: // the switch ran inside the start
					switched <- err
				case <-time.After(time.Second): // it waits on the start
				}
			})
		}
		logFn(format, args...)
	}
	_, startErr := eng.StartProcessChangeover(pid, to, "test", "")
	switchErr := <-switched
	p := mustProcess(t, db, pid)
	if open, err := db.GetActiveProcessChangeover(pid); err == nil && styleOf(open.FromStyleID) != styleOf(p.ActiveStyleID) {
		t.Fatalf("changeover %d from style %s is open while style %s runs (start: %v, switch: %v)",
			open.ID, styleOf(open.FromStyleID), styleOf(p.ActiveStyleID), startErr, switchErr)
	}
	var refused *SwitchRefused
	if startErr != nil || styleOf(p.ActiveStyleID) != fmt.Sprint(from) || !errors.As(switchErr, &refused) {
		t.Errorf("start = %v, active style %s, switch = %v: the start, already planning, inserts and the switch is refused",
			startErr, styleOf(p.ActiveStyleID), switchErr)
	}
}

// TestStart_RacingTheSwitchLeavesTheStartItsRow: row 10. An admin switch to
// the target style fired once the start's row is in (from the first leg the
// start orders, with the door open again) completes the changeover, and the
// start still returns its row, completed, rather than "sql: no rows". Before,
// the start read its row back as the process's open changeover, after the door
// unlocked, so a switch in between left it nothing to read.
func TestStart_RacingTheSwitchLeavesTheStartItsRow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	pid, _, _, to := seedPhase3SwapScenario(t, db)
	switched := make(chan error, 1)
	ranInside := false
	var once sync.Once
	eng.Events.SubscribeTypes(func(Event) {
		once.Do(func() {
			go func() { switched <- eng.SetProcessActiveStyle(pid, &to) }()
			select {
			case err := <-switched: // the switch ran while the start ordered its legs
				ranInside = true
				switched <- err
			case <-time.After(2 * time.Second): // it waits on the start
			}
		})
	}, EventOrderCreated)
	co, startErr := eng.StartProcessChangeover(pid, to, "test", "")
	var switchErr error
	select {
	case switchErr = <-switched:
	case <-time.After(5 * time.Second):
		t.Fatalf("the start (%+v, %v) ordered no leg, so no switch was fired", co, startErr)
	}
	if !ranInside {
		t.Fatalf("the switch did not run while the start ordered its legs (switch: %v): the race is not exercised", switchErr)
	}
	if switchErr != nil {
		t.Fatalf("the switch to the target: %v", switchErr)
	}
	if startErr != nil || co == nil || co.ToStyleID != to || co.State != domain.ChangeoverCompleted {
		t.Errorf("start = %+v, %v; want its own row back, completed by the switch", co, startErr)
	}
}

// TestDoor_TerminalWriteLatchedForCompleteAndCancel: an ended row stays as it
// ended. A second completion does not rewrite completed_at or triggered_by,
// and a cancel does not overwrite a completion.
func TestDoor_TerminalWriteLatchedForCompleteAndCancel(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, _, _, coID := openGreenChangeover(t, eng, "")
	testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(processID), "cutover")
	before := changeoverRow(t, db, coID)

	time.Sleep(1100 * time.Millisecond) // a rewrite would carry a later second
	if err := db.UpdateProcessChangeoverStateWithTrigger(coID, domain.ChangeoverCompleted, "again", ""); !errors.Is(err, processes.ErrChangeoverEnded) {
		t.Errorf("second completion = %v, want ErrChangeoverEnded", err)
	}
	if err := db.UpdateProcessChangeoverState(coID, domain.ChangeoverCancelled); !errors.Is(err, processes.ErrChangeoverEnded) {
		t.Errorf("cancel of a completed row = %v, want ErrChangeoverEnded", err)
	}
	after := changeoverRow(t, db, coID)
	if after.State != domain.ChangeoverCompleted || after.TriggeredBy != before.TriggeredBy ||
		!after.CompletedAt.Equal(*before.CompletedAt) {
		t.Errorf("row after = %s by %q at %v, want %s by %q at %v", after.State, after.TriggeredBy, after.CompletedAt,
			before.State, before.TriggeredBy, before.CompletedAt)
	}
}

// TestDoor_CompletionChainOnlyPokes: a flipped row with a delivered,
// unconfirmed placing leg. The door's pre-pass confirms the leg, and the
// completion cascade that runs on the confirm only pokes the monitor — the
// row is finalized once, by the door. Before, the cascade completed it too.
func TestDoor_CompletionChainOnlyPokes(t *testing.T) {
	t.Parallel()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	eng.cutoverPokes = make(chan int64, 16)
	processID, evacID, coID := start2PrimeScenario(t, eng)
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(evacID,
		`[{"action":"pickup","node":"MARKET-X"},{"action":"dropoff","node":"P3-NODE"}]`), "placing steps")
	testutil.MustNoErr(t, db.UpdateOrderStatus(evacID, string(orders.StatusDelivered)), "delivered")
	co := changeoverRow(t, db, coID)
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &co.ToStyleID), "flip active style")

	counter.Reset()
	testutil.MustNoErr(t, eng.CompleteProcessProductionCutover(processID), "cutover")
	if n := countStateWrites(counter); n != 1 {
		t.Errorf("the row's state was written %d times, want once (the door's)", n)
	}
	if got := changeoverRow(t, db, coID); got.State != domain.ChangeoverCompleted || got.TriggeredBy != triggeredManual {
		t.Errorf("row = %s by %q, want completed by %q", got.State, got.TriggeredBy, triggeredManual)
	}
	select {
	case pid := <-eng.cutoverPokes:
		if pid != processID {
			t.Errorf("poked process %d, want %d", pid, processID)
		}
	default:
		t.Error("the confirm's completion cascade did not poke the monitor")
	}
}

// TestRestore_FlippedRowCompletesThroughTheDoor: a row whose process was
// flipped to the target before the Edge stopped is finished by the restore,
// through the door. A terminal transition alone no longer finishes it — the
// chain pokes, and only the monitor or a person completes.
func TestRestore_FlippedRowCompletesThroughTheDoor(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, _, toStyleID := seedPhase3SwapScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	changeover, _ := startChangeover(t, eng, db, processID, toStyleID)
	task, err := db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	testutil.MustNoErr(t, err, "get task")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &toStyleID), "the door's flip, before a crash")
	for _, orderIDPtr := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if orderIDPtr != nil {
			markOrderTerminal(db, *orderIDPtr)
		}
	}
	releaseOrder, err := db.GetOrder(*task.OldMaterialReleaseOrderID)
	testutil.MustNoErr(t, err, "get release order")
	emitOrderCompleted(eng, releaseOrder.ID, releaseOrder.UUID, releaseOrder.OrderType, &nodeID)
	if _, err := db.GetActiveProcessChangeover(processID); err != nil {
		t.Fatalf("a terminal transition completed the changeover (%v); the chain only pokes", err)
	}

	eng.restoreChangeoverState()
	got := changeoverRow(t, db, changeover.ID)
	if got.State != domain.ChangeoverCompleted || got.TriggeredBy != triggeredRestore {
		t.Errorf("row after restore = %s by %q, want completed by %q", got.State, got.TriggeredBy, triggeredRestore)
	}
}

// TestFlip_AdminSwitchCreatesThePointInTheEngine: the admin switch syncs the
// point inside the flip, not in the HTTP handler — a new process's first
// "Mark as running" counts from the engine verb alone.
func TestFlip_AdminSwitchCreatesThePointInTheEngine(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.plcMgr = seedRigManager()
	processID, err := db.CreateProcess("NEW-CELL", "", "plc", "MES_Cell.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle("FIRST", "", processID)
	testutil.MustNoErr(t, err, "create style")

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &styleID), "admin switch")
	rp, err := db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	if err != nil || rp == nil || rp.StyleID != styleID {
		t.Fatalf("reporting point = %+v (%v), want one on style %d", rp, err, styleID)
	}
}

// openRedChangeover starts a real changeover whose node task and orders are in
// flight, so its gate is red.
func openRedChangeover(t *testing.T) (eng *Engine, db *store.DB, processID, fromID, toID, coID int64, task *processes.NodeTask) {
	t.Helper()
	db = testEngineDB(t)
	processID, _, fromID, toID = seedPhase3SwapScenario(t, db)
	eng = testEngine(t, db)
	eng.wireEventHandlers()
	co, task := startChangeover(t, eng, db, processID, toID)
	if ok, _, err := eng.canCompleteChangeover(co.ID); err != nil || ok {
		t.Fatalf("fixture gate = %v (%v), want red", ok, err)
	}
	return eng, db, processID, fromID, toID, co.ID, task
}

// TestAdminSwitch_ToTargetCompletesPastTheGate: "Mark as running" on the open
// changeover's target completes it past its red gate, writes what it
// overrode, and leaves the legs alone.
func TestAdminSwitch_ToTargetCompletesPastTheGate(t *testing.T) {
	t.Parallel()
	eng, db, processID, _, toID, coID, task := openRedChangeover(t)

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &toID), "admin switch")
	co := changeoverRow(t, db, coID)
	if co.State != domain.ChangeoverCompleted || co.TriggeredBy != triggeredAdminSwitch+triggeredForced {
		t.Fatalf("row = %s by %q, want completed by admin-switch-forced", co.State, co.TriggeredBy)
	}
	if !strings.Contains(co.Notes, "forced past: ") || !strings.Contains(co.Notes, "task at node") {
		t.Errorf("notes = %q, want the overridden blockers", co.Notes)
	}
	if p := mustProcess(t, db, processID); p.ActiveStyleID == nil || *p.ActiveStyleID != toID || p.TargetStyleID != nil {
		t.Errorf("process after = active %v target %v, want active %d and no target", p.ActiveStyleID, p.TargetStyleID, toID)
	}
	assertLegsUntouched(t, db, task)
}

// assertLegsUntouched checks a forced completion left the node task and its
// orders exactly as the changeover left them.
func assertLegsUntouched(t *testing.T, db *store.DB, task *processes.NodeTask) {
	t.Helper()
	got, err := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	testutil.MustNoErr(t, err, "get task")
	if got.State != task.State {
		t.Errorf("task state %s → %s; a forced completion leaves tasks alone", task.State, got.State)
	}
	for _, id := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if id == nil {
			continue
		}
		o, err := db.GetOrder(*id)
		testutil.MustNoErr(t, err, "get order")
		if protocol.IsTerminal(o.Status) {
			t.Errorf("order %d is %s; a forced completion leaves orders alone", o.ID, o.Status)
		}
	}
}

// TestAdminSwitch_ThirdStyleCompletesThenSwitches: "Mark as running" on a style
// that is neither the running one nor the target completes the open
// changeover to its target, then switches, and records both.
func TestAdminSwitch_ThirdStyleCompletesThenSwitches(t *testing.T) {
	t.Parallel()
	eng, db, processID, _, toID, coID, _ := openRedChangeover(t)
	thirdID, err := db.CreateStyle("THIRD", "", processID)
	testutil.MustNoErr(t, err, "create third style")

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &thirdID), "admin switch")
	first := changeoverRow(t, db, coID)
	if first.State != domain.ChangeoverCompleted || first.TriggeredBy != triggeredAdminSwitch+triggeredForced {
		t.Fatalf("open changeover = %s by %q, want completed by admin-switch-forced", first.State, first.TriggeredBy)
	}
	second := latestChangeover(t, db, processID)
	if second.ID == coID || second.State != domain.ChangeoverCompleted || second.TriggeredBy != triggeredAdminSwitch ||
		second.FromStyleID == nil || *second.FromStyleID != toID || second.ToStyleID != thirdID {
		t.Fatalf("second row = %+v, want completed admin-switch from %d to %d", second, toID, thirdID)
	}
	if p := mustProcess(t, db, processID); p.ActiveStyleID == nil || *p.ActiveStyleID != thirdID {
		t.Errorf("active style = %v, want %d", p.ActiveStyleID, thirdID)
	}
}

// latestChangeover is the process's newest changeover row, ended or not.
func latestChangeover(t *testing.T, db *store.DB, processID int64) *processes.Changeover {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM process_changeovers WHERE process_id = ? ORDER BY id DESC LIMIT 1`,
		processID).Scan(&id), "latest changeover")
	return changeoverRow(t, db, id)
}

// TestAdminSwitch_ActiveStyleIsANoOp: with a changeover open, "Mark as
// running" on the style already running changes nothing and says to cancel.
func TestAdminSwitch_ActiveStyleIsANoOp(t *testing.T) {
	t.Parallel()
	eng, db, processID, fromID, _, coID, _ := openRedChangeover(t)

	err := eng.SetProcessActiveStyle(processID, &fromID)
	var refused *SwitchRefused
	if !errors.As(err, &refused) || !strings.Contains(refused.Sentence, "use Cancel to stay on") {
		t.Fatalf("switch to the running style = %v, want a refusal naming Cancel", err)
	}
	if co := changeoverRow(t, db, coID); co.State.IsTerminal() {
		t.Errorf("the changeover ended: %s", co.State)
	}
	if p := mustProcess(t, db, processID); p.ActiveStyleID == nil || *p.ActiveStyleID != fromID {
		t.Errorf("active style = %v, want %d", p.ActiveStyleID, fromID)
	}
}

// TestAdminSwitch_NoChangeoverWritesARecord: with no changeover open the
// switch flips as it always did and leaves a completed row behind.
func TestAdminSwitch_NoChangeoverWritesARecord(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	processID, _, fromID, _ := seedProduceNode(t, db, "two_robot")
	toID, err := db.CreateStyle("SWITCHED", "", processID)
	testutil.MustNoErr(t, err, "create style")

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &toID), "admin switch")
	row := latestChangeover(t, db, processID)
	if row.State != domain.ChangeoverCompleted || row.TriggeredBy != triggeredAdminSwitch ||
		row.FromStyleID == nil || *row.FromStyleID != fromID || row.ToStyleID != toID ||
		row.CompletedAt == nil || !row.CompletedAt.Equal(row.StartedAt) {
		t.Fatalf("record = %+v, want a completed admin-switch row from %d to %d born completed", row, fromID, toID)
	}
	tasks, err := db.ListChangeoverNodeTasks(row.ID)
	testutil.MustNoErr(t, err, "list tasks")
	if len(tasks) != 0 {
		t.Errorf("the record has %d node tasks, want none", len(tasks))
	}
}

// TestForceDoor_NeverStarts: a forced completion opens no changeover and
// creates no order.
func TestForceDoor_NeverStarts(t *testing.T) {
	t.Parallel()
	eng, db, processID, _, toID, _, _ := openRedChangeover(t)
	before, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "list orders")

	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &toID), "admin switch")
	if co, err := db.GetActiveProcessChangeover(processID); err == nil {
		t.Errorf("a changeover is open after a forced completion: %+v", co)
	}
	after, err := db.ListActiveOrders()
	testutil.MustNoErr(t, err, "list orders")
	if len(after) != len(before) {
		t.Errorf("live orders %d → %d; a forced completion creates none and ends none", len(before), len(after))
	}
}

// TestForcedCutover_LeavesLegsRunning: a staging leg still moving when the
// changeover is forced complete is left alone, and its later landing is an
// ordinary delivery: the ended row is not rewritten and the task not advanced.
func TestForcedCutover_LeavesLegsRunning(t *testing.T) {
	t.Parallel()
	eng, db, processID, _, toID, coID, task := openRedChangeover(t)
	testutil.MustNoErr(t, eng.SetProcessActiveStyle(processID, &toID), "admin switch")
	assertLegsUntouched(t, db, task)
	ended := changeoverRow(t, db, coID)

	if task.NextMaterialOrderID != nil {
		o, err := db.GetOrder(*task.NextMaterialOrderID)
		testutil.MustNoErr(t, err, "get supply order")
		markOrderTerminal(db, o.ID)
		nodeID := task.ProcessNodeID
		emitOrderCompleted(eng, o.ID, o.UUID, o.OrderType, &nodeID)
	}
	got := changeoverRow(t, db, coID)
	if got.State != ended.State || got.TriggeredBy != ended.TriggeredBy {
		t.Errorf("the landing rewrote the ended row: %s by %q", got.State, got.TriggeredBy)
	}
	after, err := db.GetChangeoverNodeTaskByNode(task.ProcessChangeoverID, task.ProcessNodeID)
	testutil.MustNoErr(t, err, "get task")
	if after.State != task.State {
		t.Errorf("the landing advanced the ended changeover's task %s → %s", task.State, after.State)
	}
}

// TestForceDoor_ThreeCallers is the census of CutoverForced: every call site
// that passes it, by enclosing function. Three: the admin switch's two and the
// counted-part force's auto fire.
func TestForceDoor_ThreeCallers(t *testing.T) {
	t.Parallel()
	want := []string{
		"SetProcessActiveStyle", // to the open changeover's target
		"SetProcessActiveStyle", // to a third style, before the second flip
		"callDoor",              // the counted-part force, in auto (the fire, under the re-read)
	}
	var got []string
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	testutil.MustNoErr(t, err, "glob")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		testutil.MustNoErr(t, err, "read "+f)
		file, err := parser.ParseFile(fset, f, src, 0)
		testutil.MustNoErr(t, err, "parse "+f)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == "CutoverForced" {
						got = append(got, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CutoverForced callers = %v, want %v", got, want)
	}
}
