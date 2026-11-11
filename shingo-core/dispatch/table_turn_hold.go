package dispatch

import (
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/fleet"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

// table_turn_hold.go — CORE HOLDS A LIFT UNTIL THE ROBOT'S TABLE IS TURNED.
//
// ── WHERE THE PAUSE COMES FROM ──────────────────────────────────────────────
//
// A turn needs the robot parked under the cart at the node with the jack down,
// and recognize, drive-under and lift run as one robot task with no gap to
// turn in. So the turn happens at a WAIT at the node, and the lift is the
// segment after it:
//
//   - a swap's lift already has one: the station wait at the line. The swap
//     does not change. Core sends the turn when the robot arrives (the staged
//     hook), and the operator presses once. If the table is not ready when the
//     press comes, Core TAKES the release (the count is written now) and marks
//     the wait held (its TurnHeldAt, plan/legs.go), the same way the vacate hold takes
//     one (holdRelease), and appends the lift when the table reads the angle;
//   - a lift with no wait in front gets a turn wait Core inserts (the plan's
//     phase 5), released by the same re-driver with nobody pressing.
//
// ── WHEN A TURN APPLIES (turnApplies) ───────────────────────────────────────
//
// The order is parked at (or driving to) a station or turn wait at node X, and
// the segment after that wait opens with pickup(X) on a node set to turn before
// the lift, or dropoff(X) on one set to turn before the set-down. A node with
// no angle answers from the in-memory set (table_turn.go): no query, no robot
// read.
//
// ── THE RE-DRIVER (redriveTurnOrder, PLAN §4) ───────────────────────────────
//
// It reads the robot and either sends the turn or appends the lift; it never
// re-enters the release handler, so the operator's count is written once. The
// send count and last send time live in memory: a restart forgets them, which
// costs at most three more ABSOLUTE sends, each of which turns the table once.
//
// ── WHAT DRIVES IT ──────────────────────────────────────────────────────────
//
//   - the robot's arrival at the wait (the staged hook): one send, never a wait;
//   - a robot status change (the 2 s poll, behind its unchanged-hash test), only
//     while a turn is pending (TableTurnPending);
//   - the 60 s floor (SweepLaneWaiters), which re-reads the population from the
//     plans and is the restart hook.
//
// Nothing here waits: no sleep, no poll loop, and every fleet call is the
// backend's own short-timeout call.

// The re-driver's bounds (PLAN §4).
const (
	// turnMaxSends is how many turns Core sends at one wait before it stops
	// and raises an alarm.
	turnMaxSends = 3
	// turnResendAfter is the least time between two sends at one wait: a 90°
	// turn takes about 8 s (measured 2026-11-10), so a send this old that left
	// the table off its angle did not take.
	turnResendAfter = 30 * time.Second
)

// ErrTurnNotDone is an append refused at the door because the lift (or the
// set-down) it would send is at a turn node and the table does not read the
// angle. Nothing went to the fleet; the turn's re-driver appends it later.
var ErrTurnNotDone = errors.New("the robot's table has not been turned at this node")

// turnKey names one wait of one order: a plan can turn at two nodes, and the
// sends at one are not the other's.
type turnKey struct {
	orderID   int64
	waitIndex int
}

// turnSends is the in-memory record of the turns sent at one wait.
type turnSends struct {
	count   int
	last    time.Time
	alarmed bool
}

// turnApplies is the turn the segment after wait waitIndex owes, from the
// in-memory set of turn nodes: the node, its turn, and true when it applies.
func (d *Dispatcher) turnApplies(steps []resolvedStep, waitIndex int) (string, turnNode, bool) {
	return turnAppliesWith(steps, waitIndex, d.turnAt)
}

// turnAppliesWith is turnApplies over a given lookup, pure.
func turnAppliesWith(steps []resolvedStep, waitIndex int, at func(string) (turnNode, bool)) (string, turnNode, bool) {
	w, ok := waitAt(steps, waitIndex)
	if !ok || w.Node == "" {
		return "", turnNode{}, false
	}
	if !protocol.IsStationWaitKind(w.WaitKind) && w.WaitKind != WaitKindTurn {
		return "", turnNode{}, false // a lane's or a vacate wait: not where a turn is made
	}
	t, ok := at(w.Node)
	if !ok {
		return "", turnNode{}, false
	}
	seg, _, _ := splitSegment(steps, waitIndex)
	if len(seg) == 0 || seg[0].Node != w.Node {
		return "", turnNode{}, false
	}
	switch {
	case t.side == TableTurnSideSetDown && seg[0].Action == protocol.ActionDropoff:
	case t.side != TableTurnSideSetDown && seg[0].Action == protocol.ActionPickup:
	default:
		return "", turnNode{}, false
	}
	return w.Node, t, true
}

// tableReading is one read of the robot an order is on, for its turn.
type tableReading struct {
	vehicle string
	status  fleet.RobotStatus
	// ok is false when no fresh status could be read (no turner, the fleet named
	// no vehicle, the read failed, the poll is stale); why says which.
	ok  bool
	why string
}

// readTable reads the robot the fleet holds the order on, at the moment of
// asking: the vehicle from the fleet (fleet.TableTurner.OrderVehicle), its
// status from the poll Core already makes.
func (d *Dispatcher) readTable(o *orders.Order) tableReading {
	turner, read := d.tableTurner()
	if turner == nil || read == nil {
		return tableReading{why: "this fleet cannot turn a table"}
	}
	vehicle, err := turner.OrderVehicle(o.VendorOrderID)
	if err != nil {
		return tableReading{why: fmt.Sprintf("the fleet did not say which robot holds the order (%v)", err)}
	}
	if vehicle == "" {
		return tableReading{why: "the fleet names no robot on the order"}
	}
	st, fresh := read(vehicle)
	if !fresh {
		return tableReading{vehicle: vehicle, why: "no fresh status for " + vehicle}
	}
	return tableReading{vehicle: vehicle, status: st, ok: true}
}

// turnPrecondition is why a robot cannot be turned now, "" when it can: not
// e-stopped, its control with the fleet manager, and its jack down for a lift
// or up and loaded for a set-down.
func turnPrecondition(r tableReading, side string) string {
	st := r.status
	switch {
	case st.Emergency:
		return r.vehicle + " is e-stopped"
	case st.Undispatchable.Unlock != 0:
		if st.Undispatchable.LockHolder != "" {
			return fmt.Sprintf("%s is controlled by %s", r.vehicle, st.Undispatchable.LockHolder)
		}
		return r.vehicle + " is not controlled by the fleet manager"
	case side == TableTurnSideSetDown && !(st.JackState == 1 && st.JackIsFull):
		return r.vehicle + "'s jack is not up and loaded"
	case side != TableTurnSideSetDown && st.JackState != 3:
		return r.vehicle + "'s jack is not down"
	}
	return ""
}

// turnStillOwed reports whether the segment after the order's wait owes a turn
// the table has not made: the turn applies and the robot does not read on
// angle, or cannot be read (unknown is not done).
func (d *Dispatcher) turnStillOwed(o *orders.Order, steps []resolvedStep) bool {
	_, t, applies := d.turnApplies(steps, o.WaitIndex)
	if !applies {
		return false
	}
	if o.Status != StatusStaged {
		return true // not yet parked at the wait: nothing has turned
	}
	r := d.readTable(o)
	return !r.ok || !fleet.TableOnAngle(r.status, t.rad)
}

// turnHoldAtRelease is whether a station release of the order must be taken
// and held for the table: the turn is owed.
func (d *Dispatcher) turnHoldAtRelease(o *orders.Order, steps []resolvedStep) bool {
	return d.turnStillOwed(o, steps)
}

// turnBackstop is the append door's refusal (appendSegmentAndAdvance): a lift
// or set-down at a turn node with the table off its angle goes nowhere. Every
// releaser checks before it appends, so reaching the refusal is a defect: it is
// logged and audited on the order ("table_turn_refused"), so a plant can find
// one. The hard release passes the door with the angle in its audit line
// instead.
func (d *Dispatcher) turnBackstop(order *orders.Order, what string) error {
	if !d.hasTurnNodes() {
		return nil // no node turns: every append is exactly as it was
	}
	steps, ok := decodeSteps(order.StepsJSON)
	if !ok {
		return nil
	}
	node, t, applies := d.turnApplies(steps, order.WaitIndex)
	if !applies || !d.turnStillOwed(order, steps) {
		return nil
	}
	detail := fmt.Sprintf("%s would %s at %s with the table not on %.0f° (%s) — refused at the append; the table "+
		"turn re-drives it", what, sideWords(t.side), node, degrees(t.rad), d.describeTable(order, t))
	log.Printf("dispatch: DEFECT: order %d: %s", order.ID, detail)
	d.db.AppendAudit("order", order.ID, "table_turn_refused", "", detail, "system")
	d.markTurnPending()
	return fmt.Errorf("%s: order %d: %w", what, order.ID, ErrTurnNotDone)
}

// describeTable is a robot's table in words, for a log line or an audit row.
func (d *Dispatcher) describeTable(o *orders.Order, t turnNode) string {
	r := d.readTable(o)
	if !r.ok {
		return "table unread: " + r.why
	}
	running := ""
	if r.status.TaskStatus == fleet.TaskStatusRunning {
		running = ", a task running"
	}
	return fmt.Sprintf("%s's table reads %.1f°, wanted %.1f°%s", r.vehicle, degrees(r.status.TableAngle), degrees(t.rad), running)
}

// degrees is radians in degrees.
func degrees(rad float64) float64 { return rad * 180 / math.Pi }

// RedriveTableTurns is the table turn's re-driver (PopTurnHeld). orderID names
// one order (the staged hook); 0 re-asks every order in the population (a
// robot status change, the floor). Returns the orders it appended.
//
// Statements: with no turn node and nothing pending, the staged hook asks
// nothing. Otherwise one order read (an order) or one population read (0),
// and per order the re-driver's reads: the fleet's vehicle, the polled status
// (no statement), a re-read before a send, the append path.
func (d *Dispatcher) RedriveTableTurns(orderID int64) []int64 {
	if orderID != 0 {
		if !d.hasTurnNodes() && !d.TableTurnPending() {
			return nil
		}
		o, err := d.db.GetOrder(orderID)
		if err != nil || o == nil {
			return nil
		}
		if appended, _ := d.redriveTurnOrder(o); appended {
			return []int64{o.ID}
		}
		return nil
	}
	cands, err := d.db.TurnWaitCandidates()
	if err != nil {
		log.Printf("dispatch: table turn: could not read the held set: %v (the floor retries)", err)
		return nil
	}
	var released []int64
	pending := false
	for _, o := range cands {
		appended, owes := d.redriveTurnOrder(o)
		if appended {
			released = append(released, o.ID)
		}
		pending = pending || owes
	}
	d.turnPending.Store(pending)
	return released
}

// MayOweATableTurn reports, from an order in hand and with no statement,
// whether it can owe a table turn: its plan carries a turn wait or a turn's
// held mark, or names a node that turns the table. The staged hook asks it
// before RedriveTableTurns, so a robot parking at a node with no angle costs no
// query even while another node has one (the plan's rule 2).
func (d *Dispatcher) MayOweATableTurn(o *orders.Order) bool {
	if o == nil {
		return false
	}
	if strings.Contains(o.StepsJSON, `"wait_kind":"turn"`) || strings.Contains(o.StepsJSON, `"turn_held_at":`) {
		return true
	}
	if !d.hasTurnNodes() {
		return false
	}
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		return false
	}
	for _, s := range steps {
		if _, ok := d.turnAt(s.Node); ok {
			return true
		}
	}
	return false
}

