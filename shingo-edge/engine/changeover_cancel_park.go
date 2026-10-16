package engine

import (
	"log"
	"slices"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// changeover_cancel_park.go — a cancelled changeover leg's parked bin.
//
// The single-robot changeover leg lifts the line's bin, sets it down on the
// outgoing claim's outbound staging, collects the incoming bin, delivers it,
// and only then takes the parked bin on to the outbound destination. A cancel
// in between aborts the leg with the line's bin standing on outbound staging,
// and every later single-robot swap at the line drops there: each one waits
// behind it for good. So the cancel finishes that bin's trip, with one plain
// move to where the aborted leg was taking it.
//
// On a claim that keeps no spare, the leg parks the incoming bin on inbound
// staging first, and collects it itself after parking the line's bin
// (relayedTrip; a separate stage order did this until 2026-10-16, stagedTrip).
// A cancel in between leaves that bin where every later swap sets its own
// incoming bin down, so it goes back to where it was fetched from, by the same
// plain move and in the same read.

// parkedBin is one bin a changeover order left standing: the line's bin a leg
// parked, bound for dest later in the same leg, or the incoming bin a stage set
// down, with dest the place it was fetched from.
type parkedBin struct {
	nodeID     int64
	line       string
	park, dest string
}

// parkedTrip reads a leg's steps for the park of the bin it lifts off line: a
// staging dropoff straight after the pickup at the line, a later pickup at the
// same node, and the dropoff after that. Only the line's own bin counts: a
// bin parked on inbound staging on its way TO the line is the incoming style's,
// and a cancel must not carry it on. ok is false when the leg parks nothing or
// the dropoff after the park names no node.
func parkedTrip(steps []protocol.ComplexOrderStep, line string) (park, dest string, ok bool) {
	lifted := false
	for i, s := range steps {
		switch s.Action {
		case protocol.ActionPickup:
			lifted = s.Node == line
		case protocol.ActionDropoff:
			if !lifted || !s.ExclusiveSlot || s.Node == "" {
				lifted = false
				continue
			}
			return parkDestination(steps[i+1:], s.Node)
		}
	}
	return "", "", false
}

// parkDestination finds, in the steps after a park on node, the pickup there
// and the dropoff that follows it.
func parkDestination(rest []protocol.ComplexOrderStep, node string) (park, dest string, ok bool) {
	for j, s := range rest {
		if s.Action != protocol.ActionPickup || s.Node != node {
			continue
		}
		for _, d := range rest[j+1:] {
			if d.Action == protocol.ActionDropoff {
				return node, d.Node, d.Node != ""
			}
		}
		return "", "", false
	}
	return "", "", false
}

// setDownAtStaging finds, in an order's steps, a carrier fetched from away from
// the line and set down on a staging node: a pickup somewhere other than the
// line and an exclusive dropoff on a staging node straight after it. i is the
// dropoff's index, stage the staging node, dest the place it was fetched from.
// The one reading of that shape: a stage order (stagedTrip) ends there and
// leaves the carrier for another order; a two-robot supply (waitingAtStage)
// waits there and collects it itself.
func setDownAtStaging(steps []protocol.ComplexOrderStep, line string) (i int, stage, dest string, ok bool) {
	for i = 1; i < len(steps); i++ {
		s, prev := steps[i], steps[i-1]
		if s.Action != protocol.ActionDropoff || !s.ExclusiveSlot || s.Node == "" ||
			prev.Action != protocol.ActionPickup || prev.Node == "" || prev.Node == line {
			continue
		}
		return i, s.Node, prev.Node, true
	}
	return 0, "", "", false
}

// stagedTrip reads a stage order's steps for the bin it set down to wait: a
// carrier set down at staging (setDownAtStaging) and no later pickup there in
// the same order. The bin waits for another order to collect it, and dest is
// where it was fetched from. A two-robot supply collects its own staged bin, so
// it is not a stage; waitingAtStage is its reading.
func stagedTrip(steps []protocol.ComplexOrderStep, line string) (stage, dest string, ok bool) {
	i, stage, dest, ok := setDownAtStaging(steps, line)
	if !ok {
		return "", "", false
	}
	for _, later := range steps[i+1:] {
		if later.Action == protocol.ActionPickup && later.Node == stage {
			return "", "", false
		}
	}
	return stage, dest, true
}

// relayedTrip reads an order's steps for the carrier it parks on staging and
// collects again itself (relayPickup): a carrier set down at staging
// (setDownAtStaging) and a later pickup there in the same order. The
// single-robot changeover is that order since 2026-10-16: aborted before its
// collect, the carrier it parked waits on staging with nothing to collect it,
// exactly as a stage order's did, and goes back the same way.
// TestSingleRobotCancel_SendsTheStagedBinBack pins it.
func relayedTrip(steps []protocol.ComplexOrderStep, line string) (stage, dest string, ok bool) {
	i, stage, dest, ok := setDownAtStaging(steps, line)
	if !ok {
		return "", "", false
	}
	for _, later := range steps[i+1:] {
		if later.Action == protocol.ActionPickup && later.Node == stage {
			return stage, dest, true
		}
	}
	return "", "", false
}

// waitingAtStage reads a supply leg's steps for the carrier it sets down at
// staging to WAIT with: a carrier set down at staging (setDownAtStaging) and a
// station wait on that node straight after the drop. waitOrdinal is that wait's
// number among the order's station waits, the number Core reports an order
// staged at (Order.StationWait).
func waitingAtStage(steps []protocol.ComplexOrderStep, line string) (stage, dest string, waitOrdinal int, ok bool) {
	i, stage, dest, ok := setDownAtStaging(steps, line)
	if !ok || i+1 >= len(steps) {
		return "", "", 0, false
	}
	next := steps[i+1]
	if next.Action != protocol.ActionWait || next.Node != stage || next.WaitKind != protocol.WaitKindStation {
		return "", "", 0, false
	}
	for _, s := range steps[:i+1] {
		if s.Action == protocol.ActionWait && s.WaitKind == protocol.WaitKindStation {
			waitOrdinal++
		}
	}
	return stage, dest, waitOrdinal, true
}

// leftOnStaging is parkedTrip and stagedTrip for one changeover order: one
// steps read, and one node read when the steps leave a bin anywhere. A park
// counts only for a leg aborted now (aborted); a stage only once its order has
// reached a robot (flown), since before that it has set nothing down.
func (e *Engine) leftOnStaging(order *domain.Order, processNodeID int64, aborted, flown bool) (park, stage parkedBin, parked, staged bool) {
	if order.OrderType != ordermgr.TypeComplex || (!aborted && !flown) {
		return
	}
	raw, err := e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		return
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		return
	}
	node, err := e.db.GetProcessNode(processNodeID)
	if err != nil || node == nil {
		return
	}
	if p, dest, ok := parkedTrip(steps, node.CoreNodeName); ok && aborted {
		park, parked = parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: p, dest: dest}, true
	}
	if s, dest, ok := stagedTrip(steps, node.CoreNodeName); ok && flown {
		stage, staged = parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: s, dest: dest}, true
	}
	if s, dest, ok := relayedTrip(steps, node.CoreNodeName); ok && flown && aborted {
		stage, staged = parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: s, dest: dest}, true
	}
	return
}

