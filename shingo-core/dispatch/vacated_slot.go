package dispatch

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// vacated_slot.go — A DROP MAY LAND ON THE SLOT A COMMITTED LIFT VACATES.
//
// ── WHAT WAS BROKEN ─────────────────────────────────────────────────────────
//
// A complex leg's drop was refused wherever the only thing between it and a free
// slot was a lift already committed to happen first: its own earlier pickup, or
// its partner's. SPR 2026-09-28, pairs 7060/7061 and 7062/7063: the supply was
// widened FIFO onto a buffer partial, the return found its home holding a bin
// nobody lifts and every buffer full, and the pair parked on loader-park-no-slot
// every pass until an operator cancelled it. The buffer the supply was about to
// empty was the answer, and nothing could say so.
//
// ── THE RULE ────────────────────────────────────────────────────────────────
//
// A drop at step i of leg L onto node N is clear when N's only bin is taken
// before L gets there by
//
//	(a) L's own pickup at N strictly before step i (binsAtStep, carried logic
//	    included), or
//	(b) L's partner's pickup at N, when the partner ran EARLIER IN THIS PASS,
//	    the pickup is committedAtDispatch, it lifts a bin the partner claimed
//	    this pass, and L's drop is heldForRelease,
//
// and nothing else is in flight to N, by the asker's own in-flight predicate.
// Never a lane slot: lane holds are per order, and two orders on one mouth is
// the order 22/23 deadlock.
//
// CONSULTED ONLY WHERE TODAY'S ANSWER IS WAIT. Every asker keeps its answer
// and asks this only in the branch that would have parked, so a free slot
// always wins and every ordinary drop is byte-unchanged.
//
// MUTUAL GRANTS ARE IMPOSSIBLE BY CONSTRUCTION. Only the leg that runs second
// in stage 1 is handed partner facts (dispatchPairInOnePass), so two legs can
// never each be cleared by the other's lift.
//
// ── THE STAMP, AND WHY IT VALIDATES ITSELF ──────────────────────────────────
//
// A (b) grant is a promise about ORDER: the partner lifts before L drops. The
// fleet does not keep it — release gates on L being staged, not on the partner
// having lifted — so the release fence in HandleOrderRelease keeps it, and the
// stamp is what tells the fence which drops need keeping. Only a drop the rule
// admitted is stamped; nothing today's code already dispatches ever is.
//
// It names its node, and is honoured only while the step still drops there. Five
// writers can re-point a drop, one of them at release; each clears the stamp as
// housekeeping, but the self-check is the guarantee, because the next writer
// someone adds can forget to.

// vacateStamp records what the vacated-slot rule counted when it admitted a
// drop. Partner is 0 for an own-lift (a) grant, which needs no release fence:
// one robot lifts before it drops.
type vacateStamp struct {
	Node    string `json:"node"`
	Bin     int64  `json:"bin"`
	Partner int64  `json:"partner,omitempty"`
}

// stampHonoured returns the step's stamp when it still speaks for the node the
// step drops at, else nil. Every reader goes through this.
func stampHonoured(s resolvedStep) *vacateStamp {
	if s.Action != protocol.ActionDropoff || s.Vacate == nil || s.Vacate.Node == "" || s.Vacate.Node != s.Node {
		return nil
	}
	return s.Vacate
}

// partnerStamp is stampHonoured narrowed to a (b) grant — the only kind the
// release fence and the slot claim's partner arm read.
func partnerStamp(s resolvedStep) *vacateStamp {
	if st := stampHonoured(s); st != nil && st.Partner != 0 {
		return st
	}
	return nil
}

// pairPass is what the leg that ran first in stage 1 hands the leg after it:
// its THIS-PASS resolved steps and the bins it claimed. Never re-read from the
// database, and never built for a one-leg slice — a partner already committed or
// faulted is not in this pass, and its lift is not a promise anyone here made.
type pairPass struct {
	partner *orders.Order
	steps   []resolvedStep
	claimed []reservedPickup
}

// ── THE TWO BOUNDARY PREDICATES ─────────────────────────────────────────────
//
// Deliberately not widen's predicate. Widen asks which pickups should see today's
// pools; these ask what is physically committed.

// committedAtDispatch reports whether the pickup at step i is sent to the fleet
// by the dispatch itself: it sits in the splitAtWait prefix — before the plan's
// first wait of ANY kind, which is what the create sends — and no step at or
// before it touches a lane node, since the splice can put a gate wait in front
// of a lane entry at fleet create and delay the lift behind it. isLaneNode nil
// skips the lane test, for callers that only rank legs.
func committedAtDispatch(steps []resolvedStep, i int, isLaneNode func(string) bool) bool {
	if i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionPickup || steps[i].Node == "" {
		return false
	}
	pre, _ := splitAtWait(steps)
	if i >= len(pre) {
		return false
	}
	if isLaneNode == nil {
		return true
	}
	for j := 0; j <= i; j++ {
		if steps[j].Action != protocol.ActionWait && steps[j].Node != "" && isLaneNode(steps[j].Node) {
			return false
		}
	}
	return true
}

