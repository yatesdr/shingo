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
// already pools), else drain (the configured outbound — unchanged). Source and park
// use the SAME Core representation, so a parked partial is re-sourced by the same
// pool and the loop closes end-to-end.
//
// LOCUS — Core is the single authority. The Edge ships the evac order with
// DeliveryNode="" and holds no authoritative bin-landing record; Core resolves the
// dropoff here and the existing release-time redirect overlay (patchRedirectSegments)
// carries it to the fleet. Divergence-free.
//
// NEVER-2N — the park's placement MUST consult the Core in-flight authority (the
// SAME order-truth restock gates on, CountInFlightOrdersByDeliveryNode /
// planning_service.go's CheckDropoffCapacity) — NEVER a bespoke count. Committing
// DeliveryNode=home makes this order in-flight to the home, so a later restock's
// own gate sees it and yields; and if a restock got there first, this read sees it
// and yields to buffer. A lost race requeues (scanner replay), the same contract
// every dropoff has. Do NOT route through the Edge reserveLoaderBins seam (wrong
// store, re-introduces divergence) and do NOT add ClaimSlot here.
//
// The home occupancy check is split by order shape:
//
//   - Evac/return legs (no wait step): in-flight orders only. The physical bin at
//     the home is the one being evac'd (it is leaving), so CountBins would always
//     read as occupied and force buffer incorrectly.
//
//   - Supply legs (wait step embedded): full CheckDropoffCapacity — both physical
//     bins and in-flight orders. A supply leg may target a home that already holds
//     a real bin (e.g. leftover from a prior manual move); routing it there without
//     the physical check causes a robot fault on arrival.
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
		//   - Evac/return (no wait step): the removal robot returning an evac bin to
		//     the home after changeover. In-flight check only — see header comment.
		//   - Supply leg (wait step): a fresh bin sourced from a staging/supermarket
		//     node delivering to the home. Use the full capacity gate so a physically-
		//     occupied home routes to buffer instead of faulting on arrival.
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
		if hasWaitStep(steps) {
			// Supply leg: full capacity gate (physical bins + in-flight).
			if blocked, _ := CheckDropoffCapacity(d.db, homeName, order.ID); blocked {
				d.placeForLoader(order, home.LoaderID, homeName)
			} else {
				d.setParkDestination(order, homeName, "home")
			}
			return
		}
		// Evac/return: in-flight only.
		inFlight, ierr := d.db.CountInFlightOrdersByDeliveryNodeExcluding(homeName, order.ID)
		if ierr == nil && inFlight == 0 {
			d.setParkDestination(order, homeName, "home")
			return
		}
		d.placeForLoader(order, home.LoaderID, homeName)
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

// applyDeliveryNode is the canonical way to override an order's final
// destination. It updates delivery_node (the in-flight gate used by the
// never-2N handshake and capacity checks) and, for complex orders, patches
// the last dropoff step in steps_json so the HMI route display stays
// consistent with what the fleet will actually receive.
//
// Call this instead of db.UpdateOrderDeliveryNode directly whenever the
// resolved destination may differ from what the Edge originally planned.
func applyDeliveryNode(db *store.DB, order *orders.Order, node string) error {
	if err := db.UpdateOrderDeliveryNode(order.ID, node); err != nil {
		return err
	}
	order.DeliveryNode = node
	if order.StepsJSON != "" {
		var steps []resolvedStep
		if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err == nil {
			for i := len(steps) - 1; i >= 0; i-- {
				if steps[i].Action == protocol.ActionDropoff {
					steps[i].Node = node
					break
				}
			}
			if patched, err := json.Marshal(steps); err == nil {
				if uErr := db.UpdateOrderStepsJSON(order.ID, string(patched)); uErr != nil {
					log.Printf("dispatch: applyDeliveryNode steps_json order %d → %s: %v", order.ID, node, uErr)
				} else {
					order.StepsJSON = string(patched)
				}
			}
		}
	}
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
	if err := applyDeliveryNode(d.db, order, node); err != nil {
		log.Printf("dispatch: place park dest order %d → %s: %v", order.ID, node, err)
		return
	}
	d.dbg("place: order %d returning partial → %s (%s)", order.ID, node, kind)
}
