package engine

import (
	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// keep_staged_floor.go — the level keeper's floor for a keep-staged spot.
//
// The request path fills the spot as it goes, but there is one state no request
// reaches: a swap already asked for and waiting at Core for its spare, with
// nothing on its way to the spot because the refills it was counting on ended.
// REQUEST is refused while the swap is in flight, so nobody asks again; the swap
// waits for a spare that is never coming. The floor is the reconcile for that
// state, run by the level sweep for keep-staged claims and kicked when a refill
// goes terminal.
//
// COST. Nothing for a keep-staged cell whose runtime slots are empty (no swap
// in flight). One indexed read of the line's orders for a cell with a live swap.
// In the trigger state only, one node-bins read for the spot and the orders the
// reconcile decides.

// keepStagedFloor runs the spot reconcile for a keep-staged line whose swap is
// waiting at Core with nothing coming to the spot.
//
// THE TRIGGER, over Edge's own facts: a swap leg in the line's runtime slots is
// still acquiring at Core (queued or sourcing) — not yet with the fleet — and no
// refill for the claim's part and role is on its way. Status alone: Core pushes
// a wait's cause once, at intake, so a cause on the row says nothing about now.
//
// THE STOP. It does not re-create after a refill to the spot that ended failed
// or skipped: Core fails a plain order for a structural reason (congestion
// waits), so re-creating it each period would be a failure on a timer. The
// operator's REQUEST is not subject to the stop and re-arms it. A cancelled
// refill is re-created: a cancel is a person's or the fleet's, not the plan's.
func (e *Engine) keepStagedFloor(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) {
	spot := spotNode(claim)
	if node == nil || runtime == nil || spot == "" {
		return
	}
	if runtime.ActiveOrderID == nil && runtime.StagedOrderID == nil {
		return // no swap in flight: nothing can be waiting for a spare
	}
	// TRY, NEVER WAIT. A held lock is a request, a changeover start or cancel,
	// or a save deciding this cell now, and its decision counts the spot. And
	// the kick runs inside the completion cascade, which events deliver
	// synchronously: a changeover start aborting under the lock would reach
	// here on its own goroutine. The next sweep re-asks.
	mu := e.primeNodeLock(claim)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()

	rows, err := storeorders.ListActiveByProcessNodeWithLatestTo(e.db.DB, node.ID, spot)
	if err != nil {
		e.logFn("keep-staged floor: node %s: read the line's orders: %v — the next pass re-asks", node.Name, err)
		return
	}
	leg := waitingSwapLeg(rows, runtime)
	if leg == nil || spotComing(rows, claim) > 0 {
		return
	}
	if last := latestRefill(rows, claim); last != nil &&
		(last.Status == ordermgr.StatusFailed || last.Status == ordermgr.StatusSkipped) {
		e.debugFn("keep-staged floor: node %s: the last refill to %s ended %s; not re-created — REQUEST re-arms it",
			node.Name, spot, last.Status)
		return
	}
	if e.coreClient == nil || !e.coreClient.Available() {
		return
	}
	bins, _, ferr := e.coreClient.FetchNodeBins([]string{spot})
	if ferr != nil {
		e.logFn("keep-staged floor: node %s: read the spot %s: %v — the next pass re-asks", node.Name, spot, ferr)
		return
	}
	read := spotOf(claim, bins, e.spotNodeKnown, e.PayloadBinTypes()).lessLeaving(spotLeaving(rows, spot, claim.CoreNodeName))
	if !read.known {
		return
	}
	plan := reconcileSpot(claim, read.occupied, spareIsRight(claim, read), 0, 1, 1)
	origin := ordermgr.NoDemand()
	if leg.OriginID != "" {
		origin = ordermgr.Attached(leg.OriginID) // the refill serves the swap's demand
	}
	e.logFn("keep-staged floor: node %s: swap %d waits for a spare with nothing coming to %s",
		node.Name, leg.ID, spot)
	e.applySpotPlan(node, claim, plan, read, origin)
}

// waitingSwapLeg is the swap leg in the line's runtime slots that is still
// acquiring at Core, or nil.
func waitingSwapLeg(rows []domain.Order, runtime *processes.RuntimeState) *domain.Order {
	for i := range rows {
		o := &rows[i]
		for _, slot := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
			if slot != nil && *slot == o.ID && protocol.IsAcquiring(o.Status) {
				return o
			}
		}
	}
	return nil
}

// latestRefill is the newest retrieve the line sent to the claim's spot, any
// status, among rows; nil when there has been none.
func latestRefill(rows []domain.Order, c *processes.NodeClaim) *domain.Order {
	var last *domain.Order
	for i := range rows {
		o := &rows[i]
		if isRetrieve(o.OrderType) && o.DeliveryNode == spotNode(c) && (last == nil || o.ID > last.ID) {
			last = o
		}
	}
	return last
}

