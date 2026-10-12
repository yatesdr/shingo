package dispatch

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"

	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/store/orders"
)

// vacated_slot.go — the dispatcher's half of the vacated-slot rule.
//
// THE RULE IS THE PLANNER'S (dispatch/plan/vacated.go): whether a drop that
// would wait may land on the slot a committed lift empties first, read through
// the slot book (plan.BinsAtStep) and decided once. This file keeps what is not
// a decision: the pair's pass and the leg it hands the planner (pairPass), the
// stage-1 order, the in-flight half each asker asks of its own predicate, the
// stamp written onto the persisted plan, the INFO line the deploy check counts,
// and the release fence (U5).

// vacateStamp is the planner's grant record (plan.Vacate).
type vacateStamp = plan.Vacate

func partnerStamp(s resolvedStep) *vacateStamp { return plan.PartnerStamp(s) }

// pairPass is what the leg that ran first in stage 1 hands the leg after it:
// its THIS-PASS resolved steps and the bins it claimed. Never re-read from the
// database, and never built for a one-leg slice — a partner already committed or
// faulted is not in this pass, and its lift is not a promise anyone here made.
type pairPass struct {
	partner *orders.Order
	steps   []resolvedStep
	claimed []reservedPickup
}

// leg is the pass's partner as the planner's need carries it (plan.Need.Pair).
// nil for no partner.
func (pc *pairPass) leg() *plan.PairLeg {
	if pc == nil || pc.partner == nil {
		return nil
	}
	l := &plan.PairLeg{Partner: pc.partner.ID, Steps: pc.steps}
	for _, cb := range pc.claimed {
		l.Lifts = append(l.Lifts, plan.Lift{Bin: cb.binID, Node: cb.nodeName, Step: cb.stepIndex})
	}
	return l
}

// stageOneOrder is the order the pair's legs acquire in: lifters first, ties in
// id order. The leader election is not this — it stays "lowest acquiring id".
func stageOneOrder(legs []*orders.Order) []*orders.Order {
	out := append([]*orders.Order(nil), legs...)
	lifts := make(map[int64]bool, len(out))
	for _, l := range out {
		if steps, ok := decodeSteps(l.StepsJSON); ok {
			lifts[l.ID] = plan.LiftsBeforeFirstWait(steps)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return lifts[out[i].ID] && !lifts[out[j].ID] })
	return out
}

// vacatedFor asks the planner the rule's bin half for this leg (plan.Planner.Vacated).
func (d *Dispatcher) vacatedFor(order *orders.Order, steps []resolvedStep, i int, node string, pc *pairPass) *vacateStamp {
	if order == nil {
		return nil
	}
	return d.pure.Vacated(plan.Need{PayloadCode: order.PayloadCode, Asker: digAskerFor(order),
		Order: order.ID, Pair: pc.leg()}, steps, i, node)
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
// appends a segment. It returns the arm that refused ("Z" or "Y") and the
// refusal detail, or "" for both when the release may go.
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
func (d *Dispatcher) vacateReleaseRefusal(order *orders.Order) (arm, detail string) {
	seg, ok := d.pendingSegment(order)
	if !ok {
		return "", ""
	}
	for _, s := range seg {
		st := partnerStamp(s)
		if st == nil {
			continue
		}
		on, err := d.binOn(st.Bin, s.Node)
		if err != nil {
			return "Z", fmt.Sprintf("order %d would drop on %s, which its partner order %d must empty first, "+
				"and Core could not read where bin %d is (%v) — release again",
				order.ID, s.Node, st.Partner, st.Bin, err)
		}
		if on {
			return "Z", fmt.Sprintf("order %d would drop on %s, which its partner order %d has not yet emptied "+
				"(bin %d is still there) — release again once that robot has lifted it", order.ID, s.Node, st.Partner, st.Bin)
		}
	}
	if order.SiblingOrderUUID == "" {
		return "", ""
	}
	z, err := d.db.GetOrderByUUID(order.SiblingOrderUUID)
	if err != nil || z == nil || z.Status != StatusStaged {
		return "", ""
	}
	zSeg, ok := d.pendingSegment(z)
	if !ok {
		return "", ""
	}
	var held *vacateStamp
	for _, s := range zSeg {
		if st := partnerStamp(s); st != nil {
			held = st
			break
		}
	}
	if held == nil {
		return "", ""
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
				return "Y", fmt.Sprintf("order %d would drop on %s, which still holds bin %d of its partner order %d — "+
					"and order %d is held until order %d empties %s. Release again once it has",
					order.ID, s.Node, b.ID, z.ID, z.ID, held.Partner, held.Node)
			}
		}
	}
	return "", ""
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

// binOn reports whether a bin still stands on the named node. An answer that
// could not be read is an error: the fence still refuses on it (a refused
// release is a click repeated), but says it could not read, rather than
// claiming the bin is there.
func (d *Dispatcher) binOn(binID int64, node string) (bool, error) {
	b, err := d.db.GetBin(binID)
	if err != nil {
		return false, fmt.Errorf("read bin %d: %w", binID, err)
	}
	if b == nil || b.NodeID == nil {
		return false, nil
	}
	n, err := d.db.GetNodeByDotName(node)
	if err != nil {
		return false, fmt.Errorf("read node %s: %w", node, err)
	}
	if n == nil {
		return false, fmt.Errorf("node %s not found", node)
	}
	return *b.NodeID == n.ID, nil
}
