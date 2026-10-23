package dispatch

import (
	"log"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// place_by_contents.go — the partial arm of choosing a lifted carrier's drop by
// what it holds.
//
// A carrier leaves a produce line full, empty, or UNFINISHED: its part set, its
// count above 0 and below the part's capacity. A full goes where the claim
// sends it; so does an unfinished one, unless the claim names a partial home
// (partial_destination), where it waits to be called back and finished. Core
// owns the count, so Core decides: the Edge authors the route and never reads
// the count to choose a drop.
//
// ── WHEN ──────────────────────────────────────────────────────────────────
// At the moment the count is final and before the drop reaches the fleet:
//   - a staged leg's release of the segment that lifts the carrier off the
//     line (HandleOrderRelease, after syncManifestForRelease; a produce
//     release's ingest is ahead of it in the same outbox, so the bin row holds
//     the shipped count);
//   - a plain move's planning, where the count is the operator's, declared on
//     the order (placePartialMove).
// A leg that lifts with no release cannot be diverted — the fleet holds the
// whole order by then — which is why every lift off a line position goes
// through its release.
//
// ── WHICH DROP ────────────────────────────────────────────────────────────
// The one that sets the lifted carrier down for good (carrierSetDown), which is
// not always the order's last: a press's R1 drops the old tote at the outbound
// and goes on to fill the back position, and the single robot parks the line's
// bin on outbound staging and lifts it again later.

// isUnfinished is the partial predicate: the carrier's part set, its count
// above 0 and below the part's capacity. Both defaults a count can hold (0, the
// standard pack) fall outside it.
func isUnfinished(payload string, count, capacity int) bool {
	return payload != "" && capacity > 0 && count > 0 && count < capacity
}

// carrierSetDown is the index of the dropoff that sets down for good the
// carrier the pickup at index p lifts: the next dropoff, followed through any
// later pickup at that same node (the same carrier lifted again). -1 when the
// plan drops nothing after p.
func carrierSetDown(steps []resolvedStep, p int) int {
	at := p
	for {
		d := -1
		for i := at + 1; i < len(steps); i++ {
			if steps[i].Action == protocol.ActionDropoff {
				d = i
				break
			}
		}
		if d < 0 {
			return -1
		}
		again := -1
		for i := d + 1; i < len(steps); i++ {
			if steps[i].Action == protocol.ActionPickup && steps[i].Node == steps[d].Node {
				again = i
				break
			}
		}
		if again < 0 {
			return d
		}
		at = again
	}
}

// releasedSpan is the plan's [from, to) released at waitIndex: after the
// (waitIndex+1)th wait, up to the next one. Empty when there is no such wait.
func releasedSpan(steps []resolvedStep, waitIndex int) (from, to int) {
	seen := 0
	from = -1
	for i, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if from >= 0 {
			return from, i
		}
		seen++
		if seen == waitIndex+1 {
			from = i + 1
		}
	}
	if from < 0 {
		return 0, 0
	}
	return from, len(steps)
}

// liftOff is the index of the first pickup at node in steps[from:to], or -1.
func liftOff(steps []resolvedStep, node string, from, to int) int {
	for i := from; i < to && i < len(steps); i++ {
		if steps[i].Action == protocol.ActionPickup && steps[i].Node == node {
			return i
		}
	}
	return -1
}

