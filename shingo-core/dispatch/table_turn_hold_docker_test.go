//go:build docker

package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// table_turn_hold_docker_test.go — the table turn's hold on a swap's lift
// (PLAN §5 phase 3), against a real database. The fleet is the mock backend
// (its turner records each TurnTable) and the robot is a fake status reader
// the test turns by hand, which is what the robot poll hands the re-driver.

const (
	ttVehicle = "AMR-05"
	ttRad     = 1.5707963267948966 // 90°
)

// fakeTable is one robot as the status reader reports it.
type fakeTable struct {
	mu    sync.Mutex
	st    fleet.RobotStatus
	reads int
}

func (f *fakeTable) read(vehicle string) (fleet.RobotStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if vehicle != f.st.VehicleID {
		return fleet.RobotStatus{}, false
	}
	return f.st, true
}

// turn sets the table's angle and whether the robot is still running a task.
func (f *fakeTable) turn(rad float64, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.TableAngle, f.st.TaskStatus = rad, 4
	if running {
		f.st.TaskStatus = fleet.TaskStatusRunning
	}
}

func (f *fakeTable) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

type turnRig struct {
	releaseRig
	table  *fakeTable
	alarms *[]string
}

func newTurnRig(t *testing.T) turnRig {
	t.Helper()
	r := newReleaseRig(t)
	tbl := &fakeTable{st: fleet.RobotStatus{VehicleID: ttVehicle, Connected: true, TaskStatus: 4, JackState: 3}}
	r.d.SetTableTurner(r.backend, tbl.read)
	alarms := &[]string{}
	r.d.SetTableTurnAlarm(func(_ int64, node, _ string) { *alarms = append(*alarms, node) })
	return turnRig{releaseRig: r, table: tbl, alarms: alarms}
}

// turnAt sets a node to turn its table before the lift.
func (r turnRig) turnAt(t *testing.T, node string) {
	t.Helper()
	n, err := r.db.GetNodeByDotName(node)
	n = testutil.Must(t, n, err, "node "+node)
	testutil.MustNoErr(t, r.db.SetNodeProperty(n.ID, PropTableTurnRad, "1.5707963267948966"), "angle")
	testutil.MustNoErr(t, r.d.ReloadTableTurns(), "reload")
}

func (r turnRig) turns() int { return len(r.backend.TurnCalls()) }

// ttEvac is a two_robot swap's evac at its line: a station wait at the line,
// the lift, the drop on the outbound staging. A cart stands on the line at
// count 9, and the fleet holds the order on ttVehicle.
func ttEvac(t *testing.T, r turnRig, prefix, sibling string, status protocol.Status) (*orders.Order, *bins.Bin) {
	t.Helper()
	line, out := prNode(t, r.db, prefix+"-LINE"), prNode(t, r.db, prefix+"-OUT")
	cart := makeEmptyBin(t, r.db, line.ID, prefix+"-CART")
	testutil.MustNoErr(t, r.db.SetBinManifest(cart.ID, `{"items":[]}`, "PART-A", 9), "manifest")
	testutil.MustNoErr(t, r.db.ConfirmBinManifest(cart.ID, ""), "confirm")
	steps := []resolvedStep{voWait(line.Name, WaitKindStation), vsPick(line.Name), voDrop(out.Name, nil)}
	o := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = prefix+"-evac", "line-1", OrderTypeComplex, status
		o.ProcessNode, o.SiblingOrderUUID, o.Coordinated = line.Name, sibling, true
		o.StepsJSON = string(mustJSON(t, steps))
	})
	testutil.MustNoErr(t, r.db.UpdateOrderVendor(o.ID, "V-"+o.EdgeUUID, "WAITING", ""), "vendor")
	r.backend.SetOrderVehicle("V-"+o.EdgeUUID, ttVehicle)
	return prReload(t, r.db, o.ID), cart
}