// taskLeftOnStaging collects what one changeover task's orders left standing,
// aborting the live ones as it goes. A stage counts only beside a park: the
// leg that would have collected the staged bin was aborted now, so the bin is
// still waiting, and the read that answers for the park answers for it too.
// Without a live leg the stage was collected, or its leg ended earlier.
func (e *Engine) taskLeftOnStaging(task processes.NodeTask) []parkedBin {
	var parks, stages []parkedBin
	for _, orderID := range []*int64{task.NextMaterialOrderID, task.OldMaterialReleaseOrderID} {
		if orderID == nil {
			continue
		}
		order, err := e.db.GetOrder(*orderID)
		if err != nil {
			continue
		}
		flown := protocol.ChangeoverStartActionFor(order.Status) != protocol.ChangeoverStartCancel &&
			order.Status != protocol.StatusCancelled && order.Status != protocol.StatusFailed
		aborted := !ordermgr.IsTerminal(order.Status)
		if aborted {
			if err := e.orderMgr.AbortOrder(order.ID); err != nil {
				log.Printf("changeover cancel: abort order %s: %v", order.UUID, err)
			}
		}
		park, stage, parked, staged := e.leftOnStaging(order, task.ProcessNodeID, aborted, flown)
		if parked {
			parks = append(parks, park)
		}
		if staged {
			stages = append(stages, stage)
		}
	}
	if len(parks) == 0 {
		return nil
	}
	return append(parks, stages...)
}

