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
// A keep-staged claim keeps one spare on its spot. A REQUEST's swap fetches its
// carrier from there when a right spare stands on it (refillPickup), and from
// the inbound source otherwise, exactly as a line without a spot does
// (requestClaim). Nothing about the spot is stored on Edge: what stands there is
// Core's answer, read in the occupancy call the request already makes, and what
// is coming is Edge's own order rows. Every arrival at the spot is a plain order
// — a retrieve from the inbound source, or nothing — and decideSpot, the
// keeper's, is the only thing that decides them for a running line. A REQUEST
// makes none: it asks only for the line.
//
// A RUNNING LINE NEVER MOVES A BIN OFF ITS SPOT. A bin standing there that is
// not a spare is left where it is, and the board says so; a swap that stages
// its carrier on the spot is held by Core's staging check (waiting for slot,
// dropoff occupied) until a person moves it, as on any line whose staging node
// is occupied. Only a changeover's start and cancel, and the save that clears
// the flag, send a spare back (reconcileSpot): those change what the spot is
// kept for. So does a refill a cancel left flying, when it lands
// (returnUnwantedLanding): that is the cancel's own bin.

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
	// drops is true when a live leg of the line will still drop onto the spot:
	// a swap planned before the spot was kept, staging its own carrier there.
	drops bool
	// paused is the sentence for a keeper that stopped, blank when it has not:
	// the line's last keep-staged order ended cancelled, failed or skipped, and
	// nothing has re-armed it since (spotPause).
	paused string
}

