// wiring_completion.go — Order delivery and completion handling.
//
// handleOrderDelivered runs when fleet reports FINISHED. It sends the
// delivered notification and moves the order's carts to their drops
// immediately so telemetry is accurate. handleOrderCompleted runs when
// Edge confirms receipt and advances compound orders. Both settle the
// order's carts, read with the order (settleCarts / completionNet): one
// arrival for one cart or several; the completion path is idempotent
// (skips carts already at their drops).

package engine

import (
	"fmt"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/domain"
	"shingocore/service"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// handleOrderDelivered runs on fleet-reported FINISHED. Notifies Edge
// and moves the bin to its destination immediately so subsequent orders
// see accurate occupancy.
func (e *Engine) handleOrderDelivered(order *orders.Order) {
	// Apply bin arrival FIRST so telemetry is accurate immediately. The
	// previous order — sendToEdge then applyBinArrivalForOrder — let
	// AutoConfirm Edge orders auto-confirm before the bin-arrival
	// commit landed.
	//
	// ── FAIL LOUD, AND STOP ───────────────────────────────────────────────
	// A refusal here means the robot arrived carrying something the ledger says
	// belongs to someone else, and Core cannot tell what is actually on the deck.
	// That is an integrity fault, not congestion, so wait-not-fail does not cover
	// it: standing law 1's own carve-out is that genuine faults fail loud with a
	// named message.
	//
	// THE LICENCE IS THE EVIDENCE, not an argument. The instrument read 121, then
	// 2, then 1 as three successive extraction errors were corrected, and every
	// surviving specimen was explained benign — the last being a terminal order
	// whose bin had moved on, closed by the discriminator in arrival_guard.go. A
	// refusal that survives all four cuts is a state nothing in the claim
	// lifecycle should be able to produce, so the right response to seeing one is
	// to stop and say so.
	//
	// Parking was the alternative and it loses on a fact discovered while
	// building: Core does not know what the robot is holding. Parking keeps an
	// order alive whose payload is unidentifiable, still holding a runtime slot —
	// the dead-robot wedge — which is worse than a loud failure.
	//
	// IT RETURNS rather than falling through, because the rest of this function
	// tells Edge the order was DELIVERED. Failing the order and then announcing
	// its delivery in the same breath is the lie this whole thread has been
	// unwinding.
	//
	// failOrderAndEmit, not a bare FailOrderAtomic: it routes through
	// Lifecycle().Fail and fires EventOrderFailed, so the failure lands in the
	// audit trail and reaches the station like every other failure. It also
	// releases the order's bin claims, which is correct here — the bin it thought
	// it held is demonstrably not its own.
	if refusal := e.applyBinArrivalForOrder(order); refusal != nil {
		claimant := "nobody"
		if refusal.ClaimedBy != nil {
			claimant = fmt.Sprintf("order %d", *refusal.ClaimedBy)
		}
		detail := fmt.Sprintf("cargo does not match the ledger: bin %d is claimed by %s, not by this "+
			"order (%s). Core cannot identify what the robot is carrying, so the delivery is not "+
			"recorded and the order is failed rather than reported delivered.",
			refusal.BinID, claimant, refusal.Context())
		e.logFn("FAIL: order=%d refused at %s — %s", order.ID, refusal.Site, detail)
		e.failOrderAndEmit(order.ID, "cargo_ledger_mismatch", detail)
		return
	}

	// Ship the cart so Edge can attribute PLC tick deltas to the right one: the
	// order's one cart, or, for an order that carries several, the cart its plan
	// leaves at the consuming (process) node (F1b, selectConsumingCart). Edge's
	// bin-ownership flip means active_bin_id at the runtime row is sourced from
	// this envelope. Read from the carts the order was loaded with: no statement.
	binID, binDestNode, multiBin := deliveredCart(order)
	if binID == nil && multiBin {
		e.logFn("engine: order=%d multi-bin delivered: no cart set down last at process node %q — Edge multi-bin backstop alarm will fire (F1b no-match)",
			order.ID, order.ProcessNode)
	}

	// Diagnostic: an order that carries no cart at delivery ships none, so a
	// future "Edge isn't tracking ticks" investigation can grep for the cause.
	if binID == nil && !multiBin {
		e.logFn("engine: order=%d type=%s shipped order.delivered without bin_id (the order carries no cart at delivery — Edge tick attribution will be silent until next order)",
			order.ID, order.OrderType)
	}

	// Snapshot the just-arrived bin's authoritative count + load-lifecycle
	// epoch onto the envelope so Edge seeds its runtime cache and stamps
	// outgoing BinUOPDeltas from these — no separate HTTP pull. Read AFTER
	// applyBinArrivalForOrder above so the bin row reflects the arrival.
	// Reads whichever bin was selected above: order.BinID for single-bin, or
	// the consuming-node bin for multi-tote (F1b). binID nil (unmatched
	// multi-bin) leaves the snapshot empty and Edge falls back to its default.
	var uopRemaining *int
	var deltaEpoch int64
	var binPayloadCode *string
	if binID != nil {
		if bin, binErr := e.db.GetBin(*binID); binErr == nil {
			u := bin.UOPRemaining
			uopRemaining = &u
			deltaEpoch = bin.DeltaEpoch
			// The payload rode this row the whole time and was dropped on the
			// line above. The Edge has no bins table and cannot look it up, so
			// it was inferring the carrier's identity from the requested
			// style's claim — right until a changeover moved the cell on
			// underneath a carrier that stayed.
			p := bin.PayloadCode
			binPayloadCode = &p
		} else {
			e.logFn("engine: order=%d delivered: bin %d uop/epoch lookup failed: %v — the envelope goes out with NO count and the Edge will seat 0 for it",
				order.ID, *binID, binErr)
		}
	}

	// A Core-born order that projects to nobody is heard by no station.
	stationID, ok := materialStation(order)
	if !ok {
		return
	}
	// Core-admin (manual move) orders have no station; broadcast to all edges so
	// each can attempt the bin binding via DeliveryNode fallback.
	if stationID == "" {
		stationID = protocol.StationBroadcast
	}
	if err := e.sendToEdge(protocol.TypeOrderDelivered, stationID, &protocol.OrderDelivered{
		OrderUUID:   order.EdgeUUID,
		DeliveredAt: clock.Now().UTC(),
		// This Core announces every carrier's drop on transit.bin_landed, in
		// the arrival's transaction (applyArrival above), so a new Edge binds
		// nothing from this message (H.5, H.7). The bind fields below stay one
		// release for an Edge too old for the landing; they go, with this
		// flag, on the cleanup sweep row.
		LandingSent: true,
		// StagedExpireAt stays unset: staging no longer expires
		// (resolveNodeStaging). The field stays on the wire for older Edges.
		BinID:          binID,
		UOPRemaining:   uopRemaining,
		DeltaEpoch:     deltaEpoch,
		BinPayloadCode: binPayloadCode,
		DeliveryNode:   order.DeliveryNode,
		BinDestNode:    binDestNode,
	}); err != nil {
		e.logFn("engine: delivered notification: %v", err)
	}
}

// materialStation is the station an order's material notices (order.delivered,
// bin_picked_up) go to, and false when they go to nobody. A station-known order
// sends to its own station, as it always did; a station-unaware one sends to the
// station it projects to, if any (the orc's ruling on Stop 1's delivery line).
// The order is already read, so this adds no statement.
func materialStation(order *orders.Order) (string, bool) {
	if !order.StationUnaware {
		return order.StationID, true
	}
	return order.ProjectsTo, order.ProjectsTo != ""
}

// deliveredCart is the cart order.delivered ships: the order's one cart, or for
// an order that carries several the one its plan sets down last at the
// consuming (process) node (selectConsumingCart). dest is that cart's drop node,
// shipped only for an order of several carts, as before. multi reports an order
// of several carts.
func deliveredCart(order *orders.Order) (bin *int64, dest string, multi bool) {
	finals := order.FinalDrops()
	switch len(finals) {
	case 0:
		return nil, "", false
	case 1:
		b := finals[0].BinID
		return &b, "", false
	}
	if c := selectConsumingCart(order, finals); c != nil {
		b := c.BinID
		return &b, domain.StepNode(order, *c.DropStep), true
	}
	return nil, "", true
}

// selectConsumingCart returns the cart the order leaves at its consuming
// (process) node — the cart that stays at the line and receives consume ticks.
// For a swap this is the SUPPLY cart set down at the line, NOT the one lifted
// off it. Its drop is the plan's (domain.StepNode), compared as the Edge
// delivery gate compares (dest == CoreNodeName).
//
// Returns nil when processNode is empty or no cart's last drop is there, so the
// caller ships no bin and the Edge multi-bin backstop alarm (P2-C3) fires rather
// than binding a guess.
func selectConsumingCart(order *orders.Order, finals []domain.Cart) *domain.Cart {
	if order.ProcessNode == "" {
		return nil
	}
	for i := range finals {
		if domain.StepNode(order, *finals[i].DropStep) == order.ProcessNode {
			return &finals[i]
		}
	}
	return nil
}

// applyBinArrivalForOrder moves the order's carts to where its plan sets each
// one down last. Called from handleOrderDelivered (on fleet FINISHED) so that
// telemetry is accurate immediately. handleOrderCompleted still runs on
// confirmation but is idempotent — it skips a cart already at its drop. It
// returns the first refusal when the claim guard declines a placement, so the
// completion path can see that this order did not deliver what it says it did
// (arrival_guard.go).
//
// ONE ARRIVAL, ONE CART OR SEVERAL. The carts are the order's rows, read with
// the order (orders.SelectCols): a single-cart order is the one-row case of the
// same settle, not a second path with a second spelling of "which cart goes
// where". An order whose carts could not be read never got here: its read
// failed, and the settle fails closed with it.
func (e *Engine) applyBinArrivalForOrder(order *orders.Order) *ArrivalRefusal {
	if order.SourceNode == "" || order.DeliveryNode == "" {
		// Bin-stuck-at-source diagnostic: previously a silent skip. Move-order
		// post-mortem 2026-04-28 traced "delivered but bin still at source"
		// scenarios that left no log line at all.
		e.logFn("delivery: order=%d type=%s carts=%d skipped arrival: missing source/delivery (source=%q delivery=%q)",
			order.ID, order.OrderType, len(order.Carts), order.SourceNode, order.DeliveryNode)
		return nil
	}

	// Release the order's destination-slot claims now that its bins have
	// arrived — the dispatch-time ClaimSlot has served its purpose. Without this
	// the happy path leaked slot claims until the terminal transition (the
	// TerminalizeOrder backstop at 'confirmed'), so a slot could stay
	// un-reclaimable in the Delivered→Confirmed window after its bin moved on.
	if err := e.db.UnclaimOrderSlots(order.ID); err != nil {
		e.logFn("engine: release slot claims for order %d on arrival: %v", order.ID, err)
	}

	finals := order.FinalDrops()
	if len(finals) == 0 {
		// SHADOWED: an order with no carts is a coordinator (it owns legs, and
		// carries nothing) or a defect (a door that held a cart wrote no row).
		// See service.NoteFolderShadow.
		owns, oerr := e.db.OrderOwnsNoCargo(order.ID)
		service.NoteFolderShadow(service.FolderSiteDeliverySettle, order.ID, owns, oerr)
		e.logFn("delivery: order=%d type=%s skipped arrival: the order carries no cart (source=%s delivery=%s)",
			order.ID, order.OrderType, order.SourceNode, order.DeliveryNode)
		return nil
	}
	if rs := e.settleCarts(order, finals); len(rs) > 0 {
		return rs[0]
	}
	return nil
}

// noteEvictedGhosts announces the bins a placement displaced — in the journal,
// and in the displaced bin's OWN history.
//
// AN EVICTION USED TO LEAVE NO TRACE ON ITS VICTIM. helpers.EvictStaleGhostBinsTx
// moves the displaced rows to _TRANSIT with a raw UPDATE, and nothing emitted a
// BinUpdatedEvent for them, so the audit subscriber (wiring.go) never wrote the row
// the ARRIVING bin gets. The bin whose record was deleted is the one an operator
// goes looking at, and its journal ended at whatever order last touched it —
// hours or days earlier.
//
// Springfield 2026-08-26: CARRIER-0003 was evicted from SMN_016 by order 5560's
// arrival, and its journal's last word was order 5514, a complex delivery from the
// night before. Two separate reconstructions of that night were misled by the gap,
// one of them inventing a double-occupancy at SMN_009 out of an inferred recovery
// that had gone unaudited the same way.
//
// The action is "evicted", not "moved": a reader can then tell a displacement
// from a delivery. (An older comment added that kanban keyed on "moved" — that
// demand path was deleted in 2026-08; the distinction survives on its own
// merits, and "moved" would also book CMS transfer rows for a displacement.)
func (e *Engine) noteEvictedGhosts(evicted []int64, what string, byBinID int64, atNode string) {
	if len(evicted) == 0 {
		return
	}
	e.logFn("WARN: %s of bin %d to %s evicted %d stale bin record(s) there — a delivery cannot "+
		"physically complete onto an occupied slot, so the completed delivery proves the slot was "+
		"empty; the stale bin is at _TRANSIT, recover via the anomalies page",
		what, byBinID, atNode, len(evicted))

	transit, err := e.db.GetNodeByDotName(domain.TransitNodeName)
	if err != nil || transit == nil {
		e.logFn("engine: resolve %s for eviction audit: %v", domain.TransitNodeName, err)
		return
	}
	for _, id := range evicted {
		ghost, gerr := e.db.GetBin(id)
		if gerr != nil || ghost == nil {
			e.logFn("engine: get evicted bin %d for its own audit row: %v", id, gerr)
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      BinActionEvicted,
			BinID:       ghost.ID,
			PayloadCode: ghost.PayloadCode,
			ToNodeID:    transit.ID,
			NodeID:      transit.ID,
		}})
	}
}