// TableTurnPending reports whether an order may be waiting on a table turn, so
// the robot poll's hook can return at once when none is. Set when a turn is
// held or sent, cleared by a full pass that finds none owed.
func (d *Dispatcher) TableTurnPending() bool { return d.turnPending.Load() }

// markTurnPending says a turn may be waiting.
func (d *Dispatcher) markTurnPending() { d.turnPending.Store(true) }

// redriveTurnOrder is §4 for one order. appended is true when it sent the
// lift; owes is true while the order still waits on a turn here.
func (d *Dispatcher) redriveTurnOrder(o *orders.Order) (appended, owes bool) {
	key := turnKey{orderID: o.ID, waitIndex: o.WaitIndex}
	if protocol.IsTerminal(o.Status) {
		d.forgetTurn(key)
		return false, false
	}
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		return false, false
	}
	held := turnHeldOrTurnWait(steps, o.WaitIndex)
	node, t, applies := d.turnApplies(steps, o.WaitIndex)
	if !applies {
		d.forgetTurn(key)
		if held && o.Status == StatusStaged {
			// THE TURN IS MOOT: the node's angle was cleared while the order was
			// held, or the segment no longer opens with the lift there. Nothing
			// else would append it.
			return d.appendTurned(o, steps, "table turn moot"), false
		}
		return false, false
	}
	if o.Status != StatusStaged {
		return false, held // driving to the wait; the arrival re-drives it
	}
	r := d.readTable(o)
	if !r.ok {
		return false, held // the next trigger retries
	}
	if why := turnPrecondition(r, t.side); why != "" {
		if held {
			d.setQueueReason(o, protocol.QueueWaitingForSlot, CauseTableTurnBlocked,
				QueueParams{TableNode: node, TableWantDeg: degrees(t.rad), TableBlocked: why})
		}
		return false, held
	}
	if r.status.TaskStatus == fleet.TaskStatusRunning {
		return false, held // a turn (or another task) is running
	}
	if fleet.TableOnAngle(r.status, t.rad) {
		if !held {
			return false, false // a station wait not yet pressed: the press goes straight through
		}
		if !d.vacateStampsMet(o, steps) {
			return false, true // the vacate hold appends once its bin has left
		}
		d.forgetTurn(key)
		return d.appendTurned(o, steps, "table turn release"), false
	}
	d.sendTurn(o, key, node, t, held, r)
	return false, held
}

