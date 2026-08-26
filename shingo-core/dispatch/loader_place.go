package dispatch

import (
	"encoding/json"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/orders"
)

// loader_place.go — the PARK side of the dedicated home loader, the inverse of the
// source side (loader_source.go). When a dedicated-loader changeover returns a bin
// from a home position, Core decides where it lands: its HOME if provably free,
// else a free buffer slot (home_kind='buffer', the M1 representation the source side
// already pools). Source and park use the SAME Core representation, so a parked
// partial is re-sourced by the same pool and the loop closes end-to-end.
//
// THERE IS NO THIRD BRANCH, and this header used to promise one — "else drain (the
// configured outbound)". No code in dispatch/ reads bin_loaders.outbound_dest, and
// a produce loader has none to read: fulls leave by order, not to a configured
// pool. When neither home nor buffer is free, placeForLoader writes nothing and the
// order keeps the delivery node it arrived with, which for these legs is the home
// that was just found occupied. "Draining" is a no-op with a log line, and on
// 2026-08-26 it delivered two carriers onto occupied homes at SMN_016 and SMN_035.
//
// LOCUS — Core is the single authority. The Edge ships the evac order with
// DeliveryNode="" and holds no authoritative bin-landing record; Core resolves the
// dropoff here and the existing release-time redirect overlay (patchRedirectSegments)
// carries it to the fleet. Divergence-free.
//
// NEVER-2N — the park's placement MUST consult the Core in-flight authority (the
// SAME order-truth restock gates on, the in-flight delivery-node counts /
// planning_service.go's CheckDropoffCapacity) — NEVER a bespoke count. Committing
// DeliveryNode=home makes this order in-flight to the home, so a later restock's
// own gate sees it and yields; and if a restock got there first, this read sees it
// and yields to buffer. A lost race requeues (scanner replay), the same contract
// every dropoff has. Do NOT route through the Edge withLoaderBudget seam (wrong
// store, re-introduces divergence) and do NOT add ClaimSlot here.
//
// The home occupancy check is split by LEG ROLE, read from the leg's own steps
// (legReturnsToHome). It used to be split on "does the plan contain a wait step",
// which is a proxy, and at Springfield the proxy is simply wrong — every return leg
// carries a station wait, so every return read as a supply leg. See
// legReturnsToHome for what that cost.
//
//   - Return legs whose home holds nothing, or holds only the carrier this swap is
//     itself lifting (homeClearForReturn): in-flight orders only. The bin standing
//     there is the one leaving, so counting it would force buffer incorrectly and
//     hand the home to the replenishment loop the moment it clears.
//
//   - Everything else — supply legs, unreadable shapes, and return legs whose home
//     holds a carrier nobody is coming for: full CheckDropoffCapacity, both physical
//     bins and in-flight orders. A leg routed onto a home that really is occupied
//     faults the robot on arrival.
//
// The buffer read is always full CheckDropoffCapacity — a buffer legitimately holds
// a parked partial, so its physical occupancy is real and must block.
func (d *Dispatcher) placeForDedicatedLoader(order *orders.Order, steps []resolvedStep) {
	// Pattern A: SourceNode is a home position (produce-side return).
	// Pattern B: DeliveryNode is a home position (consume-side removal leg).
	// Both route to the same home/buffer/drain logic; the only structural
	// difference is that Pattern A guards against same-order double-commit
	// (orderDeliversTo) while Pattern B does not — delivering to the home IS
	// the intent for the removal leg, and the single-robot-swap shape can't
	// produce a same-order conflict here.

	// Pattern A DECLINING must fall through to Pattern B, not end placement.
	// Before this was extracted, Pattern A's three lookup failures each
	// `return`ed out of placeForDedicatedLoader entirely, so a leg whose SOURCE
	// is not a dedicated-loader home never reached the delivery-side branch
	// below — Pattern B was unreachable for every such order even when its
	// DeliveryNode WAS a home. tryPlaceFromHomeSource reports whether it took
	// ownership; "false" means "not mine", never "done".
	if order.SourceNode != "" && !hasWaitStep(steps) {
		if d.tryPlaceFromHomeSource(order, steps) {
			return
		}
	}

	if order.DeliveryNode != "" {
		// Pattern B: explicit DeliveryNode is a home position. Two shapes land here:
		//   - Return leg: the robot bringing the line's spent bin back to the home.
		//     In-flight check only, but ONLY once homeClearForReturn has established
		//     that anything standing on the home is this swap's own — see header.
		//   - Supply leg: a fresh bin sourced from a staging/supermarket node
		//     delivering to the home. Full capacity gate, so a physically-occupied
		//     home routes to buffer instead of faulting on arrival.
		destNode, err := d.db.GetNodeByDotName(order.DeliveryNode)
		if err != nil || destNode == nil {
			return
		}
		home, err := d.db.GetLoaderHomeByPositionNode(destNode.ID)
		if err != nil || home == nil {
			return
		}
		loader, err := d.db.GetLoader(home.LoaderID)
		if err != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
			return
		}
		homeName := destNode.Name
		// A RETURN leg landing on a home whose only occupant this swap is
		// already lifting takes the in-flight check alone: the bin standing
		// there is the one leaving, so reading it as a blocker surrenders the
		// home. Everything else — a supply leg, an unreadable shape, or a home
		// holding a carrier nobody is coming for — takes the full gate, which
		// is the physical question.
		if d.legReturnsToHome(order, steps) && d.homeClearForReturn(order, homeName) {
			inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
			if ierr == nil && inFlight == 0 {
				d.setParkDestination(order, homeName, "home")
				return
			}
			d.placeForLoader(order, home.LoaderID, homeName)
			return
		}
		if blocked, _ := CheckDropoffCapacity(d.db, homeName, order.ID); blocked {
			d.placeForLoader(order, home.LoaderID, homeName)
		} else {
			d.setParkDestination(order, homeName, "home")
		}
	}
}