// settleCarts places each of the order's carts at its last drop (finals,
// domain.Order.FinalDrops), at delivery time, all or nothing.
//
// Note: previously this path forced staged=false for complex orders with
// WaitIndex > 0 ("operatorConfirmed"). Override removed 2026-04-14 — carts
// arriving at lineside stage like any other arrival.
//
// It returns one refusal per cart the claim guard declined (arrival_guard.go).
// A two-cart order can be refused for SOME of its carts, which is why this is a
// slice; and a refusal writes nothing at all (below).
func (e *Engine) settleCarts(order *orders.Order, finals []domain.Cart) []*ArrivalRefusal {
	var refusals []*ArrivalRefusal
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] and dests[i] belong to instructions[i]: the From side is the
	// cart's own last lift (the CMS ledger books it, and the lane gate evaluates
	// the lane it left), the destination is where its plan sets it down last.
	var fromNodeIDs []int64
	var dests []*nodes.Node

	for _, c := range finals {
		destName := domain.StepNode(order, *c.DropStep)
		// Claim-based teleport guard: a late-arriving FINISHED on a stale order,
		// or a cart reclaimed between FINISHED and the engine's processing of
		// it, must NOT be teleported to its drop. Compound children
		// (ParentOrderID != nil) skip the guard: a reshuffle's legs overlap
		// claims by design (each leg claims its bin at its handover,
		// store.ClaimCompoundLegBinTx, sibling-scoped), and the compound
		// dispatcher serializes them.
		guardBin, err := e.db.GetBin(c.BinID)
		if err != nil {
			e.logFn("engine: order %d bin %d get for delivery guard: %v", order.ID, c.BinID, err)
			continue
		}
		// Destination resolved BEFORE the guard so a refusal can say where the
		// cart was owed, not just who owns it — the diagnosable half.
		destNode, err := e.db.GetNodeByDotName(destName)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found on delivery: %v", order.ID, c.BinID, destName, err)
			continue
		}
		// ALREADY THERE IS NOT A PLACEMENT. Asked BEFORE ownership: a cart at its
		// drop is a finished delivery whoever holds the claim by now (an
		// intermediate set-down recorded at its block, or a repeat `delivered`
		// event — the at-least-once shape — whose claim is already released).
		// Skipping does not strand the claim: TerminalizeOrderWithReason
		// releases every claim this order holds and stamps anomaly_at only on
		// carts still at _TRANSIT, which a landed cart is not.
		if binAlreadyAt(guardBin, destNode.ID) {
			continue
		}
		if r := e.recordArrivalRefusal(refuseArrival(order, guardBin, destNode.ID, arrivalSiteDelivery)); r != nil {
			refusals = append(refusals, r)
			continue
		}
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:    c.BinID,
			ToNodeID: destNode.ID,
			Staged:   e.resolveNodeStaging(destNode),
		})
		fromNodeIDs = append(fromNodeIDs, e.stepNodeID(order, c.PickStep))
		dests = append(dests, destNode)
	}

	// ── A PARTIAL SETTLEMENT IS CORRUPTION, SO NOTHING COMMITS ───────────────
	//
	// This used to place the carts that passed and THEN hand the refusals back,
	// so a swap whose second leg was refused had its first leg written, the
	// order failed, and Edge was never told about the cart that did land (D4).
	// R.26 settled the policy on a plant fact: a dig works LANES and never
	// reclaims a leg of another process's in-flight order, so one cart of a
	// settlement belonging to somebody else is an integrity failure. The right
	// response is to stop loudly with nothing written. It is an assert placed
	// before the commit, not a compensation after it.
	if len(refusals) > 0 {
		for _, r := range refusals {
			e.logFn("ASSERT: order %d settlement refused for bin %d (%s; %s) — NOTHING IS WRITTEN. "+
				"%d of this order's %d carts were about to be placed and are not: a settlement that "+
				"finds one cart no longer belonging to this order is an integrity failure, and "+
				"digs work lanes rather than another process's in-flight legs, so this is not a race "+
				"the design permits (PLAN §R.26).",
				order.ID, r.BinID, r.Reason(), r.Context(), len(instructions), len(finals))
		}
		return refusals
	}
	if len(instructions) == 0 {
		return nil
	}

	// Through BinService so each cart set down at a seat is announced in the
	// settle's transaction (Lane H, H.2).
	evictedGhosts, err := e.binService.ApplyMultiBinArrival(order.ID, instructions)
	if err != nil {
		e.logFn("engine: delivery arrival for order %d: %v", order.ID, err)
		return nil
	}
	e.settled(order, instructions, fromNodeIDs, dests, evictedGhosts, "delivery")
	return nil
}