// heldForRelease reports whether the drop at step i waits for a station release:
// a station-owned wait comes before it. A leg with none is never a (b) dropper,
// because nothing then stands between its dispatch and its drop for the release
// fence to hold. IsStationWait still counts an untagged wait as the station's
// during the drain window; when that arm goes, so does this answer for untagged
// plans, and TestHeldForRelease_UntaggedWait pins it so the change is seen.
func heldForRelease(steps []resolvedStep, i int) bool {
	if i > len(steps) {
		i = len(steps)
	}
	for j := 0; j < i; j++ {
		if steps[j].Action == protocol.ActionWait && IsStationWait(steps[j].WaitKind) {
			return true
		}
	}
	return false
}

// liftsBeforeFirstWait is the stage-1 ranking key: a leg with a pickup the
// dispatch commits runs first, so the leg after it can be handed that fact. It
// reads the persisted plan and asks no lane question — ranking a leg first grants
// nothing, and the grant asks the full committedAtDispatch.
func liftsBeforeFirstWait(steps []resolvedStep) bool {
	for i := range steps {
		if committedAtDispatch(steps, i, nil) {
			return true
		}
	}
	return false
}

// stageOneOrder is the order the pair's legs acquire in: lifters first, ties in
// id order. The leader election is not this — it stays "lowest acquiring id".
func stageOneOrder(legs []*orders.Order) []*orders.Order {
	out := append([]*orders.Order(nil), legs...)
	lifts := make(map[int64]bool, len(out))
	for _, l := range out {
		if steps, ok := decodeSteps(l.StepsJSON); ok {
			lifts[l.ID] = liftsBeforeFirstWait(steps)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return lifts[out[i].ID] && !lifts[out[j].ID] })
	return out
}

// ── THE RULE ────────────────────────────────────────────────────────────────

// isLaneNode reports whether a node is a lane or a lane slot. Unreadable counts
// as a lane: the answer only ever refuses a grant, and refusing is today's wait.
func (d *Dispatcher) isLaneNode(name string) bool {
	n, err := d.db.GetNodeByDotName(name)
	if err != nil || n == nil {
		return true
	}
	return d.nodeIsLaneOrLaneSlot(n)
}

func (d *Dispatcher) nodeIsLaneOrLaneSlot(n *nodes.Node) bool {
	if n.NodeTypeCode == protocol.NodeClassLANE {
		return true
	}
	if n.ParentID == nil {
		return false
	}
	p, err := d.db.GetNode(*n.ParentID)
	if err != nil || p == nil {
		return true
	}
	return p.NodeTypeCode == protocol.NodeClassLANE
}

// vacatedFor is the rule's bin half: whether the drop at step i onto node is
// cleared by a committed lift, and the stamp that says which. nil means today's
// answer stands. The in-flight half is the caller's, through its own predicate.
//
// ONE BIN, TAKEN. The node must hold exactly one bin and that bin must be the
// one the lift takes. Two bins, or a bin held by anyone else, is a node this
// lift does not empty.
func (d *Dispatcher) vacatedFor(order *orders.Order, steps []resolvedStep, i int, node string, pc *pairPass) *vacateStamp {
	if order == nil || node == "" || i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionDropoff {
		return nil
	}
	n, err := d.db.GetNodeByDotName(node)
	if err != nil || n == nil || n.IsSynthetic || d.nodeIsLaneOrLaneSlot(n) {
		return nil
	}
	remaining, taken, err := d.allocator.binsAtStep(order, steps, i, node)
	if err != nil {
		return nil
	}
	// (a) L's own earlier pickup takes the only bin.
	if len(remaining) == 0 && len(taken) == 1 {
		return &vacateStamp{Node: node, Bin: taken[0]}
	}
	if len(remaining) != 1 || len(taken) != 0 {
		return nil
	}
	// (b) the partner's committed lift takes it.
	if pc == nil || pc.partner == nil || !heldForRelease(steps, i) {
		return nil
	}
	b := remaining[0]
	if b.ClaimedBy == nil || *b.ClaimedBy != pc.partner.ID {
		return nil
	}
	for _, cb := range pc.claimed {
		if cb.binID != b.ID || cb.nodeName != node {
			continue
		}
		if committedAtDispatch(pc.steps, cb.stepIndex, d.isLaneNode) && partnerLiftsResident(pc.steps, cb.stepIndex, node) {
			return &vacateStamp{Node: node, Bin: b.ID, Partner: pc.partner.ID}
		}
	}
	return nil
}