// tryPlaceFromHomeSource is Pattern A: the order lifts its bin AT a
// dedicated-loader home, so that home is the first candidate for the return —
// home if provably free, else a buffer slot, else drain.
//
// Reports whether it took OWNERSHIP of the placement. Every `false` means
// "this order is not a dedicated-loader home return" and the caller must go on
// to try Pattern B; it never means "placement is finished". That distinction is
// the whole reason this is a separate function: as inline code the three
// lookup failures below returned from placeForDedicatedLoader and silently
// skipped the delivery-side branch.
//
// `true` covers the drain outcome too — if placeForLoader finds no free buffer
// and drains, Pattern A still owned and answered the question.
func (d *Dispatcher) tryPlaceFromHomeSource(order *orders.Order, steps []resolvedStep) bool {
	srcNode, err := d.db.GetNodeByDotName(order.SourceNode)
	if err != nil || srcNode == nil {
		return false
	}
	home, err := d.db.GetLoaderHomeByPositionNode(srcNode.ID)
	if err != nil || home == nil {
		return false // source is not a loader home — Pattern B may still apply
	}
	loader, err := d.db.GetLoader(home.LoaderID)
	if err != nil || loader == nil || loader.Layout != loaders.LayoutDedicatedPositions {
		return false
	}
	homeName := srcNode.Name
	if !orderDeliversTo(steps, homeName) {
		inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
		if ierr == nil && inFlight == 0 {
			d.setParkDestination(order, homeName, "home")
			return true
		}
	}
	d.placeForLoader(order, home.LoaderID, homeName)
	return true
}

// placeForLoader routes to a free buffer slot for the given loader, or drains.
// Shared by Pattern A and Pattern B after the home-first check fails.
func (d *Dispatcher) placeForLoader(order *orders.Order, loaderID int64, homeName string) {
	members, merr := d.db.ListLoaderHomes(loaderID)
	if merr != nil {
		log.Printf("dispatch: place loader %d members: %v — draining order %d", loaderID, merr, order.ID)
		return
	}
	for _, m := range members {
		if m.Kind != loaders.HomeKindBuffer {
			continue
		}
		bn, nerr := d.db.GetNode(m.PositionNodeID)
		if nerr != nil || bn == nil {
			continue
		}
		if blocked, _ := CheckDropoffCapacity(d.db, bn.Name, order.ID); blocked {
			continue
		}
		d.setParkDestination(order, bn.Name, "buffer")
		return
	}
	d.dbg("place: loader home %s not free and no free buffer — draining order %d", homeName, order.ID)
}