// stepNodeID is the node id of plan step i of the order, 0 when the step names
// no node that resolves (the CMS ledger books no From side and the lane gate
// skips it, which is the right degradation).
func (e *Engine) stepNodeID(order *orders.Order, i *int) int64 {
	if i == nil {
		return 0
	}
	name := domain.StepNode(order, *i)
	if name == "" {
		return 0
	}
	if n, err := e.db.GetNodeByDotName(name); err == nil && n != nil {
		return n.ID
	}
	return 0
}

// settled is what follows a settle's commit, at delivery or at the completion
// net: the evictions it caused, the burial shadow, a containment arrival, and
// one moved event per cart, from the cart's own last lift.
func (e *Engine) settled(order *orders.Order, instructions []orders.BinArrivalInstruction, fromNodeIDs []int64,
	dests []*nodes.Node, evictedGhosts []int64, what string) {
	if len(evictedGhosts) > 0 {
		by, at := int64(0), ""
		for i, inst := range instructions {
			if i > 0 {
				at += ", "
			}
			at += dests[i].Name
			by = inst.BinID
		}
		e.noteEvictedGhosts(evictedGhosts, fmt.Sprintf("%s of order %d", what, order.ID), by, at)
	}
	for i, inst := range instructions {
		// The burial shadow instrument: explicit, because the settle goes through
		// the store aggregate rather than BinService.ApplyArrival, which calls it
		// for itself. Post-commit and result-free.
		e.binService.NoteBurialShadow(inst.BinID, inst.ToNodeID, order.ID)
		e.stampContainmentArrival(inst.BinID, dests[i], order.PayloadCode, what)
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			e.logFn("engine: get bin %d for its %s event: %v", inst.BinID, what, err)
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      BinActionMoved,
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
			// Captured from the order, which is still in hand — the settle has
			// already released the cart's claim.
			RobotID: order.RobotID,
			OrderID: order.ID,
		}})
	}
}

