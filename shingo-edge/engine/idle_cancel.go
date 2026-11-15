package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/counters"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// idle_cancel.go — the one cancel rule.
//
// WHEN A LINE HAS COUNTED NO PART FOR ITS IDLE HOUR, the Edge sends back the
// robots parked at that line waiting on a person, tells Core, and the
// positions they were parked at go off until the line counts a part after the
// stop or someone gives a go. On a running line nothing is cancelled. Core
// keeps no clock of its own for this: the Edge is the one with the counter.
//
// THE LINE'S CLOCK is reporting_points.last_poll_at, the engine's clock at the
// last read that counted (plc pollReportingPoint). It is stamped by the
// reporting point's UPDATE before the stroke is handed to the engine, so a
// stroke the strict-pack hold sets aside at a full cart still moves it. A read
// that counts nothing writes nothing. One reporting point per process
// (counters.ListPolledReportingPoints), so a line is a process with a counter
// the poll reads; a process with none is outside the rule.
//
// THE HOUR STARTS at the latest of four things: the line's last counted part,
// the end of its last changeover, the position's last go (a REQUEST, or
// Processes turning its auto-reorder on: runtime reorder_go_at), and when the
// group's robots parked (the latest orders.staged_at among its legs, the
// engine's clock, stamped by the staged mirror's own UPDATE). Measured from
// the part alone, a REQUEST at a line quiet since last shift would be sent
// back on the next pass, and a pair that parks at a line stopped for three
// hours, waiting for exactly that swap, as it arrives. orders.created_at is
// never read: it is SQLite's wall clock, and the line's is the engine's.
//
// IT STANDS DOWN, WITH NO LIMIT, while a changeover is open, and while the
// counter has not read a number since this Edge started or has failed since
// (stationHealthTracker.readSinceBoot): a silence it cannot hear is not a
// stopped line.
//
// WHAT IT SENDS BACK is a parked group: a lone order, or a swap pair, every
// live leg of which is staged at a wait Core named the station's (a person's
// RELEASE) and was authored by this Edge (parkedLeg). Never an order no robot
// has reached, never one moving, never one Core released (Core releases only
// its own waits), never a leg Core made: cancelling a leg makes Core cancel its
// partner (shingo-core/dispatch/swap_peer.go), so a pair is sent back only
// whole.
//
// WITHIN A PASS it runs first (reconcileDemand), and for each group writes off
// at its position before it cancels: the level pass after it reads the off and
// asks for nothing, and the keep-staged keeper after it sees the cancels. The
// cancels go through AbortOrderAsEdge, the door every Edge cancel takes,
// with protocol.CancelReasonLineIdle and protocol.CancelActorEdge, so Core
// files them as the Edge's and not a person's; the cancel never expires.
//
// A PART AFTER A STOP ENDS AN OFF. The rule's own off (domain.IsIdleOff, by
// its note) was written at a line that had counted nothing for its hour, so
// any part the line counts after it is a restart: it ends when the line's last
// counted part (reporting_points.last_part_at) passes the off's
// reorder_hold_at, read from the database, so a restart while the Edge was
// down, or ten minutes after it booted, ends it all the same. last_part_at is
// not last_poll_at: a reading with no part (a counter reset to 0, a point's
// first read) moves the hour's clock and not this one. Memory (idleOffs)
// decides only when to look, so a line that holds none costs nothing.
//
// An off an older Core wrote ("abandoned: ..." for an order that waited) may
// have been written at a running line, so a part after it is not a restart:
// the rule remembers, per line, whether the line was stopped (silent for its
// hour) at the last pass, and the first part after that ends it (lineStops).
// That one is memory only, as before: it is the interim, for a Core that still
// gives up on its own; a go still ends it.
//
// COST per pass: one statement (the polled points). Per line silent for its
// hour with a counter it trusts: one more (ChangeoverBounds), and one more
// (its live orders) when no changeover is open. Only when a group is parked
// past its hour: the line's nodes, and under the locks a re-read of the group
// and the position before the writes. Ending the rule's offs: one UPDATE per
// line on the first pass after a start, then one per line only where an off
// of the rule's may still wait and the line has counted a part since the last
// look (idleOffWatch). Per restart an older Core's off is waiting on: one
// UPDATE. Nothing per PLC tick: the poll's own UPDATE stamps last_part_at, and
// the staged mirror's own UPDATE staged_at.