// sendTurn is §4's off-angle arms: send the turn if fewer than turnMaxSends
// have gone and none in the last turnResendAfter, re-reading the order and the
// robot just before; after the last one, the failure cause and the alarm.
func (d *Dispatcher) sendTurn(o *orders.Order, key turnKey, node string, t turnNode, held bool, r tableReading) {
	now := clock.Now()
	d.turnMu.Lock()
	s := d.turnSent[key]
	if s == nil {
		s = &turnSends{}
		d.turnSent[key] = s
	}
	count, last, alarmed := s.count, s.last, s.alarmed
	d.turnMu.Unlock()

	if count >= turnMaxSends {
		if !alarmed {
			d.turnFailed(o, key, node, t, r)
		}
		return
	}
	if !last.IsZero() && now.Sub(last) < turnResendAfter {
		return
	}
	// A TURN CAN RACE A CANCEL: re-read the order and the robot immediately
	// before the send.
	fresh, err := d.db.GetOrder(o.ID)
	if err != nil || fresh == nil || fresh.Status != StatusStaged || fresh.WaitIndex != o.WaitIndex {
		return
	}
	turner, read := d.tableTurner()
	if turner == nil || read == nil {
		return
	}
	st, ok := read(r.vehicle)
	if !ok || st.TaskStatus == fleet.TaskStatusRunning || fleet.TableOnAngle(st, t.rad) {
		return
	}
	err = turner.TurnTable(r.vehicle, t.rad)
	d.turnMu.Lock()
	s.count, s.last = count+1, now
	d.turnMu.Unlock()
	d.markTurnPending()
	if err != nil {
		log.Printf("dispatch: table turn: order %d at %s: turning %s's table to %.1f° (send %d of %d): %v",
			o.ID, node, r.vehicle, degrees(t.rad), count+1, turnMaxSends, err)
	} else {
		log.Printf("dispatch: table turn: order %d at %s: turning %s's table to %.1f° (send %d of %d)",
			o.ID, node, r.vehicle, degrees(t.rad), count+1, turnMaxSends)
	}
	if held {
		d.setQueueReason(o, protocol.QueueWaitingForSlot, CauseTableTurning,
			QueueParams{TableNode: node, TableWantDeg: degrees(t.rad)})
	}
}