// partialHomeSlot is where an unfinished carrier of the claim at lineNode goes:
// a concrete home as named, a group home's free slot for this carrier. count
// is the carrier's final count: the bin's at a release, the operator's at a
// move that declares one. "" when the carrier is not unfinished, its claim
// names no home, or the home has no room (the carrier then keeps its authored
// drop and the log says why: a partial home is a preference, and a line must
// not stop for want of one).
func partialHomeSlot(db *store.DB, pure *plan.Planner, order *orders.Order, b *bins.Bin, count int,
	lineNode, current string, stated binresolver.BinTypeStatement) string {
	if b == nil || b.PayloadCode == "" {
		return ""
	}
	pl, err := db.GetPayloadByCode(b.PayloadCode)
	if err != nil || pl == nil || !isUnfinished(b.PayloadCode, count, pl.UOPCapacity) {
		return ""
	}
	// CONTAINMENT FIRST. Where the line's claim routes a contained part to
	// containment, the carrier's drop is containment's (placeForContainment, at
	// planning), and an unfinished one is not set aside to be finished with
	// fresh parts. A failed read keeps the drop as well: the home is a
	// preference, containment is not.
	if contained, err := containedAt(db, lineNode, b.PayloadCode); err != nil || contained {
		if err != nil {
			log.Printf("dispatch: containment read for %s/%s failed — order %d keeps its drop: %v",
				lineNode, b.PayloadCode, order.ID, err)
		}
		return ""
	}
	home, ambiguous, err := db.PartialHomeFor(lineNode, b.PayloadCode)
	if err != nil {
		log.Printf("dispatch: partial home read for %s/%s failed — order %d keeps its drop: %v",
			lineNode, b.PayloadCode, order.ID, err)
		return ""
	}
	if ambiguous {
		log.Printf("WARN: partial home for %s/%s is ambiguous (claims disagree) — order %d keeps its drop. "+
			"Fix the claims: one node and part, one partial home.", lineNode, b.PayloadCode, order.ID)
		return ""
	}
	if home == "" {
		return ""
	}
	node, err := db.GetNodeByDotName(home)
	if err != nil || node == nil {
		log.Printf("WARN: partial home %q for %s/%s does not resolve to a node — order %d keeps its drop",
			home, lineNode, b.PayloadCode, order.ID)
		return ""
	}
	if node.NodeTypeCode != protocol.NodeClassNGRP {
		return node.Name
	}
	// Already going there: a replay, or a release after the move's planning.
	if cur, err := db.GetNodeByDotName(current); err == nil && cur != nil && cur.ParentID != nil && *cur.ParentID == node.ID {
		return cur.Name
	}
	if pure == nil {
		return ""
	}
	res, err := pure.StoreDestination(node, b.PayloadCode, stated, digAskerFor(order))
	if err != nil || res == nil || res.Node == nil {
		log.Printf("dispatch: partial home %s has no room for bin %d (%d of %s) — order %d keeps its drop: %v",
			home, b.ID, b.UOPRemaining, b.PayloadCode, order.ID, err)
		return ""
	}
	return res.Node.Name
}

// containedAt reports whether payloadCode is contained and lineNode's claim
// routes it to containment: the case where containment, not the partial home,
// owns the carrier's drop. Two claims disagreeing about the route read as
// routed (placeForContainment then leaves the leg alone and warns).
func containedAt(db *store.DB, lineNode, payloadCode string) (bool, error) {
	active, err := db.PayloadContainmentActive(payloadCode)
	if err != nil || !active {
		return false, err
	}
	_, containment, ambiguous, err := db.ContainmentRouteForNode(lineNode)
	if err != nil {
		return false, err
	}
	return containment != "" || ambiguous, nil
}

// placePartialAtRelease re-points, in the order's stored plan, the drop of an
// unfinished carrier the segment released at waitIndex lifts off the order's
// line to the claim's partial home, moving the order's slot hold with it. It
// reports whether the plan changed; the caller re-reads the plan and splits its
// segment again.
func (d *Dispatcher) placePartialAtRelease(order *orders.Order, steps []resolvedStep, waitIndex int) bool {
	line := order.ProcessNode
	if line == "" {
		return false
	}
	from, to := releasedSpan(steps, waitIndex)
	p := liftOff(steps, line, from, to)
	if p < 0 {
		return false
	}
	f := carrierSetDown(steps, p)
	if f < 0 {
		return false
	}
	b := d.binStandingAt(line)
	if b == nil {
		return false
	}
	target := partialHomeSlot(d.db, d.planner.pure, order, b, b.UOPRemaining, line, steps[f].Node, d.carriedType(order, steps, f))
	if target == "" || target == steps[f].Node {
		return false
	}
	return d.repointDrop(order, steps, f, b, target)
}