// lineStop is what the rule remembers of a line between passes (lineStops).
type lineStop struct {
	lastPart time.Time
	stopped  bool
}

// idleCancelLines is the rule's pass over every line the poll reads.
func (e *Engine) idleCancelLines() {
	points, err := e.db.ListPolledReportingPoints()
	if err != nil {
		e.logFn("idle-cancel: read the lines' counters: %v — this pass cancels nothing", err)
		return
	}
	now := e.reconcilerClock().Now()
	idle := e.cfg.IdleCancelAfter()
	for i := range points {
		e.idleCancelLine(&points[i], now, idle)
	}
}

// idleCancelLine is the rule for one line: end the offs a restart ends, then,
// if the line has been idle its hour, send back what is parked there.
func (e *Engine) idleCancelLine(rp *counters.ReportingPoint, now time.Time, idle time.Duration) {
	if rp.LastPollAt == nil {
		return // never read: no clock yet
	}
	part := *rp.LastPollAt
	e.endIdleOffs(rp.ProcessID, rp.LastPartAt, now, idle)
	if e.noteLinePart(rp.ProcessID, part, now, idle) {
		if n, err := processes.ClearReorderOffAtProcess(e.db.DB, rp.ProcessID, part); err != nil {
			e.logFn("idle-cancel: process %d restarted: clear its offs: %v", rp.ProcessID, err)
		} else if n > 0 {
			e.logFn("idle-cancel: process %d counted a part after a stop: auto-reorder back on at %d position(s)", rp.ProcessID, n)
			e.nudgeBoards()
		}
	}
	if now.Sub(part) < idle {
		return
	}
	if !e.stationHealth.readSinceBoot(rp.PLCName, rp.TagName) {
		return
	}
	open, lastEnd, err := processes.ChangeoverBounds(e.db.DB, rp.ProcessID)
	if err != nil {
		e.logFn("idle-cancel: process %d: read its changeovers: %v — this pass cancels nothing", rp.ProcessID, err)
		return
	}
	if open {
		return
	}
	since := latest(part, lastEnd)
	if now.Sub(since) < idle {
		return
	}
	rows, err := storeorders.ListActiveStrictByProcess(e.db.DB, rp.ProcessID)
	if err != nil {
		e.logFn("idle-cancel: process %d: read its live orders: %v — this pass cancels nothing", rp.ProcessID, err)
		return
	}
	var groups []parkedGroup
	for _, g := range parkedGroups(rows) {
		if now.Sub(latest(since, g.stagedAt)) >= idle {
			groups = append(groups, g) // parked past its hour; robots that arrived within it wait
		}
	}
	if len(groups) == 0 {
		return
	}
	nodes, err := e.db.ListProcessNodesByProcess(rp.ProcessID)
	if err != nil {
		e.logFn("idle-cancel: process %d: read its positions: %v — this pass cancels nothing", rp.ProcessID, err)
		return
	}
	byID := make(map[int64]*processes.Node, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	for _, g := range groups {
		node := byID[g.position]
		if node == nil {
			continue
		}
		e.sendBackParked(rp.ProcessID, node, g, since, now, idle)
	}
}

// idleOffWatch is when the rule looks for its own offs to end at a line:
// pending while one may wait there for a part (from a start, and from each
// send-back, until the line is seen running), and the last part it looked at.
// A line running past its last look holds none: the rule wrote each off at a
// moment the line had counted nothing for its hour, and a part since then has
// ended it (ClearIdleOffAtProcess).
type idleOffWatch struct {
	checked time.Time
	pending bool
}

// endIdleOffs ends the rule's offs at a line its last counted part has passed
// (processes.ClearIdleOffAtProcess). It runs the statement once per line
// after a start, then only where an off of the rule's may still wait and the
// line has counted since the last look.
func (e *Engine) endIdleOffs(processID int64, lastPart *time.Time, now time.Time, idle time.Duration) {
	if lastPart == nil {
		return // no part counted since this clock began: nothing has restarted
	}
	v, seen := e.idleOffs.Load(processID)
	w, _ := v.(idleOffWatch)
	if seen && (!w.pending || !lastPart.After(w.checked)) {
		return
	}
	n, err := processes.ClearIdleOffAtProcess(e.db.DB, processID, *lastPart)
	if err != nil {
		e.logFn("idle-cancel: process %d: end its offs: %v", processID, err)
		return
	}
	if n > 0 {
		e.logFn("idle-cancel: process %d counted a part after its offs were set: auto-reorder back on at %d position(s)", processID, n)
		e.nudgeBoards()
	}
	e.idleOffs.Store(processID, idleOffWatch{checked: *lastPart, pending: now.Sub(*lastPart) >= idle})
}

// noteSentBack marks a line as holding an off of the rule's, which the next
// part it counts ends (endIdleOffs).
func (e *Engine) noteSentBack(processID int64) {
	v, _ := e.idleOffs.Load(processID)
	w, _ := v.(idleOffWatch)
	w.pending = true
	e.idleOffs.Store(processID, w)
}

// noteLinePart records the line's last part at this pass and reports whether
// it is a restart: a part after a pass that found the line stopped.
func (e *Engine) noteLinePart(processID int64, part, now time.Time, idle time.Duration) bool {
	was, _ := e.lineStops.Load(processID)
	prev, seen := was.(lineStop)
	e.lineStops.Store(processID, lineStop{lastPart: part, stopped: now.Sub(part) >= idle})
	return seen && prev.stopped && part.After(prev.lastPart)
}

// sendBackParked sends back one parked group at node, the line silent since
// since. Under the position's release lock and its cell lock, it reads the
// group and the position again: a RELEASE or a REQUEST that won the locks has
// moved a leg or stamped a go, a leg staged again has a later arrival, and
// the group is left. Then it writes off and cancels each leg.
func (e *Engine) sendBackParked(processID int64, node *processes.Node, g parkedGroup, since, now time.Time, idle time.Duration) {
	unlock := e.lockNode(node.ID)
	defer unlock()
	cell := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: node.CoreNodeName})
	if !cell.TryLock() {
		e.logFn("idle-cancel: node %s: the cell is being decided (its lock is held) — this pass sends nothing back, the next re-asks", node.Name)
		return
	}
	defer cell.Unlock()

	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil {
		e.logFn("idle-cancel: node %s: read its runtime row: %v — nothing sent back", node.Name, err)
		return
	}
	since = latest(since, rt.ReorderGoAt)
	for _, id := range g.orderIDs {
		o, err := e.db.GetOrder(id)
		if err != nil || !parkedLeg(o) {
			return // moved, ended or unreadable since the pass read it
		}
		since = latest(since, stagedAt(o))
	}
	if now.Sub(since) < idle {
		return // someone said go here, or the robots parked, within the hour
	}
	if _, err := processes.HoldReorder(e.db.DB, node.ID, domain.ReorderHoldOff, now, domain.IdleOffNote(idleWords(idle))); err != nil {
		e.logFn("idle-cancel: node %s: write off: %v — nothing sent back", node.Name, err)
		return
	}
	e.noteSentBack(processID)
	for _, id := range g.orderIDs {
		if err := e.orderMgr.AbortOrderAsEdge(id, protocol.CancelReasonLineIdle); err != nil {
			e.logFn("idle-cancel: node %s: cancel order %d: %v", node.Name, id, err)
			continue
		}
		e.logFn("idle-cancel: node %s: order %d sent back — nothing has happened at the line since %s (its last part, changeover end, go or the robots' arrival)",
			node.Name, id, since.Format(time.RFC3339))
	}
	e.nudgeBoards()
}

