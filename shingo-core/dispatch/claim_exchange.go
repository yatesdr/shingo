package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// claim_exchange.go — the same-part claim exchange at a dig (the A/B strand,
// FINDINGS-ab-strand-press10-2026-10-20).
//
// A dispatched complex order claims every pickup bin at dispatch, so a leg
// parked at a station wait BEFORE a later pickup holds a hard claim with no
// robot moving toward that bin. When that bin is the mouth bin of a lane, and
// another order's bin of the same part sits behind it, the other order's dig
// was refused as "a robot is carrying the blocker out" while nothing was
// moving: the wait's releaser was a station act. On a sequential pair the
// Edge would not make that act until the other order delivered, and neither
// moved.
//
// The exchange: the digger takes the mouth bin with no dig, and the holder's
// later pickup is re-pointed to the bin behind it, which is the mouth by the
// time the holder's robot gets there. It runs only on the complex-burial side
// (handleComplexBurial), where the digger is by construction not yet sent, so
// a bin moves one way only, toward the order that has not been dispatched. The
// gate's acceptance dig, whose requester is a robot at the mark, never
// exchanges: a re-pointed holder refused at the same lane would swap back.
//
// Each refusal keeps the claims as they were. The digger then waits under the
// cause the proposer read, except when the holder's next unreleased wait
// before its pickup is a station wait: then under dig-blocker-held-at-station,
// whose sentence names the holder and its station release.

// claimExchangeRefusal names why an exchange was not made, for the log line.
type claimExchangeRefusal string

// exchangeRead is what the decision reads about the holder: its stored plan,
// the step that picks the mouth bin, and where that step stands against the
// fleet.
type exchangeRead struct {
	steps []resolvedStep
	// k is the index of the holder's pickup of the mouth bin, -1 when no step
	// is found.
	k int
	// unsent: step k is after the wait at wait_index, so the fleet has not
	// received it (SplitAtWait sends only the prefix up to a wait; each later
	// segment goes out on that wait's release).
	unsent bool
	// atStation: unsent, and that wait is a station wait.
	atStation bool
	// atLaneMark: the holder's current wait is a lane wait on laneID.
	atLaneMark bool
}

// readHolder reads the holder's plan against the mouth bin (at mouthNode) and
// the dug lane. One statement only for a holder with junction rows.
func (d *Dispatcher) readHolder(holder *orders.Order, mouthBin int64, mouthNode string, laneID int64) exchangeRead {
	r := exchangeRead{k: -1}
	steps, ok := decodeSteps(holder.StepsJSON)
	if !ok {
		return r
	}
	r.steps = steps
	if obs, err := d.db.ListOrderBins(holder.ID); err == nil {
		for _, ob := range obs {
			if ob.BinID == mouthBin && ob.StepIndex >= 0 && ob.StepIndex < len(steps) {
				r.k = ob.StepIndex
			}
		}
	}
	if r.k < 0 {
		for i, s := range steps {
			if s.Action == protocol.ActionPickup && s.Node == mouthNode {
				r.k = i
				break
			}
		}
	}
	waitPos, seen := -1, 0
	for i, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == holder.WaitIndex {
			waitPos = i
			break
		}
		seen++
	}
	if waitPos < 0 || r.k < 0 || steps[r.k].Action != protocol.ActionPickup {
		return r
	}
	w := steps[waitPos]
	r.unsent = r.k > waitPos
	r.atStation = r.unsent && protocol.IsStationWaitKind(w.WaitKind)
	r.atLaneMark = w.WaitKind == WaitKindLane && w.WaitLane == laneID
	return r
}

// diggerPickup is the index of the digger's pickup the burial was found on:
// the step naming the buried slot, else its pickup from the lane's group. -1
// when none.
func diggerPickup(steps []resolvedStep, buriedSlot, group string) int {
	for i, s := range steps {
		if s.Action == protocol.ActionPickup && s.Node == buriedSlot {
			return i
		}
	}
	for i, s := range steps {
		if s.Action == protocol.ActionPickup && !s.Empty && (s.Node == group || s.Group == group) {
			return i
		}
	}
	return -1
}

// stationWaitBefore reports whether a station wait precedes step j. A spliced
// lane wait does not count: it releases when the lane clears.
func stationWaitBefore(steps []resolvedStep, j int) bool {
	for _, s := range steps[:j] {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			return true
		}
	}
	return false
}

// exchangeCase is everything the decision reads, each fact once: the two
// orders, the two bins and their slots, the lane's group, the holder's plan
// against the fleet and the digger's pickup in its plan.
type exchangeCase struct {
	digger, holder      *orders.Order
	lane                *nodes.Node
	mouth, deep         *bins.Bin
	mouthNode, deepNode *nodes.Node
	group               string
	hr                  exchangeRead
	gSteps              []resolvedStep
	j                   int
}

