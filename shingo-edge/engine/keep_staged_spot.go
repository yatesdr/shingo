package engine

import (
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
// orders this file makes, and reconcileSpot is the only thing that decides them.

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

// reconcileSpot decides what the spot needs. It is pure, and every caller —
// the request path, changeover start, changeover cancel, the save that clears
// the flag and the level keeper's floor — asks it the same question under the
// cell's prime lock:
//
//   - present: a bin stands on the spot (spotRead.occupied, known);
//   - right:   that bin suits this claim by what Edge can see — empty for a
//     produce claim, the claim's part for a consume claim. Edge holds no carrier
//     rule; Core judges bin type at the pickup;
//   - coming:  this line's non-terminal plain orders bound for the spot, for the
//     claim's part and role (spotComing);
//   - consumes: 1 when the plan being applied lifts the spare;
//   - target:  1, or 0 when the claim no longer keeps a spare.
//
// A bare spot with a swap about to consume gives two refills: one the swap
// eats, one to stand after it. Core's dropoff gate serialises their landings.
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

// spareIsRight reports whether the bin on the spot suits the claim as far as
// Edge can tell: a carrier that is there and carries what the claim's spare
// carries (spareCarries). Edge holds no carrier rule; Core judges bin type at
// the pickup.
func spareIsRight(c *processes.NodeClaim, read spotRead) bool {
	return read.occupied && !read.bare && read.payload == spareCarries(c)
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

// planRequestSpot is what a request decides for the spot, for both roles.
// swap is a plan whose swap lifts the spare (its carrier comes from the spot,
// refillPickup); bareLine is a plan that brings one bin to a bare line, which
// takes the spare instead of a market bin when it stands there right. fromSpot
// says the bare-line delivery is sourced from the spot. Pure: the read and the
// count come from the caller.
func planRequestSpot(c *processes.NodeClaim, read spotRead, coming int, swap, bareLine bool) (plan spotPlan, fromSpot bool) {
	if spotNode(c) == "" || !read.known {
		return spotPlan{}, false
	}
	right := spareIsRight(c, read)
	consumes := 0
	switch {
	case swap:
		consumes = 1
	case bareLine && right:
		fromSpot, consumes = true, 1
	}
	return reconcileSpot(c, read.occupied, right, coming, consumes, 1), fromSpot
}

// planSpotForConsume adds the spot's orders to a consume plan. The node-empty
// downgrade's delivery is the bare-line one.
func planSpotForConsume(plan *ConsumePlan, c *processes.NodeClaim, read spotRead, coming int) {
	if plan == nil {
		return
	}
	spot, fromSpot := planRequestSpot(c, read, coming, plan.Dispatch != nil,
		plan.SimpleMove && plan.DowngradedFromSwapMode != "")
	if fromSpot {
		plan.SimpleSource = spotNode(c)
	}
	plan.Spot = spot
}

// planSpotForProduce adds the spot's orders to a produce plan, as on the
// consume side.
func planSpotForProduce(plan *ProducePlan, c *processes.NodeClaim, read spotRead, coming int) {
	if plan == nil {
		return
	}
	spot, fromSpot := planRequestSpot(c, read, coming, plan.Dispatch != nil, plan.SimpleMove)
	if fromSpot {
		plan.SimpleSource, plan.FromSpot = spotNode(c), true
	}
	plan.Spot = spot
}

// applySpotPlan creates the spot's orders: the return move first, so the spot
// can clear, then the refills, which Core's gate holds until it has. Each is
// attributed to the line (process_node_id), never written into a runtime slot
// and never linked to a changeover task; refills auto-confirm, because nobody
// receives a carrier onto a staging node.
//
// A failure here does not fail the request that called it: the swap legs are
// already on their way. It is logged, and the next decision point — the next
// request or the level keeper's floor — re-reads the spot and asks again.
func (e *Engine) applySpotPlan(node *processes.Node, c *processes.NodeClaim, plan spotPlan, read spotRead, origin ordermgr.Origin) {
	if plan.returnSpare {
		e.returnSpare(node, spotNode(c), c.InboundSource, read.payload, origin)
	}
	e.refillSpot(node, c, plan.refills, origin)
	if plan.orders() > 0 {
		e.logFn("keep-staged: node %s spot %s: return=%v refills=%d", node.Name, spotNode(c),
			plan.returnSpare, plan.refills)
	}
}

// returnSpare sends the bin standing on a spot back to a claim's inbound source
// by a plain move that names the bin by what it carries (blank for an empty).
// It is attributed to node, the line whose spare it was.
func (e *Engine) returnSpare(node *processes.Node, spot, source, carried string, origin ordermgr.Origin) {
	nodeID := node.ID
	if _, err := e.orderMgr.CreateMoveOrderCarrying(&nodeID, spot, source, carried, origin); err != nil {
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

// spotOf reads the spot's row out of a node-bins answer.
func spotOf(c *processes.NodeClaim, rows []NodeBinInfo, nodeKnown func(string) bool) spotRead {
	spot := spotNode(c)
	if spot == "" || !nodeKnown(spot) {
		return spotRead{}
	}
	for _, b := range rows {
		if b.NodeName == spot {
			return spotRead{known: true, occupied: b.Occupied, payload: b.PayloadCode, bare: b.Bare}
		}
	}
	return spotRead{}
}