// orderDeliversTo reports whether any dropoff step in this order targets node. Used
// to catch the single-robot-swap case where the SAME order delivers the new style to
// the home — that bin claims the home, so the returning partial must go to buffer.
func orderDeliversTo(steps []resolvedStep, node string) bool {
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Node == node {
			return true
		}
	}
	return false
}

// hasWaitStep reports whether any step in a resolved step list is a wait action.
// Used to distinguish evac/return legs (simple pickup→dropoff, no wait) from
// supply-from-home legs (staging wait embedded in the two-robot swap chain).
//
// KNOWN FALSE PROXY — deliberately still here. A wait step is a proxy for leg
// role, not the role itself, and it misclassifies a press-index R1 evac (which
// carries a wait) as a supply leg. Replacing it with the role predicates in
// swap_leg_role.go was planned and then FALSIFIED as specified: legTakesLineBin
// returns false — not "unknown" — when order.ProcessNode is empty, and ~12% of
// the complex orders that reach this file carry no ProcessNode (measured at
// Springfield: 193 of 1518, including 56 home-source and 47 home-delivery
// legs). A bare swap would stop Pattern A firing for those and would drop
// Pattern B's supply legs onto the in-flight-only branch, losing the physical
// capacity check that exists to prevent a robot fault on arrival.
//
// The two call sites also ask OPPOSITE questions, so one predicate cannot serve
// both by negation. Correct per-site predicates are deferred to a written
// proposal (Amendment 1 §A1.5); until then this stays, because round 1
// established its misroute is conservative — it biases to buffer, which is
// fail-safe — and a wrong replacement is strictly worse than the status quo.
// See EXEC-LOG-cobalt-kestrel-2284.md, "Queue item 4 — B-park".
func hasWaitStep(steps []resolvedStep) bool {
	for _, s := range steps {
		if s.Action == protocol.ActionWait {
			return true
		}
	}
	return false
}

// legReturnsToHome reports whether this leg is the RETURN half of a swap — the
// one bringing the line's spent carrier back to a dedicated home.
//
// Role comes from the leg's own steps, not from whether the plan contains a
// wait. The wait was a proxy and it is wrong here: every Springfield return leg
// carries a station wait (the robot dwells at the line until the operator
// releases it), so the proxy read every return as a supply leg. Consequence,
// SMN_016 and SMN_035 on 2026-08-26: the return ran the physical gate against
// its own home, saw the carrier its sibling was 83s from lifting, yielded to a
// buffer, and left the home unclaimed — so the replenishment loop filled it 3.8s
// after it cleared, and the returning carrier had nowhere to land five hours
// later. Its record was then evicted as a ghost by the delivery that landed on
// top of it.
//
// UNKNOWN IS NOT SUPPLY. legTakesLineBin answers false both for "this is a
// supply leg" and for "I cannot tell" (empty ProcessNode); collapsing those is
// the trap SHINGO_TODO records against the naive swap. The unreadable case is
// separated out first and keeps the previous behaviour verbatim, so those orders
// move exactly as they do today. It is unreachable in live traffic: every
// complex order since 2026-05-04 carries a ProcessNode, and the 193 that do not
// all predate it.
func (d *Dispatcher) legReturnsToHome(order *orders.Order, steps []resolvedStep) bool {
	if order.ProcessNode == "" {
		return !hasWaitStep(steps)
	}
	return legTakesLineBin(steps, order.ProcessNode)
}