// parkNames is every park node, once each.
func parkNames(parks []parkedBin) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range parks {
		if !seen[p.park] {
			seen[p.park] = true
			out = append(out, p.park)
		}
	}
	return out
}

// finishParkedTrips orders, for each park Core reports a bin on, one plain move
// carrying that bin's own payload to its dest.
// Attributed to the line, never in its runtime slots, and created under the
// line's prime lock, as every other order the line's cell decides. One move per
// park node: one node holds one bin.
func (e *Engine) finishParkedTrips(parks []parkedBin, rows map[string]NodeBinInfo, origin ordermgr.Origin) {
	done := map[string]bool{}
	for _, p := range parks {
		row, read := rows[p.park]
		if done[p.park] || !read || !row.Occupied {
			continue
		}
		done[p.park] = true
		mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: p.line})
		mu.Lock()
		nodeID := p.nodeID
		_, err := e.orderMgr.CreateMoveOrderCarrying(&nodeID, p.park, p.dest, row.PayloadCode, origin)
		mu.Unlock()
		if err != nil {
			e.logFn("cancel: %s: send the bin left on %s on to %s: %v", p.line, p.park, p.dest, err)
			continue
		}
		e.logFn("cancel: %s: the bin left on %s goes on to %s", p.line, p.park, p.dest)
	}
}

// withoutSpots drops what stands on a keep-staged spot: the spots' own
// decision at the cancel covers whatever stands there.
func withoutSpots(parks []parkedBin, spots []spotChange) []parkedBin {
	if len(spots) == 0 {
		return parks
	}
	out := parks[:0]
	for _, p := range parks {
		if !slices.ContainsFunc(spots, func(ch spotChange) bool { return ch.spot == p.park }) {
			out = append(out, p)
		}
	}
	return out
}

// parkAsked reports whether a request on this claim asks Core about its
// outbound staging: single-robot only, the one swap that parks the line's bin
// there on its way out.
func parkAsked(claim *processes.NodeClaim) bool {
	return claim != nil && claim.SwapMode == protocol.SwapModeSingleRobot && claim.OutboundStaging != ""
}

