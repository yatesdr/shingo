package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// keep_staged_keeper.go — the keeper of a keep-staged spot.
//
// ONE DECISION, ONE OWNER. What a spot needs is decided by decideSpot
// (keep_staged_spot.go), and only the keeper asks it: the level sweep, for every
// keep-staged line, idle ones included, so a spot is topped up without anyone
// pressing anything; and the end of any order of the line, or a pickup at the
// spot, at once instead of at the next sweep. A REQUEST does not: it asks only
// for the line, taking the spare when one stands and the market's carrier when
// none does (requestClaim), so cancelling what a REQUEST made leaves nothing of
// the spot's behind (SPR ALN_011, 2026-10-08: a refill created with each request
// outlived the request's cancel and took the carrier it freed).
//
// It replaced a floor that ran only while a swap waited at Core and assumed that
// swap would lift a spare. A swap planned before the spot was kept never does:
// it stages its own carrier ON the spot. At SPR ALN_011 (2026-10-08) the floor
// sent the bin standing there back to a source the swap's own carrier held and
// queued two refills behind both, and re-created all three on each of five
// cancels. The keeper reads the legs' steps instead of assuming them, orders
// nothing while a leg still drops onto the spot, and stops on a cancel.
//
// COST. One indexed read of the line's orders (ListKeptSpotRows) and, unless the
// rows alone settle it (paused, or a leg dropping onto the spot), one node-bins
// read for the spot, per keep-staged line per sweep.

// keepSpot runs the keeper for one keep-staged line.
//
// TRY, NEVER WAIT. A held lock is a request, a changeover start or cancel, or a
// save deciding this cell now, and its decision covers the spot. The order-end
// kick runs inside the completion cascade, which events deliver synchronously:
// a changeover start aborting under the lock reaches here on its own goroutine.
// The next sweep re-asks.
func (e *Engine) keepSpot(node *processes.Node, claim *processes.NodeClaim) {
	spot := spotNode(claim)
	if node == nil || spot == "" {
		return
	}
	mu := e.primeNodeLock(claim)
	if !mu.TryLock() {
		return
	}
	defer mu.Unlock()

	rows, err := storeorders.ListKeptSpotRows(e.db.DB, node.ID, spot, claim.CoreNodeName)
	if err != nil {
		e.logFn("keep-staged: node %s: read the line's orders: %v — the next pass re-asks", node.Name, err)
		return
	}
	resumed, rerr := processes.KeepStagedResumedAt(e.db.DB, node.ID)
	if rerr != nil {
		e.logFn("keep-staged: node %s: read the resume stamp: %v — the next pass re-asks", node.Name, rerr)
		return
	}
	f := e.spotFactsOf(claim, spotRead{known: true}, rows, resumed)
	if f.paused != "" || f.drops {
		_, note := decideSpot(claim, f)
		e.setKeepStagedNote(claim.CoreNodeName, note)
		return
	}
	if e.coreClient == nil || !e.coreClient.Available() {
		return
	}
	bins, _, ferr := e.coreClient.FetchNodeBins([]string{spot})
	if ferr != nil {
		e.logFn("keep-staged: node %s: read the spot %s: %v — the next pass re-asks", node.Name, spot, ferr)
		return
	}
	read := spotOf(claim, bins, e.spotNodeKnown, e.PayloadBinTypes())
	if !read.known {
		return
	}
	f = e.spotFactsOf(claim, read, rows, resumed)
	plan, note := decideSpot(claim, f)
	e.setKeepStagedNote(claim.CoreNodeName, note)
	e.applySpotPlan(node, claim, plan, liftingOrigin(rows, claim))
}