// press is one RELEASE with a count, as the station sends it.
func (r turnRig) press(t *testing.T, o *orders.Order, count int) string {
	t.Helper()
	before, errs := r.appends(), len(orderErrorCodes(t, r.db, o.EdgeUUID))
	r.d.HandleOrderRelease(r.d.syntheticEnvelope("line-1"), &protocol.OrderRelease{OrderUUID: o.EdgeUUID, RemainingUOP: &count})
	fresh := prReload(t, r.db, o.ID)
	return fmt.Sprintf("appends=%d wait_index=%d status=%s errors=[%s]", r.appends()-before, fresh.WaitIndex,
		fresh.Status, strings.Join(orderErrorCodes(t, r.db, o.EdgeUUID)[errs:], ","))
}

// forceTurnResend clears a wait's last send time, as if turnResendAfter had
// passed: the test's stand-in for waiting 30 s.
func (d *Dispatcher) forceTurnResend(orderID int64, waitIndex int) {
	d.turnMu.Lock()
	defer d.turnMu.Unlock()
	if s := d.turnSent[turnKey{orderID: orderID, waitIndex: waitIndex}]; s != nil {
		s.last = time.Time{}
	}
}

// stagedKinds is every OrderStaged wait kind Core sent the station for uuid.
func stagedKinds(t *testing.T, r turnRig, uuid string) []string {
	t.Helper()
	msgs, err := r.db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "outbox")
	var kinds []string
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderStaged {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "envelope")
		var p protocol.OrderStaged
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &p), "order.staged")
		if p.OrderUUID == uuid {
			kinds = append(kinds, p.WaitKind)
		}
	}
	return kinds
}

func binCount(t *testing.T, r turnRig, id int64) int {
	t.Helper()
	b, err := r.db.GetBin(id)
	return testutil.Must(t, b, err, "bin").UOPRemaining
}

// lastAppendIsTheLift asserts the last append is the segment a swap's release
// has always sent: JackLoad at the line, JackUnload at the outbound staging.
func lastAppendIsTheLift(t *testing.T, r turnRig, prefix string) {
	t.Helper()
	calls := r.backend.ReleaseCalls()
	if len(calls) == 0 {
		t.Fatal("nothing was appended")
	}
	b := calls[len(calls)-1].Blocks
	if len(b) != 2 || b[0].Location != prefix+"-LINE" || b[0].BinTask != "JackLoad" ||
		b[1].Location != prefix+"-OUT" || b[1].BinTask != "JackUnload" {
		t.Fatalf("appended %+v, want [JackLoad@%s-LINE, JackUnload@%s-OUT] as today", b, prefix, prefix)
	}
}

// TestTableTurn_SwapLift_TurnedThenPressed: the evac stages, one turn is sent,
// and a press after the table reads the angle appends the lift exactly as a
// release always has.
func TestTableTurn_SwapLift_TurnedThenPressed(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT1", "", StatusStaged)
	r.turnAt(t, "TT1-LINE")

	r.d.RedriveTableTurns(o.ID) // the staged hook
	if got := r.backend.TurnCalls(); len(got) != 1 || got[0].Vehicle != ttVehicle || got[0].Rad != ttRad {
		t.Fatalf("on arrival: turns %+v, want one to %s at %.4f", got, ttVehicle, ttRad)
	}
	r.d.RedriveTableTurns(o.ID) // a second arrival read inside 30 s sends nothing
	if n := r.turns(); n != 1 {
		t.Fatalf("%d turns, want the one", n)
	}
	r.table.turn(1.569, false)
	if got := r.press(t, o, 4); got != "appends=1 wait_index=1 status=in_transit errors=[]" {
		t.Fatalf("a press after the turn: %s, want it to go at once", got)
	}
	lastAppendIsTheLift(t, r, "TT1")
}

