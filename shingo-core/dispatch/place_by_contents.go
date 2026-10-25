package dispatch

import (
	"fmt"
	"log"
	"sort"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// place_by_contents.go — choosing a lifted carrier's drop by what it holds
// (dropByContents): the partial arm, then the maintainer arm.
//
// A carrier leaves a line full, empty, or UNFINISHED: its part set, its count
// above 0 and below the part's capacity. A full goes where the claim sends it;
// so does an unfinished one, unless the claim names a partial home
// (partial_destination), where it waits to be called back and finished. An
// EMPTY lifted off a line position goes to a maintained group that is short of
// it, when one supporting that position is: the maintainer wins over the
// planned drop. Core owns the count, so Core decides: the Edge authors the
// route and never reads the count to choose a drop.
//
// ── WHEN ──────────────────────────────────────────────────────────────────
// At the moment the count is final and before the drop reaches the fleet:
//   - a staged leg's release of the segment that lifts the carrier off the
//     line (HandleOrderRelease and the vacate hold, after
//     syncManifestForRelease; a produce release's ingest is ahead of it in the
//     same outbox, so the bin row holds the shipped count):
//     placeByContentsAtRelease;
//   - a plain move's planning, where the count is the operator's, declared on
//     the order (placeByContentsMove); a move that names its bin has its drop
//     chosen before the capacity gate, so the gate asks about the chosen drop
//     (decideTransport);
//   - a lift with no wait ahead of it (the tooling evac), which no release can
//     divert: at the reserve pass, once the removal pickup has bound its bin
//     (placeByContentsAtReserve).
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

// dropByContents is the one by-contents choice, every door's: where carrier b,
// lifted off liftNode and planned to current, goes by what it holds. The
// partial arm first (partialHome: an unfinished carrier to its claim's partial
// home), then the maintainer arm (maintainerSlot: an empty off a line position
// to a maintained group short of it). "" keeps the planned drop, and nothing
// stalls for want of either. why is the reason, for the door's log. count is
// the carrier's final count: the bin's at a release, the operator's at a move
// that declares one.
func dropByContents(db *store.DB, pure *plan.Planner, order *orders.Order, b *bins.Bin, count int,
	liftNode, current string, stated binresolver.BinTypeStatement) (target, why string) {
	if b == nil {
		return "", ""
	}
	if t := partialHome(db, pure, order, b, count, liftNode, current, stated); t != "" {
		return t, fmt.Sprintf("leaves %s unfinished (%d of %s) — to its partial home", liftNode, count, b.PayloadCode)
	}
	if t := maintainerSlot(db, pure, order, b, liftNode, current); t != "" {
		return t, fmt.Sprintf("leaves %s empty — to the maintained group short of it,", liftNode)
	}
	return "", ""
}

// partialHome is the partial arm: where an unfinished carrier of the claim at
// lineNode goes, a concrete home as named, a group home's free slot for this
// carrier. "" when the carrier is not unfinished, its claim names no home, or
// the home has no room (the carrier then keeps its authored drop and the log
// says why: a partial home is a preference, and a line must not stop for want
// of one).
func partialHome(db *store.DB, pure *plan.Planner, order *orders.Order, b *bins.Bin, count int,
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

// maintainerSlot is the maintainer arm: where an empty lifted off a line
// position goes when a maintained group supporting that position wants it,
// "" when none does. The maintainer is absolute where it wants the carrier and
// the door knows what the carrier is.
//
//   - The carrier is empty (no part) and not bare; the lift node is a line
//     position (IsLinePosition): never a move off a loader window, a storage
//     node or a group.
//   - A tote cart goes only as its own height, by its carrier state: tops &
//     bottoms to a group short of its tops & bottoms row; an empty at whole
//     layers to a group short of that wanted height (or, where the group wants
//     no heights of the type, short of the type); a leftover or an unverified
//     cart keeps the planned drop, where the unloader's card decides. A tote
//     cart lifted off a fed press's outside (IsFedOutside) always keeps the
//     planned drop: its count is exact only at its lift, after every door.
//   - Any other empty goes to a group short of its type (the keeper's Short).
//   - Of the groups that want it, the largest gap first, ties by name, the
//     first with a slot that admits the cart (StoreDestination, the cart
//     stated).
//
// Reads, only for an empty off a line position: the line-position read, the
// fed-outside read for a tote cart, the supporting groups; per supporting
// group its node and levels; once, the open maintain episodes; per group with a
// level of the type, the keeper's count (two or three statements) and, for a
// tote cart, its layer wants and residents by totes; then the slot choice.
func maintainerSlot(db *store.DB, pure *plan.Planner, order *orders.Order, b *bins.Bin, liftNode, current string) string {
	if b == nil || pure == nil || liftNode == "" {
		return ""
	}
	state := b.CarrierState()
	tote := protocol.IsToteType(b.TotesPerLayer)
	switch {
	case state == protocol.CarrierLoaded, state == protocol.CarrierBare:
		return ""
	case tote && state != protocol.CarrierTopsAndBottoms && state != protocol.CarrierEmpty:
		return "" // a leftover or an unverified cart: the card decides
	}
	if line, err := db.IsLinePosition(liftNode); err != nil || !line {
		if err != nil {
			log.Printf("dispatch: is %s a line position — order %d keeps its drop: %v", liftNode, order.ID, err)
		}
		return ""
	}
	if tote {
		if outside, err := db.IsFedOutside(liftNode); err != nil || outside {
			if err != nil {
				log.Printf("dispatch: is %s a fed outside — order %d keeps its drop: %v", liftNode, order.ID, err)
			}
			return ""
		}
	}
	groups, err := groupsWanting(db, b, liftNode)
	if err != nil {
		log.Printf("dispatch: maintained groups wanting bin %d off %s — order %d keeps its drop: %v",
			b.ID, liftNode, order.ID, err)
		return ""
	}
	cur, _ := db.GetNodeByDotName(current)
	for _, g := range groups {
		// Already going there: a replay, or a release after the move's planning.
		if cur != nil && cur.ParentID != nil && *cur.ParentID == g.node.ID {
			return cur.Name
		}
		res, err := pure.StoreDestination(g.node, "", binresolver.KnownCarrier(b), digAskerFor(order))
		if err != nil || res == nil || res.Node == nil {
			log.Printf("dispatch: maintained group %s is short %d of bin %d's kind but has no slot for it: %v",
				g.node.Name, g.gap, b.ID, err)
			continue
		}
		return res.Node.Name
	}
	return ""
}

// wantingGroup is a maintained group that wants a carrier, and by how many.
type wantingGroup struct {
	node *nodes.Node
	gap  int
}

// groupsWanting is the maintained groups supporting liftNode that are short of
// carrier b, the largest gap first, ties by name.
func groupsWanting(db *store.DB, b *bins.Bin, liftNode string) ([]wantingGroup, error) {
	ids, err := db.MaintainedGroupsSupporting(liftNode)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var open map[string]store.DemandOrigin
	var out []wantingGroup
	for _, id := range ids {
		g, err := db.GetNode(id)
		if err != nil {
			return nil, err
		}
		levels, err := db.ListMaintainLevels(id)
		if err != nil {
			return nil, err
		}
		var lv *store.MaintainLevel
		for i := range levels {
			if levels[i].BinTypeID == b.BinTypeID {
				lv = &levels[i]
			}
		}
		if lv == nil {
			continue
		}
		if open == nil {
			eps, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
			if err != nil {
				return nil, err
			}
			open = make(map[string]store.DemandOrigin, len(eps))
			for _, e := range eps {
				open[e.EpisodeKey] = e
			}
		}
		gap, err := carrierGap(db, g, *lv, open[protocol.MaintainEpisodeKey(g.Name, lv.BinTypeCode)], b)
		if err != nil {
			return nil, err
		}
		if gap > 0 {
			out = append(out, wantingGroup{node: g, gap: gap})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].gap != out[j].gap {
			return out[i].gap > out[j].gap
		}
		return out[i].node.Name < out[j].node.Name
	})
	return out, nil
}

// carrierGap is how short group g is of carrier b, by the keeper's arithmetic:
// the type's Short for a carrier that is no tote cart; for a tote cart its own
// height's (store.TypeLine.HeightShort): the tops & bottoms row for a cart at
// 0, its wanted height for a cart at whole layers, or the type's Short where
// the group wants no stack heights of the type.
func carrierGap(db *store.DB, g *nodes.Node, lv store.MaintainLevel, episode store.DemandOrigin, b *bins.Bin) (int, error) {
	if !protocol.IsToteType(b.TotesPerLayer) {
		c, err := db.CountLevel(g, lv, episode)
		return c.Short(), err
	}
	wants, err := db.ListMaintainLayerWants(g.ID)
	if err != nil {
		return 0, err
	}
	tl, _, err := db.TypeLineOf(g, lv, episode, wants)
	if err != nil {
		return 0, err
	}
	if b.CarrierState() == protocol.CarrierTopsAndBottoms {
		return tl.HeightShort(0), nil
	}
	layers, _ := b.Layers()
	for _, h := range tl.Heights {
		if h.Layers > 0 {
			return tl.HeightShort(layers), nil
		}
	}
	return tl.Short(), nil
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

// placeByContentsAtRelease re-points, in the order's stored plan, the drop of
// the carrier the segment released at waitIndex lifts off the order's line to
// where it goes by what it holds (dropByContents), moving the order's slot
// hold with it. It reports whether the plan changed; the caller re-reads the
// plan and splits its segment again.
func (d *Dispatcher) placeByContentsAtRelease(order *orders.Order, steps []resolvedStep, waitIndex int) bool {
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
	target, why := dropByContents(d.db, d.planner.pure, order, b, b.UOPRemaining, line, steps[f].Node,
		d.carriedType(order, steps, f))
	if target == "" || target == steps[f].Node {
		return false
	}
	return d.repointDrop(order, steps, f, b, target, why)
}

// placeByContentsAtReserve is the door for a lift with no wait ahead of it (the
// tooling evac lifts at its first step), which no release can divert: once the
// reserve has bound the removal pickup's bin, its drop goes where the bin goes
// by what it holds (dropByContents). The reserve is pure of claims, so the
// re-point is a pending slot reservation at the chosen slot, taken first
// (refused: nothing moves and the planned drop stands); then the plan's drop
// is re-pointed and the slot reconcile re-run over the re-pointed steps, which
// keeps that reservation and releases the planned drop's. The plan is marked
// re-anchored, so the caller persists it with its delivery node; the hand then
// confirms the re-pointed slot and writes the junction from the re-pointed
// steps. The order's bin is written with it, so the keeper counts the carrier
// coming before the confirm and asks for nothing more.
func (d *Dispatcher) placeByContentsAtReserve(order *orders.Order, plan *ComplexPlan, assigned []reservedPickup) {
	line := order.ProcessNode
	steps := plan.ResolvedSteps
	if line == "" {
		return
	}
	p := liftOff(steps, line, 0, len(steps))
	if p < 0 || waitBefore(steps, p) {
		return
	}
	f := carrierSetDown(steps, p)
	if f < 0 {
		return
	}
	var bound int64
	for _, rp := range assigned {
		if rp.stepIndex == p {
			bound = rp.binID
		}
	}
	if bound == 0 {
		return
	}
	b, err := d.db.GetBin(bound)
	if err != nil || b == nil {
		return
	}
	from := steps[f].Node
	target, why := dropByContents(d.db, d.planner.pure, order, b, b.UOPRemaining, line, from, binresolver.KnownCarrier(b))
	if target == "" || target == from {
		return
	}
	tn, err := d.db.GetNodeByDotName(target)
	if err != nil || tn == nil {
		return
	}
	if err := d.db.ReserveSlot(tn.ID, order.ID); err != nil {
		log.Printf("dispatch: slot %s is held — order %d keeps its drop at %s: %v", target, order.ID, from, err)
		return
	}
	prev := steps[f]
	steps[f].Node, steps[f].Group, steps[f].Vacate = target, "", nil
	if out, err := d.allocator.reserveComplexSlots(order, steps); err != nil || out != reserveComplete {
		steps[f] = prev
		if _, rerr := d.allocator.reserveComplexSlots(order, steps); rerr != nil {
			log.Printf("dispatch: order %d: restore its slot holds after a refused re-point: %v", order.ID, rerr)
		}
		log.Printf("dispatch: order %d: the slot reconcile refused the re-point to %s — it keeps its drop at %s: %v",
			order.ID, target, from, err)
		return
	}
	plan.Reanchored = true
	order.BinID = &b.ID
	if err := d.db.UpdateOrderBinID(order.ID, b.ID); err != nil {
		log.Printf("dispatch: order %d: record bin %d after the re-point: %v", order.ID, b.ID, err)
	}
	log.Printf("dispatch: order %d: bin %d %s %s instead of %s (at its reserve: it lifts with no wait ahead)",
		order.ID, b.ID, why, target, from)
}

// waitBefore reports whether a wait stands ahead of step p in the plan: the
// lift then has a release to be diverted at.
func waitBefore(steps []resolvedStep, p int) bool {
	for i := 0; i < p && i < len(steps); i++ {
		if steps[i].Action == protocol.ActionWait {
			return true
		}
	}
	return false
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
// drop, and the carrier's junction row. why is the caller's reason
// (dropByContents), said in the log.
func (d *Dispatcher) repointDrop(order *orders.Order, steps []resolvedStep, f int, b *bins.Bin, target, why string) bool {
	from := steps[f].Node
	tn, err := d.db.GetNodeByDotName(target)
	if err != nil || tn == nil {
		return false
	}
	if err := d.db.ReserveSlot(tn.ID, order.ID); err != nil {
		log.Printf("dispatch: slot %s is held — order %d keeps its drop at %s: %v", target, order.ID, from, err)
		return false
	}
	if err := d.db.ConfirmSlotClaim(tn.ID, order.ID, nil); err != nil {
		if rErr := d.db.ReleaseSlotReservation(tn.ID, order.ID); rErr != nil {
			log.Printf("dispatch: slot %s: release after a refused claim: %v", target, rErr)
		}
		log.Printf("dispatch: slot %s refused the claim — order %d keeps its drop at %s: %v", target, order.ID, from, err)
		return false
	}
	if fn, err := d.db.GetNodeByDotName(from); err == nil && fn != nil {
		if err := d.db.ReleaseSlotClaim(fn.ID, order.ID); err != nil {
			log.Printf("dispatch: release order %d's hold on %s after the re-point: %v", order.ID, from, err)
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
			log.Printf("dispatch: re-point order %d → %s: %v", order.ID, target, err)
			return false
		}
	} else {
		applyPlanNode(d.db, order, target, f, protocol.ActionDropoff)
	}
	if b != nil {
		if _, err := d.db.UpdateOrderBinDestNode(order.ID, b.ID, target); err != nil {
			log.Printf("dispatch: re-point order %d bin %d junction: %v", order.ID, b.ID, err)
		}
	}
	log.Printf("dispatch: order %d: bin %d %s %s instead of %s", order.ID, b.ID, why, target, from)
	return true
}

// placeByContentsMove is the plain move's door, after the pick: a move off a
// line goes where its carrier goes by what it holds (dropByContents), by the
// count the operator declared on it (the Material page's release, the manual
// empty), else by the bin's own. The move names its own source, the line.
// Returns the chosen node, or nil to keep the authored destination. A move
// that names its bin was already asked before the gate (byContentsDrop), and
// is asked here again only when it declares a count of its own.
func (s *PlanningService) placeByContentsMove(order *orders.Order, pick SourceResult) *nodes.Node {
	b := pick.Bin
	if b == nil || order.SourceNode == "" {
		return nil
	}
	count := b.UOPRemaining
	if order.RemainingUOP != nil {
		count = *order.RemainingUOP
	}
	target, why := dropByContents(s.db, s.pure, order, b, count, order.SourceNode, order.DeliveryNode, binresolver.KnownCarrier(b))
	if target == "" || target == order.DeliveryNode {
		return nil
	}
	tn, err := s.db.GetNodeByDotName(target)
	if err != nil || tn == nil {
		return nil
	}
	if blocked, block := CheckDropoffCapacity(s.db, tn.Name, order.ID); blocked {
		log.Printf("dispatch: %s has no room (%s) — move %d keeps its drop at %s",
			tn.Name, block.Cause, order.ID, order.DeliveryNode)
		return nil
	}
	log.Printf("dispatch: move %d: bin %d %s %s instead of %s", order.ID, b.ID, why, tn.Name, order.DeliveryNode)
	return tn
}

// byContentsDrop is a move for one cart (b, as the door read it), asked before
// the capacity gate so the gate asks about the drop the carrier will take
// (decideTransport): the cart's by-contents drop, by the count declared on the
// move else the cart's own, nil to keep the authored destination. It writes
// nothing; the chosen node rides the decision to applyTransport.
func (s *PlanningService) byContentsDrop(order *orders.Order, b *bins.Bin) (*nodes.Node, string) {
	if b == nil || order.SourceNode == "" {
		return nil, ""
	}
	count := b.UOPRemaining
	if order.RemainingUOP != nil {
		count = *order.RemainingUOP
	}
	target, why := dropByContents(s.db, s.pure, order, b, count, order.SourceNode, order.DeliveryNode, binresolver.KnownCarrier(b))
	if target == "" || target == order.DeliveryNode {
		return nil, ""
	}
	tn, err := s.db.GetNodeByDotName(target)
	if err != nil || tn == nil {
		return nil, ""
	}
	return tn, fmt.Sprintf("bin %d %s %s instead of %s", b.ID, why, tn.Name, order.DeliveryNode)
}