// readExchange reads the case for a digger and the holder of the mouth bin.
// ok=false when a bin, a slot or the digger's plan cannot be read. Six
// statements: the two bins, their slots, the group, and the holder's junction
// rows (readHolder); the holder is read by the caller.
func (d *Dispatcher) readExchange(digger, holder *orders.Order, lane *nodes.Node, mouthBin, deepBin int64) (exchangeCase, bool) {
	c := exchangeCase{digger: digger, holder: holder, lane: lane, j: -1, hr: exchangeRead{k: -1}}
	m, err1 := d.db.GetBin(mouthBin)
	dp, err2 := d.db.GetBin(deepBin)
	if err1 != nil || err2 != nil || m == nil || dp == nil || m.NodeID == nil || dp.NodeID == nil {
		return c, false
	}
	mn, err1 := d.db.GetNode(*m.NodeID)
	dn, err2 := d.db.GetNode(*dp.NodeID)
	if err1 != nil || err2 != nil || mn == nil || dn == nil {
		return c, false
	}
	c.mouth, c.deep, c.mouthNode, c.deepNode = m, dp, mn, dn
	if lane.ParentID != nil {
		if g, err := d.db.GetNode(*lane.ParentID); err == nil && g != nil {
			c.group = g.Name
		}
	}
	c.hr = d.readHolder(holder, mouthBin, mn.Name, lane.ID)
	steps, ok := decodeSteps(digger.StepsJSON)
	if !ok {
		return c, false
	}
	c.gSteps, c.j = steps, diggerPickup(steps, dn.Name, c.group)
	return c, true
}

// exchange builds the exchange's writes from the case: the holder's step k
// re-pointed to the deep bin's slot, the digger's pickup to the mouth slot
// (keeping its group, so the next pass keeps the choice its hold now names),
// and the ledger notes. ok=false when a plan cannot be rewritten. It decides
// nothing (refusal does); the store transaction re-checks what was read.
func (c exchangeCase) exchange() (store.HeldPickupExchange, bool) {
	var ex store.HeldPickupExchange
	if c.hr.k < 0 || c.j < 0 {
		return ex, false
	}
	hNow := append([]resolvedStep(nil), c.hr.steps...)
	hNow[c.hr.k].Node = c.deepNode.Name
	gNow := append([]resolvedStep(nil), c.gSteps...)
	gNow[c.j].Node = c.mouthNode.Name
	if gNow[c.j].Group == "" {
		gNow[c.j].Group = c.group
	}
	hRaw, hErr := json.Marshal(hNow)
	gRaw, gErr := json.Marshal(gNow)
	if hErr != nil || gErr != nil {
		return ex, false
	}
	h, g, k := c.holder, c.digger, c.hr.k
	return store.HeldPickupExchange{
		HolderID: h.ID, DiggerID: g.ID,
		MouthBin: c.mouth.ID, DeepBin: c.deep.ID,
		MouthNodeID: c.mouthNode.ID, DeepNodeID: c.deepNode.ID, DeepNodeName: c.deepNode.Name,
		HolderWaitIndex: h.WaitIndex, HolderUpdatedAt: h.UpdatedAt,
		HolderStepsWas: h.StepsJSON, HolderStepsNow: string(hRaw),
		DiggerStepsWas: g.StepsJSON, DiggerStepsNow: string(gRaw),
		HolderNote: fmt.Sprintf("pickup at step %d re-pointed from bin %s at %s to bin %s at %s, exchanged with "+
			"order %d, which takes the mouth bin with no dig; this order's robot had not reached the pickup",
			k, c.mouth.Label, c.mouthNode.Name, c.deep.Label, c.deepNode.Name, g.ID),
		DiggerNote: fmt.Sprintf("takes bin %s at the mouth %s from order %d with no dig, exchanged for bin %s at %s",
			c.mouth.Label, c.mouthNode.Name, h.ID, c.deep.Label, c.deepNode.Name),
		MouthNote: fmt.Sprintf("claim exchanged: order %d to order %d", h.ID, g.ID),
		DeepNote:  fmt.Sprintf("claim exchanged: claimed by order %d for its pickup at step %d", h.ID, k),
	}, true
}