// partnerLiftsResident reports whether the partner's pickup at step k takes a bin
// that was on node before the partner arrived, rather than re-collecting a
// carrier the partner itself set down there earlier — binsAtStep's carried
// logic, walked over the partner's plan. A re-collect vacates nothing.
func partnerLiftsResident(steps []resolvedStep, k int, node string) bool {
	if k < 0 || k >= len(steps) || steps[k].Node != node || steps[k].Action != protocol.ActionPickup {
		return false
	}
	carried := 0
	for j := 0; j < k; j++ {
		if steps[j].Node != node {
			continue
		}
		switch steps[j].Action {
		case protocol.ActionDropoff:
			carried++
		case protocol.ActionPickup:
			if carried > 0 {
				carried--
			}
		}
	}
	return carried == 0
}

// vacatedGroupChild is the NGRP store arm: a drop into a group with no free
// child, asked of the children a committed lift is emptying — this leg's own
// earlier pickups (a) and the partner's claimed ones (b). The bin half is the
// rule's; everything else about the slot is asked through the resolver's own
// fences (binresolver.ResolveStoreVacated), including the in-flight count with
// this order left out. First admitted child wins; "" and nil mean capacity
// stands.
func (d *Dispatcher) vacatedGroupChild(order *orders.Order, steps []resolvedStep, i int, group *nodes.Node,
	orderPayload string, asker reservations.DigAsker, pc *pairPass) (string, *vacateStamp) {
	var candidates []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			candidates = append(candidates, n)
		}
	}
	for j := 0; j < i; j++ {
		if steps[j].Action == protocol.ActionPickup {
			add(steps[j].Node)
		}
	}
	if pc != nil {
		for _, cb := range pc.claimed {
			add(cb.nodeName)
		}
	}
	gr := &binresolver.GroupResolver{DB: d.db, DebugLog: d.dbg}
	for _, name := range candidates {
		n, err := d.db.GetNodeByDotName(name)
		if err != nil || n == nil || n.ParentID == nil || *n.ParentID != group.ID {
			continue
		}
		st := d.vacatedFor(order, steps, i, name, pc)
		if st == nil {
			continue
		}
		carrier := binresolver.BinTypeFrom(d.binTypeBeforeStep(steps, i),
			"complex step: no preceding pickup resolved to exactly one carrier")
		payload := resolvedStepPayload(steps[i], orderPayload)
		if _, err := gr.ResolveStoreVacated(group, n, []int64{st.Bin}, payload, carrier, asker); err != nil {
			d.dbg("vacated: order %d step %d: %s refused by the group's fences: %v", order.ID, i, name, err)
			continue
		}
		return name, st
	}
	return "", nil
}

// noOtherInFlight is the in-flight half for the loader and destination-gate
// askers: CheckDropoffCapacity's own predicate — another order delivering to the
// node that holds a claimed bin — with this order excluded. Unreadable refuses.
func (d *Dispatcher) noOtherInFlight(node string, order *orders.Order) bool {
	n, err := d.db.CountInFlightOrdersByDeliveryNodeExcluding(node, order.ID)
	return err == nil && n == 0
}

// lastDropIndex is the index of a plan's final dropoff, or -1.
func lastDropIndex(steps []resolvedStep) int {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Action == protocol.ActionDropoff {
			return i
		}
	}
	return -1
}

// persistStepStamp writes a stamp (and, when non-empty, an anchor) onto step i of
// the order's PERSISTED plan. Not a marshal of the caller's slice: loader
// placement patches steps_json without touching the in-memory plan, and writing
// the slice back would undo that patch. A step that no longer drops at the
// stamp's node is left alone, which is the stamp's own rule.
func (d *Dispatcher) persistStepStamp(order *orders.Order, i int, st *vacateStamp, anchor string) {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		log.Printf("dispatch: vacated stamp order %d: steps_json unparseable: %v", order.ID, err)
		return
	}
	if i < 0 || i >= len(steps) || steps[i].Action != protocol.ActionDropoff || steps[i].Node != st.Node {
		log.Printf("dispatch: vacated stamp order %d: step %d does not drop at %s — not stamped", order.ID, i, st.Node)
		return
	}
	steps[i].Vacate = st
	if anchor != "" {
		steps[i].Anchor = anchor
	}
	j, err := json.Marshal(steps)
	if err != nil {
		log.Printf("dispatch: vacated stamp order %d: marshal: %v", order.ID, err)
		return
	}
	if err := d.db.UpdateOrderStepsJSON(order.ID, string(j)); err != nil {
		log.Printf("dispatch: vacated stamp order %d: %v", order.ID, err)
		return
	}
	order.StepsJSON = string(j)
}