// TestTableTurn_PressDuringTheTurn: the press is taken (the count is written
// now), the station is told Core holds the leg, a second press is refused by
// the ownership fence and writes nothing, and the lift follows the table with
// nobody pressing again.
func TestTableTurn_PressDuringTheTurn(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, cart := ttEvac(t, r, "TT2", "", StatusStaged)
	r.turnAt(t, "TT2-LINE")
	r.d.RedriveTableTurns(o.ID)
	r.table.turn(0.6, true) // mid-turn

	if got := r.press(t, o, 4); got != "appends=0 wait_index=0 status=staged errors=[]" {
		t.Fatalf("a press mid-turn: %s, want taken and held", got)
	}
	if n := binCount(t, r, cart.ID); n != 4 {
		t.Errorf("the cart reads %d after the press, want the station's 4", n)
	}
	held := prReload(t, r.db, o.ID)
	if !CoreOwnsWaitAt(held.StepsJSON, held.WaitIndex) {
		t.Fatal("a held leg is not Core's: the abandon sweep would not exempt it, and a second press would go")
	}
	if kinds := stagedKinds(t, r, o.EdgeUUID); len(kinds) == 0 || kinds[len(kinds)-1] != WaitKindTurn {
		t.Errorf("the station was told %v, want the last to be %q", kinds, WaitKindTurn)
	}
	if QueueCause(held.QueueCause) != CauseTableTurning {
		t.Errorf("the row says %q, want %q", held.QueueCause, CauseTableTurning)
	}
	if got := r.press(t, o, 2); got != "appends=0 wait_index=0 status=staged errors=[invalid_state]" {
		t.Errorf("a second press while held: %s, want refused", got)
	}
	if n := binCount(t, r, cart.ID); n != 4 {
		t.Errorf("the cart reads %d after the second press, want 4: the count is written once", n)
	}

	if got := r.d.RedriveTableTurns(0); len(got) != 0 {
		t.Fatalf("appended %v with the turn still running", got)
	}
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 || got[0] != o.ID {
		t.Fatalf("on angle: appended %v, want [%d]", got, o.ID)
	}
	lastAppendIsTheLift(t, r, "TT2")
	if n := binCount(t, r, cart.ID); n != 4 {
		t.Errorf("the cart reads %d after the lift, want 4: the re-driver writes no count", n)
	}
	if r.d.TableTurnPending() {
		t.Error("still pending after the lift went: the robot poll would keep asking")
	}
}

// TestTableTurn_PressWhileDriving: a press while the robot is still driving to
// the wait is held, the arrival sends the turn, and the lift follows it.
func TestTableTurn_PressWhileDriving(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT3", "", StatusInTransit)
	r.turnAt(t, "TT3-LINE")
	r.table.turn(ttRad, false) // even a table that happens to read the angle: the robot is not there

	if got := r.press(t, o, 4); got != "appends=0 wait_index=0 status=in_transit errors=[]" {
		t.Fatalf("a press while driving: %s, want taken and held", got)
	}
	r.table.turn(0, false)
	testutil.MustNoErr(t, r.db.UpdateOrderStatus(o.ID, string(StatusStaged), "test: arrived"), "arrive")
	r.d.RedriveTableTurns(o.ID) // the staged hook
	if n := r.turns(); n != 1 {
		t.Fatalf("%d turns on arrival, want 1", n)
	}
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("on angle: appended %v, want the held lift", got)
	}
	lastAppendIsTheLift(t, r, "TT3")
}

