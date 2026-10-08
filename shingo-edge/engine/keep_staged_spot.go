package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/processes"
)

// keep_staged_spot.go — what a keep-staged spot needs, decided in one place.
//
// A keep-staged claim keeps one spare on its spot, and its swap fetches its
// carrier from there instead of from the inbound source (refillPickup), in
// every swap mode. Nothing about the spot is stored on Edge: what
// stands there is Core's answer, read in the occupancy call the request already
// makes, and what is coming is Edge's own order rows. Every arrival at the spot
// is a plain order — a retrieve from the inbound source, or nothing — and a
// wrong spare leaves by a plain move back to that source. Those are the only
// orders this file makes, and decideSpot is the only thing that decides them for
// a running line (a changeover's start and cancel ask reconcileSpot directly).

// spotNode is the claim's keep-staged node (its spot), blank when it keeps no
// spare. Nil-safe, for the callers that hold an optional claim.
func spotNode(c *processes.NodeClaim) string {
	if c == nil {
		return ""
	}
	return c.KeepStagedNode
}

// carriesEmpty reports whether the claim's carrier travels empty: a produce
// line is fed empties to fill, a consume line fulls of its part. It is the one
// thing about the role the spot asks, so what the spare carries is taken from
// the claim in one place: an unstamped empty, or the claim's part.
func carriesEmpty(c *processes.NodeClaim) bool {
	return c.Role == protocol.ClaimRoleProduce
}

// spareCarries is what the right spare on a claim's spot carries: blank for an
// empty, the claim's part for a full.
func spareCarries(c *processes.NodeClaim) string {
	if carriesEmpty(c) {
		return ""
	}
	return c.PayloadCode
}

// spotRead is Core's answer for a keep-staged claim's spot.
type spotRead struct {
	// known is false when Core did not answer for the spot, or answered for a
	// node it does not have. Nothing is decided on an unknown: ordering on one is
	// how a spot gets two spares.
	known    bool
	occupied bool
	// payload is what the bin standing there carries; blank for an empty.
	payload string
	bare    bool
	// binType is the carrier type of the bin standing there, as Core names it.
	binType string
	// binID is Core's id for the bin standing there; 0 when none was named. A
	// return of the spare names it, so the return lifts that bin or nothing.
	binID int64
	// catalog is the part-to-carrier list Core sent with the node list, in
	// force when the spot was read; nil before the first one arrives. An empty
	// is judged against it (spareIsRight).
	catalog []protocol.PayloadBinTypeInfo
	// sourceBusy is true when Core answered for the claim's inbound source, the
	// node a wrong spare is returned to, and a bin stands there. A return into
	// it queues behind that bin; when that bin is what the line's own swap is
	// waiting to fetch, the two wait on each other (SPR ALN_011 2026-10-08).
	sourceBusy bool
}

// readOfRow is a spot's read from its row in a node-bins answer, with the
// catalog an empty is judged against. Every decision point reads the spot
// through it.
func readOfRow(b NodeBinInfo, catalog []protocol.PayloadBinTypeInfo) spotRead {
	return spotRead{known: true, occupied: b.Occupied, payload: b.PayloadCode, bare: b.Bare,
		binType: b.BinTypeCode, binID: b.BinID, catalog: catalog}
}

// spotFacts is everything the keeper decides a spot from, read before deciding
// so the decision itself is pure (decideSpot).
type spotFacts struct {
	// read is Core's answer for the spot, with a spare that already has a live
	// return taken off it (lessLeaving).
	read spotRead
	// coming is the line's live refills to the spot (spotComing).
	coming int
	// lifts is the spares still owed to the line's own legs: live legs, still
	// acquiring at Core, whose steps pick up at the spot. Read from the legs'
	// steps, never assumed from a swap being in flight. A leg already with the
	// fleet is not counted: Core moves a lifted bin off the spot before it
	// reports the pickup, so after the lift the spot reads bare and the refill
	// follows; before it, the spare still stands and is counted as standing.
	lifts int
	// drops is true when a live leg of the line will still drop onto the spot:
	// a swap planned before the spot was kept, staging its own carrier there.
	drops bool
	// paused is the sentence for a keeper that stopped, blank when it has not:
	// the line's last keep-staged order ended cancelled, failed or skipped, and
	// nothing has re-armed it since (spotPause).
	paused string
}