// binStandingAt is the one bin at a node, or nil when there is none or more
// than one (the divert then has no single carrier to speak for).
func (d *Dispatcher) binStandingAt(node string) *bins.Bin {
	n, err := d.db.GetNodeByDotName(node)
	if err != nil || n == nil {
		return nil
	}
	bs, err := d.db.ListBinsByNode(n.ID)
	if err != nil || len(bs) != 1 {
		return nil
	}
	return bs[0]
}

// repointDrop moves step f's drop to target: the slot hold first (refused,
// nothing moves), then the plan, the delivery node when f is the plan's last
// drop, and the carrier's junction row.
func (d *Dispatcher) repointDrop(order *orders.Order, steps []resolvedStep, f int, b *bins.Bin, target string) bool {
	from := steps[f].Node
	tn, err := d.db.GetNodeByDotName(target)
	if err != nil || tn == nil {
		return false
	}
	if err := d.db.ReserveSlot(tn.ID, order.ID); err != nil {
		log.Printf("dispatch: partial home slot %s is held — order %d keeps its drop at %s: %v", target, order.ID, from, err)
		return false
	}
	if err := d.db.ConfirmSlotClaim(tn.ID, order.ID, nil); err != nil {
		if rErr := d.db.ReleaseSlotReservation(tn.ID, order.ID); rErr != nil {
			log.Printf("dispatch: partial home slot %s: release after a refused claim: %v", target, rErr)
		}
		log.Printf("dispatch: partial home slot %s refused the claim — order %d keeps its drop at %s: %v", target, order.ID, from, err)
		return false
	}
	if fn, err := d.db.GetNodeByDotName(from); err == nil && fn != nil {
		if err := d.db.ReleaseSlotClaim(fn.ID, order.ID); err != nil {
			log.Printf("dispatch: release order %d's hold on %s after the partial re-point: %v", order.ID, from, err)
		}
	}
	last := true
	for i := f + 1; i < len(steps); i++ {
		if steps[i].Action == protocol.ActionDropoff {
			last = false
			break
		}
	}
	if last {
		if err := applyDeliveryNodeAtStep(d.db, order, target, f); err != nil {
			log.Printf("dispatch: partial re-point order %d → %s: %v", order.ID, target, err)
			return false
		}
	} else {
		applyPlanNode(d.db, order, target, f, protocol.ActionDropoff)
	}
	if b != nil {
		if _, err := d.db.UpdateOrderBinDestNode(order.ID, b.ID, target); err != nil {
			log.Printf("dispatch: partial re-point order %d bin %d junction: %v", order.ID, b.ID, err)
		}
	}
	log.Printf("dispatch: order %d: bin %d leaves %s unfinished (%d of %s) — to its partial home %s instead of %s",
		order.ID, b.ID, order.ProcessNode, b.UOPRemaining, b.PayloadCode, target, from)
	return true
}

// placePartialMove is the plain move's door: a move off a line whose carrier is
// unfinished — by the count the operator declared on it (the Material page's
// release, the manual empty), else by the bin's own — goes to its claim's
// partial home. The move names its own source, the line. Returns the home's
// node, or nil to keep the authored destination.
func (s *PlanningService) placePartialMove(order *orders.Order, pick SourceResult) *nodes.Node {
	b := pick.Bin
	if b == nil || order.SourceNode == "" {
		return nil
	}
	count := b.UOPRemaining
	if order.RemainingUOP != nil {
		count = *order.RemainingUOP
	}
	target := partialHomeSlot(s.db, s.pure, order, b, count, order.SourceNode, order.DeliveryNode, binresolver.KnownCarrier(b))
	if target == "" || target == order.DeliveryNode {
		return nil
	}
	tn, err := s.db.GetNodeByDotName(target)
	if err != nil || tn == nil {
		return nil
	}
	if blocked, why := CheckDropoffCapacity(s.db, tn.Name, order.ID); blocked {
		log.Printf("dispatch: partial home %s has no room (%s) — move %d keeps its drop at %s",
			tn.Name, why.Cause, order.ID, order.DeliveryNode)
		return nil
	}
	log.Printf("dispatch: move %d: bin %d leaves %s unfinished (%d of %s) — to its partial home %s instead of %s",
		order.ID, b.ID, order.SourceNode, count, b.PayloadCode, tn.Name, order.DeliveryNode)
	return tn
}
