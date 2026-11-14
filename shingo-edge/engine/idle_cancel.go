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
// THE HOUR STARTS at the latest of three things: the line's last counted part,
// the end of its last changeover, and the position's last go (a REQUEST, or
// Processes turning its auto-reorder on: runtime reorder_go_at). Measured from
// the part alone, a REQUEST at a line quiet since last shift would be sent
// back on the next pass. orders.created_at is never read: it is SQLite's wall
// clock, and the line's is the engine's.
//
// IT STANDS DOWN, WITH NO LIMIT, while a changeover is open, and while the
// counter has not read a number since this Edge started or has failed since
// (stationHealthTracker.readSinceBoot): a silence it cannot hear is not a
// stopped line.
//
// WHAT IT SENDS BACK is a parked group: a lone order, or a swap pair, every
// live leg of which is staged at a station wait (a person's RELEASE) and was
// authored by this Edge (parkedLeg). Never an order no robot has reached, never
// one moving, never a leg Core made: cancelling a leg makes Core cancel its
// partner (shingo-core/dispatch/swap_peer.go), so a pair is sent back only
// whole.
//
// WITHIN A PASS it runs first (reconcileDemand), and for each group writes off
// at its position before it cancels: the level pass after it reads the off and
// asks for nothing, and the keep-staged keeper after it sees the cancels. The
// cancels go through AbortOrderWithReason, the door every Edge cancel takes,
// with protocol.CancelReasonLineIdle; the cancel never expires on the wire.
//
// A PART AFTER A STOP ENDS AN OFF. The rule remembers, per line, whether the
// line was stopped (silent for the idle hour) at the last pass; the first part
// after that is a restart, and every off at the line set before it clears.
// Memory only: after a reboot the first pass learns the line afresh, and a
// restart while the Edge was down ends nothing (a go still does). An off an
// older Core wrote at a running line ("abandoned: ..." for an order that
// waited) is not ended by the line running on, only by a stop and a restart, or
// a go.
//
// COST per pass: one statement (the polled points). Per line silent for its
// hour with a counter it trusts: one more (ChangeoverBounds), and one more
// (its live orders) when no changeover is open. Only when a group is parked:
// the line's nodes and runtime rows, and under the locks a re-read of the
// group and the position before the writes. Per restart: one UPDATE. Nothing
// per PLC tick.

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
	groups := parkedGroups(rows)
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
		e.sendBackParked(node, g, since, now, idle)
	}
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
// moved a leg or stamped a go, and the group is left. Then it writes off and
// cancels each leg.
func (e *Engine) sendBackParked(node *processes.Node, g parkedGroup, since, now time.Time, idle time.Duration) {
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
	if now.Sub(latest(since, rt.ReorderGoAt)) < idle {
		return // someone said go here within the hour
	}
	for _, id := range g.orderIDs {
		o, err := e.db.GetOrder(id)
		if err != nil || !parkedLeg(o) {
			return // moved, ended or unreadable since the pass read it
		}
	}
	note := fmt.Sprintf("the line counted no part for %s and the robots waiting here were sent back", idleWords(idle))
	if _, err := processes.HoldReorder(e.db.DB, node.ID, domain.ReorderHoldOff, now, note); err != nil {
		e.logFn("idle-cancel: node %s: write off: %v — nothing sent back", node.Name, err)
		return
	}
	for _, id := range g.orderIDs {
		if err := e.orderMgr.AbortOrderWithReason(id, protocol.CancelReasonLineIdle); err != nil {
			e.logFn("idle-cancel: node %s: cancel order %d: %v", node.Name, id, err)
			continue
		}
		e.logFn("idle-cancel: node %s: order %d sent back — the line has counted no part since %s",
			node.Name, id, since.Format(time.RFC3339))
	}
	e.nudgeBoards()
}

// parkedGroup is a lone order or a swap pair the rule may send back: every
// live leg parked (parkedLeg), all at one position.
type parkedGroup struct {
	position int64
	orderIDs []int64
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
	}
	return g, len(g.orderIDs) > 0
}

// parkedLeg reports whether an order is a robot parked at a station wait for a
// person's RELEASE, on a row this Edge authored: staged, at a wait the station
// advances, not held at a lane gate Core owns.
func parkedLeg(o *storeorders.Order) bool {
	return o != nil && o.Status == protocol.StatusStaged && protocol.IsStationWaitKind(o.WaitKind) &&
		!o.LaneHeld && o.AuthoredBy == storeorders.AuthoredByEdge
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