// decideSpot is THE decision for a keep-staged spot, and every decision point
// asks it: a REQUEST as it creates the swap, the level sweep (idle lines
// included), and the end of any order to or from the spot. The rule is one
// right spare standing, or on its way, for each spare the line's legs still owe
// plus the one that stands after them, with at most one refill in flight. It
// returns the orders to make and the sentence the board shows for the spot,
// blank when there is nothing to say.
func decideSpot(c *processes.NodeClaim, f spotFacts) (spotPlan, string) {
	spot := spotNode(c)
	if spot == "" || !f.read.known {
		return spotPlan{}, ""
	}
	if f.paused != "" {
		return spotPlan{}, f.paused
	}
	if f.drops {
		return spotPlan{}, "Keep-staged waits: an order of this line is still bringing a bin to " + spot + "."
	}
	right := spareIsRight(c, f.read)
	plan := reconcileSpot(c, f.read.occupied, right, f.coming, f.lifts, 1)
	if room := 1 - f.coming; plan.refills > room {
		plan.refills = max(room, 0)
	}
	if plan.returnSpare && f.read.sourceBusy {
		return spotPlan{}, fmt.Sprintf("Keep-staged is stuck: the bin on %s is not a spare for %s, and %s is "+
			"occupied, so it cannot go back. Move it by hand.", spot, c.PayloadCode, c.InboundSource)
	}
	return plan, ""
}

// spotPlan is the orders one decision point makes for the spot.
type spotPlan struct {
	// returnSpare sends the bin standing on the spot back to the claim's
	// inbound source, by a plain move.
	returnSpare bool
	// refills is how many plain retrieves to the spot to create.
	refills int
}

// orders is how many orders the plan creates.
func (p spotPlan) orders() int {
	n := p.refills
	if p.returnSpare {
		n++
	}
	return n
}

// reconcileSpot is the arithmetic of what the spot needs. It is pure, and every
// caller asks it the same question under the cell's prime lock: decideSpot (the
// request, the sweep and an order's end), changeover start and cancel, and the
// save that clears the flag:
//
//   - present: a bin stands on the spot (spotRead.occupied, known);
//   - right:   that bin suits this claim (spareIsRight): a full by its part,
//     an empty by its carrier, asked as Core asks it at the pickup;
//   - coming:  this line's non-terminal plain orders bound for the spot, for the
//     claim's part and role (spotComing);
//   - consumes: 1 when the plan being applied lifts the spare;
//   - target:  1, or 0 when the claim no longer keeps a spare.
//
// A bare spot with a swap about to consume asks for two: one the swap eats, one
// to stand after it. decideSpot sends the first and the second once it lands,
// never two in flight.
func reconcileSpot(c *processes.NodeClaim, present, right bool, coming, consumes, target int) spotPlan {
	if spotNode(c) == "" {
		return spotPlan{}
	}
	plan := spotPlan{returnSpare: present && (!right || target == 0)}
	standing := 0
	if present && right {
		standing = 1
	}
	if n := target + consumes - coming - standing; n > 0 {
		plan.refills = n
	}
	return plan
}

// spareIsRight reports whether the bin on the spot suits the claim: a carrier
// that is there and carries what the claim's spare carries (spareCarries). A
// full is judged by its part. An empty is judged by its carrier as well: one
// the claim's part may ride, by Core's own rule over the catalog Core sent
// (partPermitsCarrier), so the Edge sends back exactly the empties Core's
// pickup would refuse. The one judgement for every decision point: the
// request, the level keeper's sweep, a changeover's start and its cancel.
// Nothing about who left the bin there, or when, is asked.
func spareIsRight(c *processes.NodeClaim, read spotRead) bool {
	if !read.occupied || read.bare || read.payload != spareCarries(c) {
		return false
	}
	return !carriesEmpty(c) || partPermitsCarrier(read.catalog, c.PayloadCode, read.binType)
}

// isSpotRefill reports whether a line's order is a keep-staged refill: a plain
// order bound for the claim's spot that never touches the line. It does not
// work the cell, so it neither refuses the line's REQUEST nor silences the
// level keeper; everything else bound anywhere near the line still does.
func isSpotRefill(o *domain.Order, c *processes.NodeClaim) bool {
	spot := spotNode(c)
	return spot != "" && o.DeliveryNode == spot && o.SourceNode != c.CoreNodeName &&
		(isRetrieve(o.OrderType) || o.OrderType == protocol.OrderTypeMove)
}