// handleOrderCompleted runs when Edge confirms receipt. Cart movement already
// happened in handleOrderDelivered, so this is mostly paperwork (compound
// order advancement). The arrival is re-applied as an idempotent safety net —
// a cart already at its drop is skipped.
func (e *Engine) handleOrderCompleted(ev OrderCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for completion: %v", ev.OrderID, err)
		return
	}
	e.handleOrderCompletedFor(ev, order)
}

// handleOrderCompletedFor is handleOrderCompleted with the order already read,
// which is how the completion event's handler calls it (one read for its
// notice gate and for this).
func (e *Engine) handleOrderCompletedFor(ev OrderCompletedEvent, order *orders.Order) {

	// If this is a child of a compound order, advance the parent
	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	if order.SourceNode == "" || order.DeliveryNode == "" {
		return
	}

	finals := order.FinalDrops()
	if len(finals) == 0 {
		// SHADOWED (silent skip): a coordinator relies on this returning, and so
		// does a defective order — the same branch for opposite reasons.
		owns, oerr := e.db.OrderOwnsNoCargo(order.ID)
		service.NoteFolderShadow(service.FolderSiteCompletionNet, order.ID, owns, oerr)
		return
	}
	e.completionNet(order, finals)
}

// completionNet re-applies the arrival of any cart still owed one, in one
// transaction. The completion events fire on (X → delivered) and again on
// (delivered → confirmed), so this runs twice per order; it is idempotent —
// the ordinary case is every cart already at its drop. The rows are never
// deleted here: an order's carts are kept after it ends.
func (e *Engine) completionNet(order *orders.Order, finals []domain.Cart) {
	var instructions []orders.BinArrivalInstruction
	var fromNodeIDs []int64
	var dests []*nodes.Node

	for _, c := range finals {
		destName := domain.StepNode(order, *c.DropStep)
		destNode, err := e.db.GetNodeByDotName(destName)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found for completion: %v", order.ID, c.BinID, destName, err)
			continue
		}
		bin, err := e.db.GetBin(c.BinID)
		if err != nil {
			e.logFn("engine: order %d bin %d get for the completion net: %v", order.ID, c.BinID, err)
			continue
		}
		// The net's question — is there anything left to re-apply? — is answered
		// by reapplyRefused, which documents itself (the terminal cut, the arrived
		// check, then the claim guard).
		if skip, r := reapplyRefused(order, bin, destNode.ID, arrivalSiteCompletionNet); skip {
			e.recordArrivalRefusal(r) // nil for the ordinary already-landed case
			continue
		}
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:    c.BinID,
			ToNodeID: destNode.ID,
			Staged:   e.resolveNodeStaging(destNode),
		})
		fromNodeIDs = append(fromNodeIDs, e.stepNodeID(order, c.PickStep))
		dests = append(dests, destNode)
	}

	if len(instructions) == 0 {
		e.dbg("completion: order %d — every cart already at its drop, nothing to re-apply", order.ID)
		return
	}
	evictedGhosts, err := e.binService.ApplyMultiBinArrival(order.ID, instructions)
	if err != nil {
		e.logFn("engine: completion arrival for order %d: %v", order.ID, err)
		return
	}
	e.settled(order, instructions, fromNodeIDs, dests, evictedGhosts, "completion")
	e.logFn("engine: order %d completion: %d cart(s) moved", order.ID, len(instructions))
}
