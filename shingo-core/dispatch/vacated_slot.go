package dispatch

import (
	"encoding/json"
	"log"
	"sort"

	"shingo/protocol"
	"shingo/protocol/clock"
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
// stamp written onto the persisted plan with the time it was written, and the
// INFO line the deploy check counts. The release fence (U5, arms Z and Y) is
// gone: Core holds a release on the stamp until the bin lifts (vacate_hold.go).

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

// stampTime gives a partner stamp its StampedAt: the time the same grant was
// first written (prev, when it records the same grant), else now on Core's
// clock — the clock the lift stamps bins.last_lift_at with. The vacate fact is
// a lift after it, so a re-ask on a later pass must not move it forward. An
// own-lift (a) grant is never held and carries none.
func stampTime(st, prev *vacateStamp) {
	if st == nil || st.Partner == 0 || st.StampedAt != nil {
		return
	}
	if plan.SameGrant(prev, st) && prev.StampedAt != nil {
		st.StampedAt = prev.StampedAt
		return
	}
	now := clock.Now().UTC()
	st.StampedAt = &now
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
	stampTime(st, steps[i].Vacate)
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