// refusal is the policy: "" when the exchange may run. Asked cheapest first;
// only the last question reads (onlyBinInFront, three statements).
//
//	(a) the holder's pickup is unsent;
//	(b) the holder is not parked at the dug lane's mark (it is the nearer
//	    robot, about to lift the mouth bin);
//	the digger has no station wait before its pickup (with one,
//	    the exchange can build the mirror deadlock, and Core cannot see which
//	    side the Edge's gate holds);
//	(d) one part: both bins, the digger's need and the holder's step; neither
//	    step names a bin nor fetches an empty;
//	(c) the mouth bin is the only bin between the mouth and the deep bin.
//
// (e), no other order's hold on either bin, and (f), both orders live, are the
// store transaction's: it reads them under the bins' locks.
func (d *Dispatcher) refusal(c exchangeCase) claimExchangeRefusal {
	hr := c.hr
	switch {
	case hr.k < 0:
		return "the holder's pickup of the mouth bin is not in its plan"
	case !hr.unsent:
		return "the holder's pickup is already with the fleet"
	case hr.atLaneMark:
		return "the holder is parked at this lane's mark"
	case c.j < 0:
		return "the digger's pickup is not in its plan"
	case stationWaitBefore(c.gSteps, c.j):
		return "the digger has a station wait before its pickup"
	}
	hs, gs := hr.steps[hr.k], c.gSteps[c.j]
	need := func(s resolvedStep, o *orders.Order) string {
		if s.PayloadCode != "" {
			return s.PayloadCode
		}
		return o.PayloadCode
	}
	part := c.mouth.PayloadCode
	switch {
	case part == "" || c.deep.PayloadCode != part || need(hs, c.holder) != part || need(gs, c.digger) != part:
		return "the parts differ"
	case hs.BinID != 0 || gs.BinID != 0 || hs.Empty || gs.Empty:
		return "a step names its bin or fetches an empty"
	case !d.onlyBinInFront(c.lane, c.deepNode.ID, c.mouth.ID):
		return "another bin stands between the mouth bin and the digger's"
	}
	return ""
}

// onlyBinInFront reports whether the mouth bin is the one bin standing in a
// slot of the lane shallower than the deep bin's.
func (d *Dispatcher) onlyBinInFront(lane *nodes.Node, deepNodeID, mouthBin int64) bool {
	depth, err := d.db.GetSlotDepth(deepNodeID)
	if err != nil {
		return false
	}
	slots, err := d.db.ListLaneSlots(lane.ID)
	if err != nil {
		return false
	}
	var front []int64
	for _, s := range slots {
		if s.Depth != nil && *s.Depth < depth {
			front = append(front, s.ID)
		}
	}
	inFront, err := d.db.ListBinsByNodes(front)
	if err != nil {
		return false
	}
	return len(inFront) == 1 && inFront[0].ID == mouthBin
}

// tryClaimExchange is handleComplexBurial's arm for a dig refused by a hard
// claim. exchanged: the exchange was written, and the digger is acquired again
// (DispatchPreparedComplex re-runs it once). Otherwise cause and params are
// the digger's wait: dig-blocker-held-at-station naming the holder when the
// holder's next wait before its pickup is a station wait, else the proposer's
// cause, unchanged. Reads: the holder, then readExchange's six, then the
// refusal's three when it gets that far.
func (d *Dispatcher) tryClaimExchange(digger *orders.Order, lane *nodes.Node, buried *BuriedError, res laneClearResult, params QueueParams) (exchanged bool, cause QueueCause, out QueueParams) {
	cause, out = res.blockerCause, params
	holder, err := d.db.GetOrder(res.blockerClaimant)
	if err != nil || holder == nil || protocol.IsTerminal(holder.Status) {
		return false, cause, out
	}
	c, ok := d.readExchange(digger, holder, lane, res.blockerBin, buried.Bin.ID)
	if c.hr.atStation {
		cause, out = CauseDigBlockerHeldAtStation, params.withStationHolder(holder.ID)
	}
	why := claimExchangeRefusal("a bin, its slot or the digger's plan cannot be read")
	if ok {
		why = d.refusal(c)
	}
	if why == "" {
		ex, built := c.exchange()
		if !built {
			why = "the plans cannot be rewritten"
		} else if done, xErr := d.db.ExchangeHeldPickup(ex); !done {
			why = claimExchangeRefusal(fmt.Sprint(xErr))
		} else {
			log.Printf("dispatch: claim exchange: order %d takes bin %d at the mouth of %s from order %d, whose pickup "+
				"at step %d is re-pointed to bin %d — no dig", digger.ID, res.blockerBin, lane.Name, holder.ID, c.hr.k,
				buried.Bin.ID)
			return true, cause, out
		}
	}
	d.dbg("dispatch: no claim exchange for order %d with order %d on %s: %s", digger.ID, holder.ID, lane.Name, why)
	return false, cause, out
}