// turnFailed writes the failure on the row and raises the alarm, once.
func (d *Dispatcher) turnFailed(o *orders.Order, key turnKey, node string, t turnNode, r tableReading) {
	reads := degrees(r.status.TableAngle)
	d.setQueueReason(o, protocol.QueueWaitingForSlot, CauseTableTurnFailed,
		QueueParams{TableNode: node, TableWantDeg: degrees(t.rad), TableReadsDeg: &reads, TableFailed: true})
	detail := fmt.Sprintf("%d turns sent to %s at %s and its table reads %.1f°, wanted %.1f° — Core stopped "+
		"sending; the order is held until someone hard-releases it, cancels it or clears the node's angle",
		turnMaxSends, r.vehicle, node, reads, degrees(t.rad))
	log.Printf("dispatch: TABLE TURN FAILED: order %d: %s", o.ID, detail)
	d.db.AppendAudit("order", o.ID, "table_turn_failed", "", detail, "system")
	d.turnMu.Lock()
	if s := d.turnSent[key]; s != nil {
		s.alarmed = true
	}
	alarm := d.turnAlarm
	d.turnMu.Unlock()
	if alarm != nil {
		alarm(o.ID, node, detail)
	}
}

// SetTableTurnAlarm wires what a failed turn raises beyond the row and the
// audit: the engine's notifier. Nil raises nothing more.
func (d *Dispatcher) SetTableTurnAlarm(fn func(orderID int64, node, detail string)) {
	d.turnMu.Lock()
	d.turnAlarm = fn
	d.turnMu.Unlock()
}