// decideSpot is THE decision for a keep-staged spot, and the keeper is its only
// caller: the level sweep (idle lines included), a lift at the spot, and the end
// of any order of the line. The rule is one right spare standing or on its way,
// with at most one refill in flight, and only onto a spot that is bare.
//
// A WRONG BIN IS LEFT WHERE IT STANDS. A refill cannot be set down on it, and
// sending it somewhere is a decision about a bin the Edge did not put there; the
// board names it for a person to move, and the next pass after it has gone
// refills the spot.
//
// NOTHING IS ORDERED AHEAD OF A LIFT. A spare a swap is about to take still
// stands, and counts as standing; the refill behind it is ordered when it is
// lifted (keepSpotOnPickup), so a swap cancelled before its lift leaves no
// refill queued behind a spare that never moved. It returns the orders to make
// and the sentence the board shows for the spot, blank when there is nothing to
// say.
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
	if f.read.occupied && !right {
		return spotPlan{}, fmt.Sprintf("Keep-staged waits: the bin on %s is not a spare for %s. Move it by hand.",
			spot, c.PayloadCode)
	}
	plan := reconcileSpot(c, f.read.occupied, right, f.coming, 0, 1)
	if room := 1 - f.coming; plan.refills > room {
		plan.refills = max(room, 0)
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
// keeper: the sweep, a lift at the spot and an order's end), changeover start
// and cancel, and the save that clears the flag:
//
//   - present: a bin stands on the spot (spotRead.occupied, known);
//   - right:   that bin suits this claim (spareIsRight): a full by its part,
//     an empty by its carrier, asked as Core asks it at the pickup;
//   - coming:  this line's non-terminal plain orders bound for the spot, for the
//     claim's part and role (spotComing);
//   - consumes: 1 when the plan being applied lifts the spare — a changeover's
//     legs, planned and created together; the keeper passes 0, because it
//     orders behind a lift only once the lift has happened;
//   - target:  1, or 0 when the claim no longer keeps a spare.
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

// spotAtRequest is what a REQUEST knows about a keep-staged claim's spot: Core's
// read of it, and from the line's rows whether its spare is leaving and how many
// refills are on their way to it.
type spotAtRequest struct {
	read    spotRead
	leaving bool
	coming  int
}

// takesSpare says the request takes the spare standing on the spot: Core says a
// right one stands there (spareIsRight) and no return of the line is taking it.
func (s spotAtRequest) takesSpare(c *processes.NodeClaim) bool {
	return s.read.known && spareIsRight(c, s.read) && !s.leaving
}

// readSpotAtRequest reads the line's rows for a keep-staged claim whose spot Core
// answered for; nothing for any other claim. It fails closed: a request that
// cannot tell what is leaving or coming takes neither the spare nor a market
// carrier, and the next request re-asks.
func (e *Engine) readSpotAtRequest(node *processes.Node, claim *processes.NodeClaim, read spotRead) (spotAtRequest, error) {
	s := spotAtRequest{read: read}
	if spotNode(claim) == "" || !read.known {
		return s, nil
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		return s, fmt.Errorf("node %s: cannot tell what is leaving or coming to %s (%w) — the next request will re-ask",
			node.Name, spotNode(claim), err)
	}
	s.leaving = spotLeaving(rows, spotNode(claim), claim.CoreNodeName) > 0
	s.coming = spotComing(rows, claim)
	return s, nil
}

// requestClaim is the claim a REQUEST builds its plan from, for both roles, and
// whether that plan takes the spare. swapClaim is the one the request would build
// from (its evac destination already adjusted).
//
// THE SPARE WHEN IT STANDS, THE MARKET WHEN IT DOES NOT. When the request takes
// the spare (takesSpare), the swap is built from swapClaim, so every leg that
// fetches a carrier fetches it from the spot (refillPickup). Otherwise it is
// built from swapClaim without its spot, which is the swap a line with no spot
// runs: its carrier comes from the inbound source. A REQUEST orders nothing for
// the spot either way; the keeper refills it when it reads it bare (decideSpot).
// takesComingSpare then asks whether that market swap should wait for a spare
// already on its way instead.
func requestClaim(claim, swapClaim *processes.NodeClaim, s spotAtRequest) (*processes.NodeClaim, bool) {
	if spotNode(claim) == "" {
		return swapClaim, false
	}
	if s.takesSpare(claim) {
		return swapClaim, true
	}
	return withoutSpot(swapClaim), false
}

// rearmKeepStaged is a REQUEST re-arming its line's keeper: pressing REQUEST is
// a person saying go, as the board's RESUME is, so it writes the same resume
// stamp (spotPause). Written once the request has passed every refusal, so a
// refused REQUEST re-arms nothing. A failed write is logged and does not fail
// the request: the swap is what the operator asked for, and the next REQUEST or
// a RESUME stamps again.
func (e *Engine) rearmKeepStaged(node *processes.Node, claim *processes.NodeClaim) {
	if spotNode(claim) == "" {
		return
	}
	if err := processes.ResumeKeepStaged(e.db.DB, node.ID); err != nil {
		e.logFn("keep-staged: node %s: re-arm the keeper on REQUEST: %v", node.Name, err)
	}
}

// withoutSpot is a copy of a claim that keeps no spare: the claim a REQUEST
// builds from when it does not take one (requestClaim).
func withoutSpot(c *processes.NodeClaim) *processes.NodeClaim {
	cp := *c
	cp.KeepStagedNode = ""
	return &cp
}

// takesComingSpare says a swap built without the spare should take the spare
// that is coming instead. A keep-staged claim may name its spot as its inbound
// staging, and the market swap stages its carrier there: with a refill about to
// land on it, the carrier and the refill could not both be set down, and the
// keeper waits for a leg that drops on the spot, so the two would hold each
// other. Taking the coming spare orders nothing new; Core holds the pickup until
// it lands. A swap that does not stage on the spot fetches from the market.
//
// NOT A REFUSAL FOR A BIN STANDING THERE. A market swap staging on a spot a bin
// still holds is the case every line has when its staging node is occupied:
// Core holds it (waiting for slot, dropoff occupied) and sends it when the spot
// clears.
func takesComingSpare(claim *processes.NodeClaim, s spotAtRequest, d *SwapDispatch) bool {
	if d == nil || spotNode(claim) == "" || !s.read.known || s.coming == 0 {
		return false
	}
	for _, steps := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
		if drops, _ := spotSteps(steps, spotNode(claim)); drops {
			return true
		}
	}
	return false
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

// applySpotPlan creates the keeper's refills. Each is attributed to the line
// (process_node_id), never written into a runtime slot and never linked to a
// changeover task, and auto-confirms, because nobody receives a carrier onto a
// staging node. The keeper's plan never returns a bin (decideSpot).
//
// A failure here is logged, and the next decision point — the sweep's keeper,
// a lift at the spot, or the next order of the line to end — re-reads
// the spot and asks again.
func (e *Engine) applySpotPlan(node *processes.Node, c *processes.NodeClaim, plan spotPlan, origin ordermgr.Origin) {
	e.refillSpot(node, c, plan.refills, origin)
	if plan.refills > 0 {
		e.logFn("keep-staged: node %s spot %s: refills=%d", node.Name, spotNode(c), plan.refills)
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
	for _, b := range rows {
		if b.NodeName == spot {
			return readOfRow(b, catalog)
		}
	}
	return spotRead{}
}