// kickKeepStagedFloor runs the floor at once when a keep-staged refill goes
// terminal, instead of at the next sweep. The completion cascade has already
// read the order, the node and the runtime row; the claim read is the one the
// cascade makes anyway, and only for a plain retrieve that did not deliver to
// the line.
func (e *Engine) kickKeepStagedFloor(ctx *orderCompletionCtx) {
	o := ctx.order
	if !isRetrieve(o.OrderType) || o.DeliveryNode == "" || o.DeliveryNode == ctx.node.CoreNodeName {
		return // only a refill's end kicks anything: a return's completion orders nothing
	}
	if o.Status == ordermgr.StatusConfirmed {
		e.returnUnwantedLanding(ctx)
	}
	if ctx.toStyleID != 0 {
		return // a changeover is armed: its start and cancel own the spots (see sweepProcessLevels)
	}
	claim := ctx.Claim()
	if claim == nil || !isSpotRefill(o, claim) {
		return
	}
	runtime, err := e.db.GetProcessNodeRuntime(ctx.node.ID)
	if err != nil {
		return
	}
	e.keepStagedFloor(ctx.node, runtime, claim)
}

// returnUnwantedLanding sends a refill's bin straight back when it lands on a
// spot nobody keeps it for.
//
// A refill already with the fleet cannot be cancelled, so it lands, possibly
// after the last decision about its spot: a changeover cancelled while the
// incoming style's refill was flying leaves that style's bin on a spot the
// staying style keeps. A decision point reads the spot only when one runs, and
// a swap waiting at Core for its spare does not run one; Edge does know, with
// no read, what it ordered. It judges the landing by the refill's part and
// role, not by the carrier: it has no read of the bin to judge a carrier by.
//
// "Wanted" is judged against the claim the spot is kept for now: the armed
// changeover's incoming claim if one is armed, else the active claim. The bin
// is wanted when that claim keeps this spot for the refill's part and role; a
// refill for the incoming style landing before cutover stays. Otherwise it goes
// back by a plain move to the refill's own source, carrying the refill's part,
// an empty untagged. Only on a keep-staged spot of this line: a plain retrieve
// to any other node is not this code's to undo.
//
// One return per bin: none is made while a live return from the spot exists,
// and the request, floor and changeover reconciles count a live return as the
// spare leaving. That check reads the line's live rows once, only when a landing
// is unwanted; the cascade has already read the order, the node and the claim.
//
// Under the cell's prime lock, taken with a wait: a refill lands from Core's
// confirm on the messaging goroutine, never inside a section that holds a
// cell's lock (those abort orders; they do not confirm them).
func (e *Engine) returnUnwantedLanding(ctx *orderCompletionCtx) {
	o := ctx.order
	keeper := ctx.Claim()
	if ctx.toStyleID != 0 {
		keeper = ctx.ToClaim()
	}
	if landingWanted(keeper, o) {
		return
	}
	active := ctx.Claim()
	if !keptSpot(keeper, o.DeliveryNode) && !keptSpot(active, o.DeliveryNode) {
		return
	}
	mu := e.primeNodeLock(&processes.NodeClaim{CoreNodeName: ctx.node.CoreNodeName})
	mu.Lock()
	defer mu.Unlock()
	rows, err := e.db.ListActiveOrdersByProcessNode(ctx.node.ID)
	if err != nil {
		e.logFn("keep-staged: node %s: read the line's orders before returning a landed refill: %v", ctx.node.Name, err)
		return
	}
	if spotLeaving(rows, o.DeliveryNode, ctx.node.CoreNodeName) > 0 {
		return // already going back
	}
	carried := o.PayloadCode
	if o.RetrieveEmpty {
		carried = ""
	}
	// The bin the refill delivered, when Core told the station which one.
	var binID int64
	if o.BinID != nil {
		binID = *o.BinID
	}
	e.logFn("keep-staged: node %s: refill %d landed %s on %s, which is not kept for it — sent back to %s",
		ctx.node.Name, o.ID, o.PayloadCode, o.DeliveryNode, o.SourceNode)
	e.returnSpare(ctx.node, o.DeliveryNode, o.SourceNode, carried, binID, ordermgr.NoDemand())
}

// landingWanted reports whether claim keeps the refill's spot for its part and
// role.
func landingWanted(c *processes.NodeClaim, o *domain.Order) bool {
	return keptSpot(c, o.DeliveryNode) && o.PayloadCode == c.PayloadCode && o.RetrieveEmpty == carriesEmpty(c)
}

// keptSpot reports whether claim keeps a spare on spot.
func keptSpot(c *processes.NodeClaim, spot string) bool {
	return spot != "" && spotNode(c) == spot
}