// TestTableTurn_PartnerWaitsForTheLift: a supply that drops on the line is not
// released with the evac while the evac's turn is owed; once the table reads
// the angle it is.
func TestTableTurn_PartnerWaitsForTheLift(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	evac, _ := ttEvac(t, r, "TT4", "TT4-supply", StatusStaged)
	r.turnAt(t, "TT4-LINE")
	stage := prNode(t, r.db, "TT4-STAGE")
	supply := vsStagedLeg(t, r.db, "TT4-supply", evac.EdgeUUID, "TT4-LINE", StatusStaged, []resolvedStep{
		voWait(stage.Name, WaitKindStation), vsPick(stage.Name), voDrop("TT4-LINE", nil)})

	coRelease := func() bool {
		pts := r.d.ReleasePoints("line-1", []string{supply.EdgeUUID})
		if len(pts) != 1 || len(pts[0].AwaitsLift) != 1 {
			t.Fatalf("release points %+v, want the supply to await the evac's lift", pts)
		}
		return pts[0].AwaitsLift[0].CoRelease
	}
	if coRelease() {
		t.Error("the supply is co-released while the evac's table is not turned: it would drive onto a line whose cart has not left")
	}
	r.table.turn(1.569, false)
	if !coRelease() {
		t.Error("the supply is not co-released once the evac's table reads the angle")
	}
}

// TestTableTurn_TableNeverTurns: three turns, the failure on the row, the
// alarm once, nothing appended; a cancel then sends nothing more.
func TestTableTurn_TableNeverTurns(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT5", "", StatusStaged)
	r.turnAt(t, "TT5-LINE")
	r.d.RedriveTableTurns(o.ID)
	r.table.turn(0.6, true)
	if got := r.press(t, o, 4); !strings.HasPrefix(got, "appends=0") {
		t.Fatalf("press: %s", got)
	}
	r.table.turn(0.02, false) // the robot stopped short and stays there
	for i := 0; i < 6; i++ {
		r.d.forceTurnResend(o.ID, 0)
		r.d.RedriveTableTurns(0)
	}
	if n := r.turns(); n != turnMaxSends {
		t.Errorf("%d turns, want %d and no more", n, turnMaxSends)
	}
	if n := len(*r.alarms); n != 1 || (*r.alarms)[0] != "TT5-LINE" {
		t.Errorf("alarms %v, want one naming TT5-LINE", *r.alarms)
	}
	fresh := prReload(t, r.db, o.ID)
	if QueueCause(fresh.QueueCause) != CauseTableTurnFailed || !strings.Contains(fresh.QueueReason, "reads 1°") {
		t.Errorf("the row says %q %q, want the failure with the angle read", fresh.QueueCause, fresh.QueueReason)
	}
	if r.appends() != 0 {
		t.Errorf("%d appends, want none", r.appends())
	}
	if _, err := r.db.TerminalizeOrder(o.ID, StatusCancelled, "test: cancelled"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	r.d.forceTurnResend(o.ID, 0)
	r.d.RedriveTableTurns(0)
	r.d.RedriveTableTurns(o.ID)
	if n := r.turns(); n != turnMaxSends || r.appends() != 0 {
		t.Errorf("after the cancel: %d turns and %d appends, want %d and 0", n, r.appends(), turnMaxSends)
	}
}

// TestTableTurn_RestartWhileHeld: a fresh dispatcher on the same database
// (Core restarted, memory gone) finds the held order on its floor and appends
// it once the table reads the angle — once.
func TestTableTurn_RestartWhileHeld(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT6", "", StatusStaged)
	r.turnAt(t, "TT6-LINE")
	r.table.turn(0.3, true)
	if got := r.press(t, o, 4); !strings.HasPrefix(got, "appends=0") {
		t.Fatalf("press: %s", got)
	}
	d2, _ := newTestDispatcher(t, r.db, r.backend)
	d2.SetTableTurner(r.backend, r.table.read)
	if d2.TableTurnPending() {
		t.Fatal("a fresh dispatcher remembers a pending turn — the test does not model a restart")
	}
	r.table.turn(1.569, false)
	d2.SweepLaneWaiters()
	d2.SweepLaneWaiters()
	if n := r.appends(); n != 1 {
		t.Fatalf("%d appends after the restart's floor, want 1", n)
	}
	lastAppendIsTheLift(t, r, "TT6")
}

// TestTableTurn_AngleClearedWhileHeld: the turn is moot and the held lift goes.
func TestTableTurn_AngleClearedWhileHeld(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT7", "", StatusStaged)
	r.turnAt(t, "TT7-LINE")
	r.table.turn(0.3, true)
	if got := r.press(t, o, 4); !strings.HasPrefix(got, "appends=0") {
		t.Fatalf("press: %s", got)
	}
	line, err := r.db.GetNodeByDotName("TT7-LINE")
	line = testutil.Must(t, line, err, "line")
	testutil.MustNoErr(t, r.db.DeleteNodeProperty(line.ID, PropTableTurnRad), "clear")
	testutil.MustNoErr(t, r.d.ReloadTableTurns(), "reload")
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("after clearing the angle: appended %v, want the held lift", got)
	}
}