// clearStrandedPark is the request's half of the cancel's park: a bin standing on
// a single-robot claim's outbound staging, read in the request's own node-bins
// call, goes on to the outbound destination by the same plain move.
//
// A PARK CAN BE SET DOWN AFTER THE CANCEL'S READ. The cancel queues its abort to
// Core and reads node-bins at once; a robot still carrying the line's bin when
// that read answers sets it down on outbound staging afterwards, and the cancel
// has already decided there was nothing to move. Every later single-robot swap
// at the line parks there, so it would wait behind that bin for good.
//
// Called after the request's guards, so no live swap holds the line's slots. It
// still leaves the bin when any live complex order of the line remains (a leg
// that has left the line but will still collect its parked bin) or a live move
// already takes it off (the cancel's own, or an earlier request's). The line's
// rows are read only when Core reports a bin there. The move names no demand:
// it is the cell's own clean-up, not what the request asked for.
func (e *Engine) clearStrandedPark(node *processes.Node, claim *processes.NodeClaim, park NodeBinInfo) {
	if !park.Occupied || !parkAsked(claim) || claim.OutboundDestination == "" {
		return
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		e.logFn("request: %s: cannot read the line's orders (%v) — the bin on %s is left for the next request",
			node.Name, err, claim.OutboundStaging)
		return
	}
	for _, o := range rows {
		if o.OrderType == ordermgr.TypeComplex || (o.OrderType == ordermgr.TypeMove && o.SourceNode == claim.OutboundStaging) {
			return
		}
	}
	nodeID := node.ID
	if _, err := e.orderMgr.CreateMoveOrderCarrying(&nodeID, claim.OutboundStaging, claim.OutboundDestination,
		park.PayloadCode, ordermgr.NoDemand()); err != nil {
		e.logFn("request: %s: send the bin left on %s on to %s: %v", node.Name, claim.OutboundStaging,
			claim.OutboundDestination, err)
		return
	}
	e.logFn("request: %s: the bin left on %s goes on to %s", node.Name, claim.OutboundStaging, claim.OutboundDestination)
}

// carrierLeftWaiting is a cancelled order's carrier left standing at staging:
// the order was cancelled while staged at the station wait that follows its own
// drop at staging (waitingAtStage), so the carrier is down on that node, and
// nothing will collect it — the order that would have is the one cancelled. A
// two-robot supply cancelled at its wait is the case (SPR 2026-10-14: CARRIER-0026
// with 350 of 76682 left on SLN_001, which then blocked the next swap's
// dropoff). ok is false for any other order or moment.
func (e *Engine) carrierLeftWaiting(changed OrderStatusChangedEvent) (parkedBin, bool) {
	if changed.NewStatus != string(protocol.StatusCancelled) || changed.OldStatus != string(protocol.StatusStaged) {
		return parkedBin{}, false
	}
	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order == nil || order.OrderType != ordermgr.TypeComplex ||
		order.ProcessNodeID == nil || order.StationWait == nil {
		return parkedBin{}, false
	}
	raw, err := e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		return parkedBin{}, false
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		return parkedBin{}, false
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil || node == nil {
		return parkedBin{}, false
	}
	stage, dest, ordinal, ok := waitingAtStage(steps, node.CoreNodeName)
	if !ok || *order.StationWait != ordinal {
		return parkedBin{}, false
	}
	// A keep-staged spot is the keeper's: a carrier standing there is the spare
	// it wants, and the changeover cancel leaves spots to it too (withoutSpots).
	// Unreadable claims leave the carrier where it is rather than guess.
	claims, err := processes.ListLiveClaimsByProcess(e.db.DB, node.ProcessID)
	if err != nil || slices.Contains(keepStagedSpotNames(claims), stage) {
		return parkedBin{}, false
	}
	return parkedBin{nodeID: node.ID, line: node.CoreNodeName, park: stage, dest: dest}, true
}

// returnCarrierLeftWaiting sends a cancelled order's waiting carrier back to
// where it was fetched from — the same plain move the changeover cancel makes
// for a stage it leaves behind (finishParkedTrips), so one move, one rule, for
// every cancel: Core's admin terminate, an Edge abort, a changeover cancel. Only
// a carrier Core reports on the node moves.
//
// OFF THE CANCELLING GOROUTINE. A status change is handled on the goroutine
// that made it, and a changeover cancel aborts its orders under the cells'
// locks; the move takes the line's prime lock and asks Core first, so it runs
// after the cancel has let go, as the changeover cancel's own park does.
func (e *Engine) returnCarrierLeftWaiting(changed OrderStatusChangedEvent) {
	p, ok := e.carrierLeftWaiting(changed)
	if !ok {
		return
	}
	go e.sendCarrierBack(p)
}

func (e *Engine) sendCarrierBack(p parkedBin) {
	_, rows := e.readSpotsAnd(nil, []string{p.park})
	e.finishParkedTrips([]parkedBin{p}, rows, ordermgr.NoDemand())
}