// isRetrieve is either spelling of a retrieve. Edge writes an empty-in (a
// produce refill) as retrieve with retrieve_empty set; Core promotes it to
// retrieve_empty, and its projection overwrites the row's order_type.
func isRetrieve(t protocol.OrderType) bool {
	return t == protocol.OrderTypeRetrieve || t == protocol.OrderTypeRetrieveEmpty
}

// worksTheCell is orderWorksTheCell for the line a claim governs, with a
// keep-staged claim's refills excused.
func worksTheCell(o *domain.Order, c *processes.NodeClaim) bool {
	return orderWorksTheCell(o) && !isSpotRefill(o, c)
}

// spotComing counts what is on its way to the spot from the line's rows: plain
// orders bound for the spot, for the claim's part, in the claim's role. Matching
// part and role is what keeps a refill for the outgoing style from counting for
// the incoming one.
func spotComing(rows []domain.Order, c *processes.NodeClaim) int {
	n := 0
	for i := range rows {
		o := &rows[i]
		if !isSpotRefill(o, c) || ordermgr.IsTerminal(o.Status) {
			continue
		}
		if o.PayloadCode == c.PayloadCode && o.RetrieveEmpty == carriesEmpty(c) {
			n++
		}
	}
	return n
}

// spotLeaving counts the line's live returns from a spot: plain moves off the
// spot to anywhere but the line. A spare with one of those is already going.
func spotLeaving(rows []domain.Order, spot, line string) int {
	n := 0
	for i := range rows {
		o := &rows[i]
		if o.OrderType == protocol.OrderTypeMove && o.SourceNode == spot && o.DeliveryNode != line &&
			!ordermgr.IsTerminal(o.Status) {
			n++
		}
	}
	return n
}

// lessLeaving is the read with a spare that is already going taken off it: a
// spot whose bin has a live return is decided as bare. Counted as standing, the
// spare would be kept and nothing ordered behind it, and the return would leave
// the spot empty; judged again, it would be sent back a second time.
func (r spotRead) lessLeaving(leaving int) spotRead {
	if leaving > 0 && r.known {
		return spotRead{known: true}
	}
	return r
}

// planRequestSpot is what a request decides for the spot, for both roles: the
// keeper's decision (decideSpot) over the line as it stands, plus the legs this
// request is about to create. dispatch is the swap's two step lists, read for a
// pickup at the spot like any live leg's; bareLine is a plan that brings one bin
// to a bare line, which takes the spare instead of a market bin when it stands
// there right. fromSpot says the bare-line delivery is sourced from the spot. A
// request is not subject to the keeper's pause: pressing REQUEST is what
// re-arms it. Pure: the facts come from the caller.
func planRequestSpot(c *processes.NodeClaim, f spotFacts, dispatch [][]protocol.ComplexOrderStep, bareLine bool) (plan spotPlan, fromSpot bool, note string) {
	if spotNode(c) == "" || !f.read.known {
		return spotPlan{}, false, ""
	}
	f.paused = ""
	for _, steps := range dispatch {
		if _, lifts := spotSteps(steps, spotNode(c)); lifts {
			f.lifts++
		}
	}
	if bareLine && spareIsRight(c, f.read) {
		fromSpot = true
		f.lifts++
	}
	plan, note = decideSpot(c, f)
	return plan, fromSpot, note
}

// spotSteps reports whether a leg's steps drop onto the spot, and whether they
// pick up from it.
func spotSteps(steps []protocol.ComplexOrderStep, spot string) (drops, lifts bool) {
	for _, s := range steps {
		if s.Node != spot {
			continue
		}
		switch s.Action {
		case protocol.ActionDropoff:
			drops = true
		case protocol.ActionPickup:
			lifts = true
		}
	}
	return drops, lifts
}

// swapLegsOf is the step lists of every leg a swap will run: its one or two
// dispatched legs, and for sequential the backfill the wiring creates once the
// removal is done (handleSequentialBackfill), which is the leg that lifts the
// spare. Read so the spare it will take is counted now, at the request.
func swapLegsOf(d *SwapDispatch, c *processes.NodeClaim) [][]protocol.ComplexOrderStep {
	legs := [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB}
	if d.CycleMode == protocol.SwapModeSequential {
		legs = append(legs, BuildSequentialBackfillSteps(c))
	}
	return legs
}