// TestTableTurn_HardReleaseOffAngle: the engineer's override appends with the
// table off its angle, and the audit line says what was read and wanted.
func TestTableTurn_HardReleaseOffAngle(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT8", "", StatusStaged)
	r.turnAt(t, "TT8-LINE")
	r.table.turn(0.3, true)
	if got := r.press(t, o, 4); !strings.HasPrefix(got, "appends=0") {
		t.Fatalf("press: %s", got)
	}
	r.table.turn(0.05, false)
	testutil.MustNoErr(t, r.d.HardReleaseStagedOrder(o.ID, "engineer"), "hard release")
	lastAppendIsTheLift(t, r, "TT8")
	rows, err := r.db.ListEntityAudit("order", o.ID)
	testutil.MustNoErr(t, err, "audit")
	found := false
	for _, a := range rows {
		if a.Action == "hard_release" && strings.Contains(a.NewValue, "TABLE TURN BYPASSED at TT8-LINE") &&
			strings.Contains(a.NewValue, "reads 2.9°, wanted 90.0°") {
			found = true
		}
	}
	if !found {
		t.Errorf("no hard_release audit row names the bypassed turn with its angles: %+v", rows)
	}
}

// TestTableTurn_HeldForBothGoesWhenBoth: a leg owed a vacate hold and a turn is
// taken once and goes only when both are met, whichever comes last.
func TestTableTurn_HeldForBothGoesWhenBoth(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	line, out := prNode(t, r.db, "TT9-LINE"), prNode(t, r.db, "TT9-B1")
	partner := testdb.CreateOrder(t, r.db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.Status = "TT9-lifter", "line-1", StatusInTransit
	})
	resident := makeEmptyBin(t, r.db, out.ID, "TT9-RESIDENT")
	stamped := time.Now().UTC().Add(-time.Minute)
	steps := []resolvedStep{voWait(line.Name, WaitKindStation), vsPick(line.Name),
		voDrop(out.Name, &vacateStamp{Node: out.Name, Bin: resident.ID, Partner: partner.ID, StampedAt: &stamped})}
	o := vsStagedLeg(t, r.db, "TT9-evac", "", out.Name, StatusStaged, steps)
	r.backend.SetOrderVehicle(o.VendorOrderID, ttVehicle)
	r.turnAt(t, "TT9-LINE")

	if got := r.press(t, o, 0); got != "appends=0 wait_index=0 status=staged errors=[]" {
		t.Fatalf("press: %s, want taken for both", got)
	}
	held := prReload(t, r.db, o.ID)
	hs, ok := decodeSteps(held.StepsJSON)
	if !ok || hs[0].TurnHeldAt == nil || hs[2].Vacate == nil || hs[2].Vacate.HeldAt == nil {
		t.Fatalf("the take wrote %+v, want both marks", hs)
	}
	vhLift(t, r.db, resident.ID)
	if got := r.d.RedriveVacateWaits(resident.ID); len(got) != 0 {
		t.Fatalf("the vacate re-driver appended %v with the table not turned", got)
	}
	r.table.turn(1.569, false)
	if got := r.d.RedriveTableTurns(0); len(got) != 1 {
		t.Fatalf("both met: appended %v, want the lift", got)
	}
	if r.appends() != 1 {
		t.Errorf("%d appends, want 1", r.appends())
	}
}