// homeClearForReturn reports whether a return leg may take its home as the
// landing slot: the home is empty, or its occupant is the carrier this swap is
// already lifting.
//
// This is the distinction the old evac branch did not draw. It skipped the
// physical check outright, on the stated ground that "the physical bin at the
// home is the one being evac'd" — true of the carrier the sibling lifts, false
// of one some other order parked there, and the second case drives a robot at an
// occupied position. Ask which it is rather than assuming either.
//
// FAILS CLOSED: every unreadable answer returns false, routing the leg to the
// full capacity gate, which is the more conservative of the two paths.
func (d *Dispatcher) homeClearForReturn(order *orders.Order, homeName string) bool {
	node, err := d.db.GetNodeByDotName(homeName)
	if err != nil || node == nil {
		return false
	}
	occupants, err := d.db.ListBinsByNode(node.ID)
	if err != nil {
		return false
	}
	if len(occupants) == 0 {
		return true
	}
	// Occupied — acceptable only if this swap's own supply sibling lifts from
	// here. A sibling that has already gone terminal vouches for nothing: if it
	// had lifted the carrier the node would read empty above.
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil || sibUUID == "" {
		return false
	}
	sib, err := d.db.GetOrderByUUID(sibUUID)
	if err != nil || sib == nil || protocol.IsTerminal(sib.Status) {
		return false
	}
	sibSteps, ok := decodeSteps(sib.StepsJSON)
	if !ok {
		return false
	}
	for _, s := range sibSteps {
		if s.Action == protocol.ActionPickup && s.Node == homeName {
			return true
		}
	}
	return false
}

// applyPlanNode is the ONE writer of a step's node in steps_json. Every
// re-point below goes through it, so they cannot drift in HOW they patch —
// only in WHICH step they name, and that is the caller's fact to carry.
//
// ── WHY THE INDEX IS A PARAMETER ──────────────────────────────────────────
// It used to be derived here, by scanning: the delivery side took the LAST
// dropoff walking backward, the pickup side the FIRST pickup walking forward,
// both on the stated assumption that a gated plan is [wait, pickup, dropoff]
// and so has exactly one of each. A SWAP has two dropoffs — store the full bin
// in the lane, then return the empty to a press — and the lane gate re-binds
// the FIRST of them, the lane entry. The backward scan therefore rewrote the
// empty's return leg with the lane slot, and the order delivered BOTH its bins
// to one slot. Downstream: the press starved waiting for the empty that was
// driven into a lane, and the ghost eviction was forced to evict an occupant
// Core's own plan had manufactured — wiping a live claim on the way, which the
// arrival guard then read as a teleport. Two specimens per rig run, deterministic
// (see PLAN §R.5).
//
// The index is bounds-checked and the action VERIFIED, because an index that
// does not name the step the caller believes it does means the plan and the
// caller disagree — a mis-spliced plan, or an index computed against a
// different revision. Rewriting whatever sits at that offset is the defect
// this function exists to end, so a disagreement patches nothing and says so.
func applyPlanNode(db *store.DB, order *orders.Order, node string, stepIndex int, want string) {
	if order.StepsJSON == "" {
		return
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		log.Printf("dispatch: applyPlanNode order %d → %s: steps_json unparseable: %v", order.ID, node, err)
		return
	}
	if stepIndex < 0 || stepIndex >= len(steps) {
		log.Printf("dispatch: applyPlanNode order %d → %s: step %d out of range (%d steps) — plan not patched",
			order.ID, node, stepIndex, len(steps))
		return
	}
	if steps[stepIndex].Action != want {
		log.Printf("dispatch: applyPlanNode order %d → %s: step %d is %q, want %q — plan not patched",
			order.ID, node, stepIndex, steps[stepIndex].Action, want)
		return
	}
	steps[stepIndex].Node = node
	patched, err := json.Marshal(steps)
	if err != nil {
		log.Printf("dispatch: applyPlanNode order %d → %s: re-marshal: %v", order.ID, node, err)
		return
	}
	if uErr := db.UpdateOrderStepsJSON(order.ID, string(patched)); uErr != nil {
		log.Printf("dispatch: applyPlanNode steps_json order %d → %s: %v", order.ID, node, uErr)
		return
	}
	order.StepsJSON = string(patched)
}