// planSpotForConsume adds the spot's orders to a consume plan. The node-empty
// downgrade's delivery is the bare-line one.
func planSpotForConsume(plan *ConsumePlan, c *processes.NodeClaim, f spotFacts) string {
	if plan == nil {
		return ""
	}
	var dispatch [][]protocol.ComplexOrderStep
	if plan.Dispatch != nil {
		dispatch = swapLegsOf(plan.Dispatch, c)
	}
	spot, fromSpot, note := planRequestSpot(c, f, dispatch, plan.SimpleMove && plan.DowngradedFromSwapMode != "")
	if fromSpot {
		plan.SimpleSource = spotNode(c)
	}
	plan.Spot = spot
	return note
}

// planSpotForProduce adds the spot's orders to a produce plan, as on the
// consume side.
func planSpotForProduce(plan *ProducePlan, c *processes.NodeClaim, f spotFacts) string {
	if plan == nil {
		return ""
	}
	var dispatch [][]protocol.ComplexOrderStep
	if plan.Dispatch != nil {
		dispatch = swapLegsOf(plan.Dispatch, c)
	}
	spot, fromSpot, note := planRequestSpot(c, f, dispatch, plan.SimpleMove)
	if fromSpot {
		plan.SimpleSource, plan.FromSpot = spotNode(c), true
	}
	plan.Spot = spot
	return note
}

// applySpotPlan creates the spot's orders: the return move first, so the spot
// can clear, then the refills, which Core's gate holds until it has. Each is
// attributed to the line (process_node_id), never written into a runtime slot
// and never linked to a changeover task; refills auto-confirm, because nobody
// receives a carrier onto a staging node.
//
// A failure here does not fail the request that called it: the swap legs are
// already on their way. It is logged, and the next decision point — the next
// request, the sweep's keeper, or the next order of the line to end — re-reads
// the spot and asks again.
func (e *Engine) applySpotPlan(node *processes.Node, c *processes.NodeClaim, plan spotPlan, read spotRead, origin ordermgr.Origin) {
	if plan.returnSpare {
		e.returnSpare(node, spotNode(c), c.InboundSource, read.payload, read.binID, origin)
	}
	e.refillSpot(node, c, plan.refills, origin)
	if plan.orders() > 0 {
		e.logFn("keep-staged: node %s spot %s: return=%v refills=%d", node.Name, spotNode(c),
			plan.returnSpare, plan.refills)
	}
}

// returnSpare sends the bin standing on a spot back to a claim's inbound source
// by a plain move that says what it carries (blank for an empty) and names the
// bin: binID, as the read that decided the return saw it. Core lifts that bin
// or nothing, so a return still waiting when something else lifts the spare
// does not carry away the refill that lands after it. 0 names no bin, and Core
// takes whatever stands there. Attributed to node, the line whose spare it was.
func (e *Engine) returnSpare(node *processes.Node, spot, source, carried string, binID int64, origin ordermgr.Origin) {
	nodeID := node.ID
	if _, err := e.orderMgr.CreateMoveOrderForBin(&nodeID, spot, source, carried, binID, origin); err != nil {
		e.logFn("keep-staged: node %s: return the spare on %s to %s: %v", node.Name, spot, source, err)
	}
}

// refillSpot sends n plain retrieves from the claim's inbound source to its
// spot, for what the claim's carrier carries (carriesEmpty). Attributed to the
// claim's line and auto-confirmed.
func (e *Engine) refillSpot(node *processes.Node, c *processes.NodeClaim, n int, origin ordermgr.Origin) {
	nodeID := node.ID
	for i := 0; i < n; i++ {
		if _, err := e.orderMgr.CreateRetrieveOrder(&nodeID, carriesEmpty(c), 1,
			spotNode(c), c.InboundSource, "", "standard", c.PayloadCode, true, false, origin); err != nil {
			e.logFn("keep-staged: node %s: refill %d/%d for %s: %v", node.Name, i+1, n, spotNode(c), err)
		}
	}
}

// spotOf reads the spot's row out of a node-bins answer, with the catalog its
// empty is judged against.
func spotOf(c *processes.NodeClaim, rows []NodeBinInfo, nodeKnown func(string) bool, catalog []protocol.PayloadBinTypeInfo) spotRead {
	spot := spotNode(c)
	if spot == "" || !nodeKnown(spot) {
		return spotRead{}
	}
	var read spotRead
	busy := false
	for _, b := range rows {
		switch b.NodeName {
		case spot:
			read = readOfRow(b, catalog)
		case c.InboundSource:
			busy = b.Occupied && nodeKnown(b.NodeName)
		}
	}
	if read.known {
		read.sourceBusy = busy
	}
	return read
}