// TestTableTurn_NoAngleNodeIsUntouched: with an unrelated node carrying an
// angle, a swap at a node with none reads no robot, turns nothing, and appends
// the same blocks as a dispatcher that knows nothing of turns.
func TestTableTurn_NoAngleNodeIsUntouched(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	prNode(t, r.db, "TT10-ELSEWHERE")
	r.turnAt(t, "TT10-ELSEWHERE")
	o, _ := ttEvac(t, r, "TT10", "TT10-supply", StatusStaged)
	stage := prNode(t, r.db, "TT10-STAGE")
	supply := vsStagedLeg(t, r.db, "TT10-supply", o.EdgeUUID, "TT10-LINE", StatusStaged, []resolvedStep{
		voWait(stage.Name, WaitKindStation), vsPick(stage.Name), voDrop("TT10-LINE", nil)})
	pts := r.d.ReleasePoints("line-1", []string{supply.EdgeUUID})
	r.d.RedriveTableTurns(o.ID)
	if got := r.press(t, o, 4); got != "appends=1 wait_index=1 status=in_transit errors=[]" {
		t.Fatalf("a press at a node with no angle: %s", got)
	}
	if r.turns() != 0 || r.table.readCount() != 0 {
		t.Errorf("%d turns and %d robot reads at a node with no angle, want none", r.turns(), r.table.readCount())
	}
	lastAppendIsTheLift(t, r, "TT10")
	if len(pts) != 1 || len(pts[0].AwaitsLift) != 1 || !pts[0].AwaitsLift[0].CoRelease {
		t.Errorf("release points %+v, want the supply co-released with the evac, as today", pts)
	}
}

// TestTableTurn_AppendDoorRefusesOffAngle: the backstop. An append that would
// lift at a turn node with the table off its angle goes nowhere and leaves the
// order where it was, whoever calls; on angle it goes; the hard release's
// bypass is the one way past it.
func TestTableTurn_AppendDoorRefusesOffAngle(t *testing.T) {
	t.Parallel()
	r := newTurnRig(t)
	o, _ := ttEvac(t, r, "TT11", "", StatusStaged)
	r.turnAt(t, "TT11-LINE")
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatal("fixture plan does not decode")
	}
	seg, more, off := splitSegment(steps, 0)
	if err := r.d.appendSegmentAndAdvance(o, seg, more, off, "probe"); !errors.Is(err, ErrTurnNotDone) {
		t.Fatalf("an off-angle lift at a turn node: %v, want ErrTurnNotDone", err)
	}
	if fresh := prReload(t, r.db, o.ID); r.appends() != 0 || fresh.WaitIndex != 0 || fresh.Status != StatusStaged {
		t.Fatalf("the refused append moved something: %d appends, wait_index %d, %s", r.appends(), fresh.WaitIndex, fresh.Status)
	}
	if err := r.d.appendSegment(o, seg, more, off, "probe bypass", true); err != nil {
		t.Fatalf("the bypass was refused: %v", err)
	}
	if r.appends() != 1 {
		t.Errorf("%d appends through the bypass, want 1", r.appends())
	}

	o2, _ := ttEvac(t, r, "TT12", "", StatusStaged)
	r.turnAt(t, "TT12-LINE")
	r.table.turn(1.569, false)
	steps2, ok := decodeSteps(o2.StepsJSON)
	if !ok {
		t.Fatal("fixture plan does not decode")
	}
	seg2, more2, off2 := splitSegment(steps2, 0)
	if err := r.d.appendSegmentAndAdvance(o2, seg2, more2, off2, "probe on angle"); err != nil {
		t.Errorf("an on-angle lift was refused: %v", err)
	}
}