// admitVacated records a grant on the in-memory plan and the persisted one, and
// says so at INFO: the first-shift deploy check counts these lines.
func (d *Dispatcher) admitVacated(order *orders.Order, steps []resolvedStep, i int, st *vacateStamp, asker string) {
	steps[i].Vacate = st
	d.persistStepStamp(order, i, st, "")
	logVacated(order, st, asker)
}

func logVacated(order *orders.Order, st *vacateStamp, asker string) {
	if st.Partner != 0 {
		log.Printf("dispatch: vacated: order %d drops on %s, whose bin %d partner order %d lifts first (%s)",
			order.ID, st.Node, st.Bin, st.Partner, asker)
		return
	}
	log.Printf("dispatch: vacated: order %d drops on %s, whose bin %d its own earlier pickup lifts (%s)",
		order.ID, st.Node, st.Bin, asker)
}

// ── THE RELEASE FENCE (U5) ──────────────────────────────────────────────────

// vacateReleaseRefusal is the fence HandleOrderRelease consults before it
// appends a segment. It returns the refusal detail, or "" when the release may go.
//
// Z — THIS LEG DROPS ON A SLOT ITS PARTNER HAS NOT VACATED YET. A (b) grant
// promised the partner lifts first; the release is what would break that
// promise, so while the stamped bin still stands on the node the release is
// refused and the Edge rolls the leg back to staged.
//
// Y — THIS LEG'S PARTNER IS BEING HELD BY Z. Refusing Z alone lets its partner
// place onto a line that still holds Z's bin: by the Edge's deferral re-firing
// when it stages, or because it was already in transit and its release went
// straight away. So a leg is refused while its sibling is staged with a stamped
// pending segment and this leg drops onto a node holding a bin that sibling has
// claimed. The stamp condition is load-bearing: without it every press-index
// click is refused, because R1 drops on the on-deck position R2 has claimed and
// R2 is still staged when R1's release is processed.
//
// Both read the segment as it will be sent — after patchRedirectSegments, which
// can rewrite the final drop at release.
func (d *Dispatcher) vacateReleaseRefusal(order *orders.Order) string {
	seg, ok := d.pendingSegment(order)
	if !ok {
		return ""
	}
	for _, s := range seg {
		st := partnerStamp(s)
		if st == nil {
			continue
		}
		if d.binStillOn(st.Bin, s.Node) {
			return fmt.Sprintf("order %d would drop on %s, which its partner order %d has not yet emptied "+
				"(bin %d is still there) — release again once that robot has lifted it", order.ID, s.Node, st.Partner, st.Bin)
		}
	}
	if order.SiblingOrderUUID == "" {
		return ""
	}
	z, err := d.db.GetOrderByUUID(order.SiblingOrderUUID)
	if err != nil || z == nil || z.Status != StatusStaged {
		return ""
	}
	zSeg, ok := d.pendingSegment(z)
	if !ok {
		return ""
	}
	var held *vacateStamp
	for _, s := range zSeg {
		if st := partnerStamp(s); st != nil {
			held = st
			break
		}
	}
	if held == nil {
		return ""
	}
	for _, s := range seg {
		if s.Action != protocol.ActionDropoff || s.Node == "" {
			continue
		}
		n, err := d.db.GetNodeByDotName(s.Node)
		if err != nil || n == nil {
			continue
		}
		onNode, err := d.db.ListBinsByNode(n.ID)
		if err != nil {
			continue
		}
		for _, b := range onNode {
			if b.ClaimedBy != nil && *b.ClaimedBy == z.ID {
				return fmt.Sprintf("order %d would drop on %s, which still holds bin %d of its partner order %d — "+
					"and order %d is held until order %d empties %s. Release again once it has",
					order.ID, s.Node, b.ID, z.ID, z.ID, held.Partner, held.Node)
			}
		}
	}
	return ""
}

// pendingSegment is the segment a release of this order would append, exactly as
// HandleOrderRelease builds it, from a private decode so the real path is not
// touched.
func (d *Dispatcher) pendingSegment(o *orders.Order) ([]resolvedStep, bool) {
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
		return nil, false
	}
	seg, more, _ := splitSegment(steps, o.WaitIndex)
	if seg == nil {
		return nil, false
	}
	d.patchRedirectSegments(seg, o, more)
	return seg, true
}

// binStillOn reports whether a bin still stands on the named node. Unreadable
// answers yes: the fence refuses, and a refused release is a click repeated.
func (d *Dispatcher) binStillOn(binID int64, node string) bool {
	b, err := d.db.GetBin(binID)
	if err != nil {
		return true
	}
	if b == nil || b.NodeID == nil {
		return false
	}
	n, err := d.db.GetNodeByDotName(node)
	if err != nil || n == nil {
		return true
	}
	return *b.NodeID == n.ID
}
