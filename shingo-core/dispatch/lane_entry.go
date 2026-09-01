package dispatch

import (
	"encoding/json"
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
// Completion is the release signal: a blocker clears
// when its order leaves the active set. Releasing on ENTRY instead (letting a
// shallower store co-occupy behind a deeper one still being placed) needs an
// entry/XY signal that is a plant-runtime dependency — deferred; see the phase log.

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
func classifyLaneEntry(self laneEntryOrder, others []laneEntryOrder) QueueCause {
	for _, o := range others {
		if o.id == self.id {
			continue
		}
		if sameOrigin(self.origin, o.origin) {
			continue // Tier 1: same-origin partner — co-dispatch, never gate
		}
		if o.depth > self.depth {
			return CauseLaneDeeperPending // Tier 2: a deeper cross-origin store is still pending
		}
		if o.grouped {
			return CauseLaneGroupActive // Tier 3: an active cross-origin group holds the lane
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

// THE PRE-DISPATCH TIERED GATE IS GONE — AdmitLaneEntry and admitLaneEntry with
// it — and the classifier below is untouched.
//
// The two were the DISPOSITION half of the tiered arm: park a store before it is
// dispatched, so entries happen deepest-first. That disposition belonged to the
// `mouth` enforcement mode, which was the "gate on, no waiting room" setting, and
// the mark ruling deletes it: a lane either has a waiting point (the robot drives
// out and the tiers are applied at the append, by the evaluator) or it has none
// (Core does not order that lane's entries at all, exactly as `none` never did).
// There is no state left in which a pre-dispatch tier park can fire.
//
// So what went is a gate that could only ever admit. Leaving it would have been
// worse than deleting it: a check that always passes reads, to the next person,
// like a check.
//
// THE POLICY SURVIVED WHOLE. laneEntryCause is the tiers, and the gate's
// evaluator calls it (lane_gate_release.go) at the moment the robot is actually
// asking to enter. The coverage moved with it — the tier tests drive the
// classifier directly now rather than through a wrapper that no longer decides
// anything.

// laneEntryCause runs the tiered classifier for `order` entering `lane` at
// destNode and returns its park cause ("" = admit). It is the POLICY half,
// shared verbatim by both arms: the tiered arm turns a cause into a pre-dispatch
// park, the gate arm turns the same cause into "dwell at the wait point".
//
// Callers must have already established that Core owns this lane's mouth.
func (d *Dispatcher) laneEntryCause(lane *nodes.Node, order *orders.Order, destNode *nodes.Node) (GateVerdict, error) {
	slots, err := d.db.ListLaneSlots(lane.ID)
	if err != nil {
		return GateVerdict{}, err // unreadable lane — undetermined, never an admit
	}
	if len(slots) < 2 {
		return Admitted(), nil // depth-1 lane: one slot, nothing to order
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
			return GateVerdict{}, dErr
		}
		slotNames = append(slotNames, s.Name, lanePrefix+s.Name)
		depthByName[s.Name] = dep
		depthByName[lanePrefix+s.Name] = dep
	}

	active, err := d.db.ActiveLaneStores(slotNames)
	if err != nil {
		return GateVerdict{}, err
	}

	self, others, err := d.buildLaneEntryView(order, destNode, lane.ID, active, depthByName)
	if err != nil {
		return GateVerdict{}, err
	}
	if c := classifyLaneEntry(self, others); c != "" {
		return Refused(c), nil
	}
	return Admitted(), nil
}

// stillComingToLane filters the active-store set down to the orders that can
// still BLOCK an entrant — the placement-aware release signal (A'), DERIVED
// rather than read off a reservation row.
//
// The active-set query returns every non-terminal order whose delivery_node is a
// slot in this lane (ActiveByDeliveryNodes), which is COMPLETION-coarse: a store
// that has already dropped its bin keeps blocking until its whole order goes
// terminal, though physically the lane is clear behind it. What is needed is the
// finer question — is this order still COMING to my lane with a bin — and the
// three facts that answer it were all already on disk.
//
// ── IT USED TO ASK THE MOUTH ROW, AND THE ROW IS GOING AWAY ───────────────
//
// The old rule was "dispatched AND holding no inbound mouth row → it has placed".
// That was true, and it was true only because a gated store held an inbound row
// from the fleet commit until its dropoff reported FINISHED. The gated
// destination hold is retired (resolveOrderLaneHolds): a robot standing at a
// group's waiting spot has not chosen its slot yet, so a row taken on its behalf
// reserved a mouth for a destination it had not committed to. Reading placement
// off a row that is no longer written would have quietly answered "everybody has
// placed" and let entrants walk past real blockers — which is 12b's failure
// exactly, and why the retire and this derivation land in ONE commit.
//
// ── THE THREE KEEP-ARMS, EACH A FACT SOMETHING ELSE ALREADY MAINTAINS ─────
//
//   - NOT yet dispatched (vendor_order_id == "") → KEEP. It has not reached the
//     fleet, so it is certainly still coming. Dropping it would silently stop a
//     queued/sourcing deeper store from holding its place — a behaviour change
//     well outside placement-release, and one that would widen the one-sided
//     deeper-later residual (F2). Unchanged from the old first half.
//
//   - GATE-STAGED at a wait whose lane is this one → KEEP. The robot is standing
//     at a spot with its tail un-appended: it is coming, it just has not been let
//     in. IsGateStaged reads the wait the order is parked AT, and WaitLane names
//     which lane that wait gates, so "coming HERE" is answered without a row.
//     This arm ALSO closes a live latent defect the release pass header already
//     names: a dwelling COMPLEX candidate held no inbound row of its own, so the
//     old rule read it as PLACED while it was still standing outside the lane
//     holding its bin. It was invisible while nothing dwelled; the group mouth
//     makes dwelling ordinary.
//
//   - HOLDING LANE OCCUPANCY → KEEP. The robot is physically in the corridor.
//     That row is taken at the append (takeLaneOccupancyByID) and released when
//     the robot leaves, and it is the mutex the mouth row was mistaken for.
//
//   - HOLDING AN INBOUND MOUTH ROW → KEEP. The old rule, unchanged, for the
//     population that still writes one: an UNMARKED lane takes its destination
//     hold at the fleet commit and drops it when the dropoff reports FINISHED,
//     so on those the row is direct evidence of "has not placed yet". Every lane
//     at both plants is one of those today, and keeping this arm is what makes
//     "an unmarked lane behaves byte-identically" true rather than aspirational.
//
// Anything else DROPS: dispatched, not standing at this lane's mark, not in the
// corridor, holding nothing — it has placed, or it is somewhere else entirely,
// and either way it blocks nobody.
//
// THE SET ONLY GREW. Three of the four arms were already the rule or already
// implied by it; what is new is that a dweller and a robot in the corridor are
// named explicitly instead of being inferred from a row that a gated lane no
// longer writes. A witness that keeps MORE candidates parks an entrant it need
// not have; one that keeps fewer walks it into a lane somebody is still in. The
// direction of the change is the safe one, deliberately.
//
// Depth order is untouched: it is derived from the plan (GetSlotDepth) and never
// came from a reservation.
func (d *Dispatcher) stillComingToLane(laneID int64, active []*orders.Order) ([]*orders.Order, error) {
	occupied, err := reservations.OccupantsOf(d.db.DB, laneID)
	if err != nil {
		return nil, err
	}
	inCorridor := make(map[int64]bool, len(occupied))
	for _, id := range occupied {
		inCorridor[id] = true
	}
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
		switch {
		case o.VendorOrderID == "": // not dispatched — certainly still coming
		case inCorridor[o.ID]: // physically inside the lane
		case inbound[o.ID]: // an unmarked lane's destination hold: not placed yet
		case IsGateStaged(o) && gateWaitLane(o) == laneID: // standing at THIS lane's mark
		default:
			continue
		}
		kept = append(kept, o)
	}
	return kept, nil
}

// gateWaitLane names the lane the order's current wait gates, or 0.
//
// Same walk laneOfGateWait does in lane_floor.go, and the duplication is worth
// the note: that one is keyed to the floor's needs and this one to the witness's,
// and collapsing them would give a widely-used predicate a second return value
// almost every caller discards. The parse is over a string already in memory.
func gateWaitLane(o *orders.Order) int64 {
	if o == nil || o.StepsJSON == "" {
		return 0
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
		return 0
	}
	w, ok := waitAt(steps, o.WaitIndex)
	if !ok {
		return 0
	}
	return w.WaitLane
}

// buildLaneEntryView resolves the classifier inputs: the self order and every other
// active store in the lane, each with its slot depth, origin key, and whether it is
// a member of an active same-origin group (≥2 same-origin stores in the lane).
//
// The active set is first narrowed by stillComingToLane, so an order that has
// already PLACED its bin is neither a blocker nor a group member — the release
// signal is placement, not completion.
func (d *Dispatcher) buildLaneEntryView(order *orders.Order, destNode *nodes.Node, laneID int64, active []*orders.Order, depthByName map[string]int) (self laneEntryOrder, others []laneEntryOrder, err error) {
	selfOrigin, err := d.laneEntryOriginFor(order)
	if err != nil {
		return self, nil, err
	}
	self = laneEntryOrder{id: order.ID, depth: depthByName[destNode.Name], origin: selfOrigin}

	active, err = d.stillComingToLane(laneID, active)
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