// applyDeliveryNodeAtStep re-points an order's destination AND the one dropoff
// step that destination belongs to, named by index.
//
// The lane gate uses this: it knows which dropoff it is speaking for, because
// laneEntryAfterWait computed that index to find the order's lane entry in the
// first place. Carrying it here is the same move binForStep made — ask the plan
// what it already knows instead of re-deriving it from shape.
//
// Steps-patch failures are logged, not returned: delivery_node is the durable
// fact and the row is already correct; a stale steps_json is a display and
// replay concern the next resolve corrects.
func applyDeliveryNodeAtStep(db *store.DB, order *orders.Order, node string, stepIndex int) error {
	if err := db.UpdateOrderDeliveryNode(order.ID, node); err != nil {
		return err
	}
	order.DeliveryNode = node
	applyPlanNode(db, order, node, stepIndex, protocol.ActionDropoff)
	return nil
}

// applyFinalDeliveryNode re-points an order's FINAL destination — the last
// dropoff in its plan.
//
// This is the loader park leg's question and it is a different one from the
// gate's: a returning partial has no lane entry to speak for, and the leg it
// owns genuinely is the plan's last. The scan lives here, named, rather than
// inside the shared writer where a caller asking a different question inherited
// it by accident.
func applyFinalDeliveryNode(db *store.DB, order *orders.Order, node string) error {
	last := -1
	if order.StepsJSON != "" {
		var steps []resolvedStep
		if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err == nil {
			for i := len(steps) - 1; i >= 0; i-- {
				if steps[i].Action == protocol.ActionDropoff {
					last = i
					break
				}
			}
		}
	}
	return applyDeliveryNodeAtStep(db, order, node, last)
}

// applySourceNodeAtStep is applyDeliveryNodeAtStep's pickup-side twin: it
// updates source_node and patches the one pickup step that source belongs to,
// named by index, so a re-pointed order and the blocks it is about to emit
// cannot disagree. Used by the lane gate when a dig relocates a dwelling
// retrieve's bin (rebindGatedPickup).
//
// IT USED TO TAKE THE FIRST PICKUP, walking forward, on the same wrong
// assumption its delivery-side twin made — that a gated plan is
// [wait, pickup, dropoff] and so holds exactly one. A plan whose lane entry is
// INTERIOR (pick at a line, cross a lane, deliver to a line — the shape the
// splice produces routinely) has an earlier pickup belonging to a leg this
// rebind is not speaking for, and the forward scan rewrote that one instead.
// The delivery side's version of this bug is the one that bit, and it is
// written up at applyPlanNode; this side is the same defect unexercised, fixed
// with it rather than left as the next one to find.
//
// Steps-patch failures are logged, not returned: source_node is the durable
// fact and the row is already correct; a stale steps_json is a display and
// replay concern that the next resolve corrects.
func applySourceNodeAtStep(db *store.DB, order *orders.Order, node string, stepIndex int) error {
	if err := db.UpdateOrderSourceNode(order.ID, node); err != nil {
		return err
	}
	order.SourceNode = node
	applyPlanNode(db, order, node, stepIndex, protocol.ActionPickup)
	return nil
}

// setParkDestination commits the chosen dropoff for a dedicated-loader
// return leg. Delegates to applyDeliveryNode to keep delivery_node and
// steps_json in sync; also makes the order in-flight to the chosen node
// so concurrent restock gates observe it (the never-2N handshake).
func (d *Dispatcher) setParkDestination(order *orders.Order, node, kind string) {
	if order.DeliveryNode == node {
		return // already there — idempotent across scanner replays
	}
	if err := applyFinalDeliveryNode(d.db, order, node); err != nil {
		log.Printf("dispatch: place park dest order %d → %s: %v", order.ID, node, err)
		return
	}
	d.dbg("place: order %d returning partial → %s (%s)", order.ID, node, kind)
}