// parkedGroup is a lone order or a swap pair the rule may send back: every
// live leg parked (parkedLeg), all at one position. stagedAt is the latest
// arrival among its legs (zero for a leg staged before the column existed).
type parkedGroup struct {
	position int64
	orderIDs []int64
	stagedAt time.Time
}

// parkedGroups groups a line's live orders by their swap link and keeps the
// groups whose every live leg is parked at one position. A partner that has
// ended is not in rows and does not hold its leg back.
func parkedGroups(rows []storeorders.Order) []parkedGroup {
	byID := make(map[int64]*storeorders.Order, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	partner := map[int64]int64{}
	for i := range rows {
		o := &rows[i]
		if o.SiblingOrderID == nil {
			continue
		}
		if _, live := byID[*o.SiblingOrderID]; live {
			partner[o.ID], partner[*o.SiblingOrderID] = *o.SiblingOrderID, o.ID
		}
	}
	seen := map[int64]bool{}
	var out []parkedGroup
	for i := range rows {
		o := &rows[i]
		if seen[o.ID] {
			continue
		}
		legs := []*storeorders.Order{o}
		seen[o.ID] = true
		if p, ok := partner[o.ID]; ok && !seen[p] {
			legs = append(legs, byID[p])
			seen[p] = true
		}
		g, ok := parkedGroupOf(legs)
		if ok {
			out = append(out, g)
		}
	}
	return out
}

// parkedGroupOf is legs as one group when every leg is parked at one position.
func parkedGroupOf(legs []*storeorders.Order) (parkedGroup, bool) {
	var g parkedGroup
	for _, o := range legs {
		if !parkedLeg(o) || o.ProcessNodeID == nil {
			return parkedGroup{}, false
		}
		if g.position != 0 && g.position != *o.ProcessNodeID {
			return parkedGroup{}, false
		}
		g.position = *o.ProcessNodeID
		g.orderIDs = append(g.orderIDs, o.ID)
		g.stagedAt = latest(g.stagedAt, stagedAt(o))
	}
	return g, len(g.orderIDs) > 0
}

// stagedAt is when the leg's robot parked (orders.staged_at), zero when the
// row has none.
func stagedAt(o *storeorders.Order) time.Time {
	if o.StagedAt == nil {
		return time.Time{}
	}
	return *o.StagedAt
}

// parkedLeg reports whether an order is a robot parked at a station wait for a
// person's RELEASE, on a row this Edge authored: staged, at a wait Core named
// the station's, not held at a lane gate Core owns, and not released already. A
// release pressed for this wait leaves an intent on the row until Core stages
// the leg at a later station wait (orders.release_intent): a leg whose release
// is held, waiting for Core, is the system's wait, not a person's (sim,
// 2026-11-14: a pair the sim operator released, held at G3 after an Edge
// restart, was sent back).
//
// THE STATION'S WAIT, IN CORE'S WORD. Core releases only a wait it owns, and
// tells the Edge nothing when it does, so the row reads staged while the robot
// drives on. Its order.staged names the owner, so a leg at Core's wait reads
// lane, vacate or turn. A blank kind is not the station's here, though the
// board reads it so (protocol.IsStationWaitKind): it is a leg no order.staged
// placed, staged by the boot reconcile, whose snapshot names no wait, after an
// outage longer than the notice's 10 minutes. That leg may be at Core's lane
// wait or past it, so it waits for a person.
func parkedLeg(o *storeorders.Order) bool {
	return o != nil && o.Status == protocol.StatusStaged && o.WaitKind == protocol.WaitKindStation &&
		!o.LaneHeld && o.AuthoredBy == storeorders.AuthoredByEdge && o.ReleaseIntent == ""
}

// latest is the later of two times; a zero time is earlier than any.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// idleWords is the idle hour as the board says it: "1 h", "10 min", "1 h 30 min".
func idleWords(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%d min", m)
	case m == 0:
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}
