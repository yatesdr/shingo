package dispatch

import (
	"fmt"
	"sort"
	"strings"

	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// Tiered depth-ordered lane entry (the tiered-entry arm). A store about to be
// submitted into a mouth-enforced lane is held (parked) until it is safe to enter
// deepest-first, so a shallower bin can never be dropped in front of a deeper
// target (the leader-walls-follower wall, proven real in the Stage-1 experiment).
//
// It is a DISPATCH-TIME CLASSIFIER: read the already-resolved depths + the origin
// labels, compare, decide whether to submit now or park. There is NO runtime
// coordination between in-flight orders — a parked order simply re-evaluates on the
// next scan (when the blocking order has completed, it drops out of the active set
// and the parked one admits). Three tiers:
//
//   - Tier 1 — SAME-ORIGIN sharers dispatch TOGETHER (never gate against each
//     other). Same-origin = same (process, style) via the plant-claims mirror, or a
//     shared compound parent. The press left+right pushes are the case: two claims,
//     one (process, style) — they are partners, not rivals, so depth-ordering must
//     not add latency to the press.
//   - Tier 2 — a CROSS-ORIGIN, same-mode store targeting a DEEPER slot that has not
//     completed → park (wait for the deeper bin to be placed first).
//   - Tier 3 — an ACTIVE cross-origin coordinated GROUP in the lane (a same-origin
//     set co-dispatched under Tier 1, so its shallower members bypassed the depth
//     gate) → park until the group completes, so a newcomer never interleaves with
//     it.
//
// Completion is the release signal (the owner's config fallback): a blocker clears
// when its order leaves the active set. Releasing on ENTRY instead (letting a
// shallower store co-occupy behind a deeper one still being placed) needs an
// entry/XY signal that is a plant-runtime dependency — deferred; see the phase log.

// laneEntryCause values (operator-facing queue causes for a parked entry).
const (
	causeLaneDeeperPending = "lane-deeper-pending" // Tier 2: a deeper cross-origin store hasn't placed yet
	causeLaneGroupActive   = "lane-group-active"   // Tier 3: an active cross-origin group holds the lane
)

// laneEntryOrder is the classifier's view of one store targeting a lane: its slot
// depth, its origin key, and whether it belongs to an active same-origin group in
// this lane (a Tier-3 group member).
type laneEntryOrder struct {
	id      int64
	depth   int
	origin  string // canonical origin key; "" = unclassified (treated as unique — never same-origin)
	grouped bool
}

// classifyLaneEntry decides whether `self` may enter now or must park. Pure: every
// liveness fact is already resolved into `others` (the active, not-completed stores
// targeting the SAME lane). Returns a park cause, or "" to admit.
func classifyLaneEntry(self laneEntryOrder, others []laneEntryOrder) string {
	for _, o := range others {
		if o.id == self.id {
			continue
		}
		if sameOrigin(self.origin, o.origin) {
			continue // Tier 1: same-origin partner — co-dispatch, never gate
		}
		if o.depth > self.depth {
			return causeLaneDeeperPending // Tier 2: a deeper cross-origin store is still pending
		}
		if o.grouped {
			return causeLaneGroupActive // Tier 3: an active cross-origin group holds the lane
		}
	}
	return ""
}

// sameOrigin reports whether two origin keys denote the same origin. Empty keys are
// unclassified and never match (conservative: an order we can't place is treated as
// its own origin, so it is depth-gated rather than wrongly co-dispatched).
func sameOrigin(a, b string) bool {
	return a != "" && a == b
}

// originKeyForStyles builds a canonical origin key from a node's (process, style)
// claim set (sorted + joined), so two nodes under the same set compare equal. Empty
// set → "" (unclassified).
func originKeyForStyles(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	sorted := append([]string(nil), pairs...)
	sort.Strings(sorted)
	return "style:" + strings.Join(sorted, ",")
}

// laneEntryOriginFor resolves an order's origin key. A compound child keys on its
// parent (siblings share it); otherwise the demand node (process_node) resolves to
// its (process, style) set via the plant-claims mirror. A node absent from the
// mirror (loaders/unloaders are structurally excluded) yields "" — unclassified,
// hence its own origin and depth-gated. ◆ tracked in the phase log.
func (d *Dispatcher) laneEntryOriginFor(order *orders.Order) (string, error) {
	if order.ParentOrderID != nil {
		return fmt.Sprintf("compound:%d", *order.ParentOrderID), nil
	}
	if order.ProcessNode == "" {
		return "", nil
	}
	pairs, err := d.db.NodeStyleOrigins(order.ProcessNode)
	if err != nil {
		return "", err
	}
	return originKeyForStyles(pairs), nil
}

// AdmitLaneEntry is the fulfillment-facing tiered-entry gate: it reports whether a
// store must PARK (with an operator cause) before entering its lane, so entry is
// deepest-first. park=false for every non-lane / non-mouth-enforced destination —
// byte-identical when the gate is off.
func (d *Dispatcher) AdmitLaneEntry(order *orders.Order, destNode *nodes.Node) (park bool, cause string, err error) {
	return d.admitLaneEntry(order, destNode)
}

// admitLaneEntry is the tiered-entry gate for a store whose DROPOFF (destNode) is a
// slot in a mouth-enforced lane group. It returns park=true (with an operator
// cause) when the order must wait, and park=false otherwise — including for every
// non-lane / non-mouth-enforced destination, so the gate is byte-identical when no
// group enforces the mouth. Depth-1 lanes are exempt (single slot, no ordering).
func (d *Dispatcher) admitLaneEntry(order *orders.Order, destNode *nodes.Node) (park bool, cause string, err error) {
	if destNode == nil {
		return false, "", nil
	}
	lane, err := d.db.LaneForNode(destNode.ID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return false, "", err // not a lane slot, or a lane with no group
	}
	if !laneGateActive(d.laneEnforcementMode(*lane.ParentID)) {
		return false, "", nil // Core does not own this group's mouth → byte-identical
	}

	slots, err := d.db.ListLaneSlots(lane.ID)
	if err != nil || len(slots) < 2 {
		return false, "", err // depth-1 (or unreadable) lane — nothing to order
	}
	// The active-set query matches order.delivery_node by string, but delivery_node
	// can be written BARE ("SMN_001", from a node's .Name — engine/orders.go) or
	// DOT-qualified ("LANE.SMN_001", resolved via GetByDotName — the complex path
	// flows step nodes through it). Match BOTH forms so a dotted row is never
	// invisible to the gate (which would silently admit — the fail-open F1 flagged).
	lanePrefix := lane.Name + "."
	slotNames := make([]string, 0, 2*len(slots))
	depthByName := make(map[string]int, 2*len(slots))
	for _, s := range slots {
		dep, dErr := d.db.GetSlotDepth(s.ID)
		if dErr != nil {
			return false, "", dErr
		}
		slotNames = append(slotNames, s.Name, lanePrefix+s.Name)
		depthByName[s.Name] = dep
		depthByName[lanePrefix+s.Name] = dep
	}

	active, err := d.db.ActiveLaneStores(slotNames)
	if err != nil {
		return false, "", err
	}

	self, others, err := d.buildLaneEntryView(order, destNode, lane.ID, active, depthByName)
	if err != nil {
		return false, "", err
	}
	if c := classifyLaneEntry(self, others); c != "" {
		return true, c, nil
	}
	return false, "", nil
}

// stillWorkingLaneMouth filters the active-store set down to the orders that can
// still BLOCK an entrant — the placement-aware release signal (A′).
//
// The active-set query returns every non-terminal order whose delivery_node is a
// slot in this lane (ActiveByDeliveryNodes), which is COMPLETION-coarse: a store
// that has already dropped its bin keeps blocking until its whole order goes
// terminal, though physically the lane is clear behind it. The finer truth is
// already on disk: a store holds an inbound MOUTH ROW from the fleet commit
// (AcquireLanesForOrder, scanner.go) until its dropoff block reports FINISHED,
// where ReleaseInboundLaneForOrder deletes the row BEFORE the delivery-node
// early-return (lane_gate.go / wiring_block_completed.go). So for a DISPATCHED
// order:
//
//	holds an inbound mouth row on lane L  ⟺  has not yet placed its bin in L
//
// Hence the rule, in two halves:
//
//   - NOT yet dispatched (vendor_order_id == "") → KEEP as a blocker. It has not
//     reached the fleet, so it cannot have a mouth row yet, and dropping it would
//     silently stop a queued/sourcing deeper store from holding its place — a
//     behavior change well outside placement-release, and one that would widen the
//     one-sided deeper-later residual (F2).
//   - dispatched AND holding no inbound row → DROP. It has placed (or otherwise
//     finished with this mouth), so it blocks nobody.
//
// Modes other than inbound are not blockers for a store's depth ordering: an
// outbound holder removes a bin rather than adding one, and a dig excludes every
// other holder at the mouth gate itself (reservations/mouth.go admitMouth), which
// parks the entrant at admitLanes with the more accurate lane-held-dig cause.
//
// Transient note: an order dispatched BEFORE the group was switched to mouth
// enforcement carries a vendor id and never took a mouth row, so it stops being a
// blocker as soon as the config flips. That window closes when it completes, and
// it is the same class of transient a mid-flight config change already implies.
func (d *Dispatcher) stillWorkingLaneMouth(laneID int64, active []*orders.Order) ([]*orders.Order, error) {
	holds, err := reservations.ActiveMouthRows(d.db.DB, laneID)
	if err != nil {
		return nil, err
	}
	inbound := make(map[int64]bool, len(holds))
	for _, h := range holds {
		if h.Mode == reservations.ModeInbound {
			inbound[h.OrderID] = true
		}
	}
	kept := make([]*orders.Order, 0, len(active))
	for _, o := range active {
		if o.VendorOrderID != "" && !inbound[o.ID] {
			continue // dispatched and no longer holding the mouth → it has placed
		}
		kept = append(kept, o)
	}
	return kept, nil
}

// buildLaneEntryView resolves the classifier inputs: the self order and every other
// active store in the lane, each with its slot depth, origin key, and whether it is
// a member of an active same-origin group (≥2 same-origin stores in the lane).
//
// The active set is first narrowed by stillWorkingLaneMouth, so an order that has
// already PLACED its bin is neither a blocker nor a group member — the release
// signal is placement, not completion.
func (d *Dispatcher) buildLaneEntryView(order *orders.Order, destNode *nodes.Node, laneID int64, active []*orders.Order, depthByName map[string]int) (self laneEntryOrder, others []laneEntryOrder, err error) {
	selfOrigin, err := d.laneEntryOriginFor(order)
	if err != nil {
		return self, nil, err
	}
	self = laneEntryOrder{id: order.ID, depth: depthByName[destNode.Name], origin: selfOrigin}

	active, err = d.stillWorkingLaneMouth(laneID, active)
	if err != nil {
		return self, nil, err
	}

	// First pass: resolve origin for each active store and tally origins, so we can
	// mark grouped members (a non-empty origin held by ≥2 active stores in the lane).
	type entry struct {
		id     int64
		depth  int
		origin string
	}
	var entries []entry
	originCount := map[string]int{}
	for _, o := range active {
		origin, oErr := d.laneEntryOriginFor(o)
		if oErr != nil {
			return self, nil, oErr
		}
		entries = append(entries, entry{id: o.ID, depth: depthByName[o.DeliveryNode], origin: origin})
		if origin != "" {
			originCount[origin]++
		}
	}
	for _, e := range entries {
		others = append(others, laneEntryOrder{
			id:      e.id,
			depth:   e.depth,
			origin:  e.origin,
			grouped: e.origin != "" && originCount[e.origin] >= 2,
		})
	}
	return self, others, nil
}