// spotFactsOf reads the facts decideSpot decides from: the spot read with a
// spare already going taken off it, and from the line's rows what is coming,
// whether a leg still drops onto it, and the pause.
// resumed is the line's board RESUME stamp (nil when never), one of the things
// that re-arms the pause.
func (e *Engine) spotFactsOf(claim *processes.NodeClaim, read spotRead, rows []domain.Order, resumed *time.Time) spotFacts {
	spot := spotNode(claim)
	f := spotFacts{
		read:   read.lessLeaving(spotLeaving(rows, spot, claim.CoreNodeName)),
		coming: spotComing(rows, claim),
		paused: spotPause(rows, claim, resumed),
	}
	f.drops = e.legDropsOnSpot(rows, claim)
	return f
}

// legDropsOnSpot reports whether a live leg of the line, keep-staged refills and
// returns excepted, will still set a carrier down on the spot: a swap built
// without the spare stages there when the spot is its inbound staging. A plain
// move off the spot is a lift, not a drop. An order whose steps cannot be read
// is judged by its delivery node alone.
func (e *Engine) legDropsOnSpot(rows []domain.Order, claim *processes.NodeClaim) bool {
	spot := spotNode(claim)
	for i := range rows {
		o := &rows[i]
		if ordermgr.IsTerminal(o.Status) || isSpotRefill(o, claim) || isSpotReturn(o, claim) {
			continue
		}
		if o.OrderType == protocol.OrderTypeMove && o.SourceNode == spot {
			continue
		}
		steps, err := e.storedStepsOf(o.ID)
		if err != nil || len(steps) == 0 {
			if o.DeliveryNode == spot {
				return true
			}
			continue
		}
		if drops, _ := spotSteps(steps, spot); drops {
			return true
		}
	}
	return false
}

// isSpotReturn reports whether a line's order is a keep-staged return: a plain
// move off the claim's spot to anywhere but the line.
func isSpotReturn(o *domain.Order, c *processes.NodeClaim) bool {
	spot := spotNode(c)
	return spot != "" && o.OrderType == protocol.OrderTypeMove && o.SourceNode == spot &&
		o.DeliveryNode != c.CoreNodeName
}

// spotPause is the keeper's stop, decided from the rows: the sentence when the
// line's newest keep-staged order (a refill or a return) ended cancelled,
// failed or skipped and nothing has re-armed it since, blank otherwise.
//
// ANY CANCEL STOPS IT. Core fails a plain order for a structural reason
// (congestion waits), so re-creating it each sweep would be a failure on a
// timer, and a cancel is someone saying stop; the Edge does not record who
// cancelled, and does not need to. What re-arms it: a REQUEST or a changeover
// start (each creates an order of the line, read here as newer than the end), a
// save of the claim, or the board's RESUME (resumed, the line's runtime stamp),
// which re-arms without a swap. The pause itself is not stored; it survives a
// restart because the rows do.
func spotPause(rows []domain.Order, c *processes.NodeClaim, resumed *time.Time) string {
	var last *domain.Order
	var rearmed time.Time
	for i := range rows {
		o := &rows[i]
		if (isRetrieve(o.OrderType) && o.DeliveryNode == spotNode(c)) || isSpotReturn(o, c) {
			if last == nil || o.ID > last.ID {
				last = o
			}
			continue
		}
		if o.CreatedAt.After(rearmed) {
			rearmed = o.CreatedAt
		}
	}
	if last == nil {
		return ""
	}
	switch last.Status {
	case ordermgr.StatusCancelled, ordermgr.StatusFailed, ordermgr.StatusSkipped:
	default:
		return ""
	}
	if c.UpdatedAt != nil && c.UpdatedAt.After(rearmed) {
		rearmed = *c.UpdatedAt
	}
	if resumed != nil && resumed.After(rearmed) {
		rearmed = *resumed
	}
	if rearmed.After(last.UpdatedAt) {
		return ""
	}
	what := "refill to"
	if isSpotReturn(last, c) {
		what = "return from"
	}
	return fmt.Sprintf("Keep-staged is paused: the last %s %s was %s. Tap to resume.",
		what, spotNode(c), last.Status)
}