// forgetTurn drops a wait's send record.
func (d *Dispatcher) forgetTurn(key turnKey) {
	d.turnMu.Lock()
	delete(d.turnSent, key)
	d.turnMu.Unlock()
}

// vacateStampsMet reports whether every vacate stamp the segment after the
// order's wait must see met has been: its bin lifted off its node since the
// stamp. The shared half of "may this segment go"; the turn's half is
// turnStillOwed. An unread answer is not met.
func (d *Dispatcher) vacateStampsMet(o *orders.Order, steps []resolvedStep) bool {
	w, _ := waitAt(steps, o.WaitIndex)
	stamps := vacateStampsAt(steps, o.WaitIndex, w.WaitKind == WaitKindVacate)
	if len(stamps) == 0 {
		return true
	}
	qs := make([]bins.LeftQuery, len(stamps))
	for i, st := range stamps {
		qs[i] = bins.LeftQuery{Bin: st.Bin, Node: st.Node, Since: vacateSince(st)}
	}
	left, err := d.db.BinsLeftSince(qs)
	if err != nil {
		return false
	}
	for _, l := range left {
		if !l {
			return false
		}
	}
	return true
}

// appendTurned appends the segment after the order's wait through the one
// append path, then asks the lane its next wait is at, as every releaser does.
func (d *Dispatcher) appendTurned(o *orders.Order, steps []resolvedStep, what string) bool {
	segment, moreWaits, blockOffset := splitSegment(steps, o.WaitIndex)
	if segment == nil {
		return false
	}
	d.patchRedirectSegments(segment, o, moreWaits)
	if err := d.appendSegmentAndAdvance(o, segment, moreWaits, blockOffset, what); err != nil {
		if !errors.Is(err, ErrAppendNotOwed) && !errors.Is(err, ErrTurnNotDone) {
			log.Printf("dispatch: %s: order %d: %v (still held; the next status change or the floor retries)", what, o.ID, err)
			if ans := FleetAnswer(err); ans != "" && !IsAppendLanded(err) && o.QueueCode != "" {
				d.setQueueReason(o, protocol.QueueCode(o.QueueCode), QueueCause(o.QueueCause),
					QueueParams{FleetReason: ans})
			}
		}
		return IsAppendLanded(err)
	}
	d.evaluateLaneWaitAfterAppend(o)
	return true
}

// sweepTurnWaiters is PopTurnHeld's floor, on the lane floor's 60 s tick:
// the turn nodes re-read (a write that went around the doors is logged), every
// held order re-asked, and a record for each it appended — an append a robot
// status change should have made. After a restart this is what finds them.
func (d *Dispatcher) sweepTurnWaiters() {
	if turner, _ := d.tableTurner(); turner != nil {
		if err := d.ReloadTableTurns(); err != nil {
			log.Printf("dispatch: table turn floor: could not re-read the turn nodes: %v", err)
		}
	}
	for _, id := range d.RedriveTableTurns(0) {
		d.recordFloorRelease(floorWaiter{orderID: id, pop: PopTurnHeld, cause: CauseTableTurning})
	}
}