// ResumeKeepStaged is the board's RESUME on a line whose keep-staged keeper a
// cancel paused: it stamps the line's resume and runs the keeper at once, so the
// spot is refilled without a REQUEST and without anything sent to the line.
func (e *Engine) ResumeKeepStaged(nodeID int64) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return fmt.Errorf("node %d: not found", nodeID)
	}
	claim := e.claimAtNode(node)
	if spotNode(claim) == "" {
		return fmt.Errorf("node %s does not keep a spare staged", node.Name)
	}
	if err := processes.ResumeKeepStaged(e.db.DB, node.ID); err != nil {
		return err
	}
	e.logFn("keep-staged: node %s: resumed from the board", node.Name)
	e.keepSpot(node, claim)
	return nil
}

// liftingOrigin is the demand a refill serves: the episode of a live leg of the
// line, when there is one, else none.
func liftingOrigin(rows []domain.Order, c *processes.NodeClaim) ordermgr.Origin {
	for i := range rows {
		o := &rows[i]
		if !ordermgr.IsTerminal(o.Status) && o.OriginID != "" && !isSpotRefill(o, c) && !isSpotReturn(o, c) {
			return ordermgr.Attached(o.OriginID)
		}
	}
	return ordermgr.NoDemand()
}

// setKeepStagedNote records the sentence the board shows for a keep-staged
// line, blank to clear it.
func (e *Engine) setKeepStagedNote(line, note string) {
	if note == "" {
		e.keepStagedNotes.Delete(line)
		return
	}
	e.keepStagedNotes.Store(line, note)
}

// KeepStagedNote is the board's sentence for a keep-staged line (the station
// view's keep_staged_note), blank when there is nothing to say.
func (e *Engine) KeepStagedNote(line string) string {
	if v, ok := e.keepStagedNotes.Load(line); ok {
		return v.(string)
	}
	return ""
}

// kickKeepSpot runs the keeper at once when an order of a keep-staged line ends,
// instead of at the next sweep. A refill a changeover cancel left flying, landing
// on a spot no longer kept for it, goes back first (returnUnwantedLanding): that
// is the cancel finishing its own work, not the keeper deciding a bin.
func (e *Engine) kickKeepSpot(ctx *orderCompletionCtx) {
	o := ctx.order
	if isRetrieve(o.OrderType) && o.DeliveryNode != "" && o.DeliveryNode != ctx.node.CoreNodeName &&
		o.Status == ordermgr.StatusConfirmed {
		e.returnUnwantedLanding(ctx)
	}
	if ctx.toStyleID != 0 {
		return // a changeover is armed: its start and cancel own the spots (see sweepProcessLevels)
	}
	claim := ctx.Claim()
	if claim == nil || spotNode(claim) == "" {
		return
	}
	e.keepSpot(ctx.node, claim)
}

// keepSpotOnPickup runs the keeper when Core reports a lift at a line's spot, so
// the refill behind a lifted spare is ordered at the lift rather than at the
// swap's end. Core moves the bin off the spot before it reports the pickup, so
// the keeper's read already sees the spot bare.
func (e *Engine) keepSpotOnPickup(order *domain.Order, location string) {
	if order == nil || order.ProcessNodeID == nil || location == "" {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil || node == nil {
		return
	}
	claim := e.claimAtNode(node)
	if claim == nil || spotNode(claim) != location {
		return
	}
	if p, perr := e.db.GetProcess(node.ProcessID); perr == nil && p != nil && p.TargetStyleID != nil {
		return // a changeover is armed: its start and cancel own the spots
	}
	e.keepSpot(node, claim)
}

// returnUnwantedLanding sends a refill's bin straight back when it lands on a
// spot nobody keeps it for.
//
// THE CHANGEOVER'S OWN BIN, NOT A JUDGEMENT ABOUT A STRANGER. It acts only on a
// refill this line ordered, judged by what the Edge ordered and not by a read;
// a bin that reached the spot any other way is left where it stands
// (decideSpot). A running line orders refills only for the part it keeps, so in
// practice this is a changeover's refill outliving the changeover's cancel.
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
