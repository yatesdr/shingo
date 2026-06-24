package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
)

// ApplyComplexPlan claims bins for a complex order from a previously computed
// ComplexPlan. It is the single claim path for complex orders, producing the
// durable state the completion flow depends on — claimed bins, Order.BinID, and
// the order_bins junction rows.
//
// Architecture: the plan supplies ordering and intent (the resolved step
// sequence, which pickup is the process node, the per-bin destination model);
// Apply re-walks the LIVE candidate list at each pickup node and claims against
// fresh state. The bin a plan selected is a PREDICTION, not a command:
//
//   - The planner is purely read-only (BinUnavailableReason only) and records
//     one candidate per step with no alternatives. Claiming plan.BinClaims[i]
//     literally would fail any step whose predicted bin was taken since the
//     snapshot, where the live loop recovers from a sibling bin.
//   - The planner builds its claimed-map after selection, so two pickups at the
//     same node both predict the same first-available bin. The live loop cannot
//     double-pick because step 1's claim makes step 2's listing show that bin
//     as taken.
//
// So Apply lists each node fresh (ListBinsByNode) and claims with
// claimFirstAvailable, sequentially, so each claim consumes its bin for later
// steps — identical per-tick atomicity to the live loop. The race signal, the
// three-way terminal disposition, and the primary-bin index are all re-derived
// from Apply's OWN claim attempts, never from plan.Skips: a planner skip can say
// "no bins at node" but can never carry "claim CAS lost", so driving the
// classifier off the plan would misroute a real race (claim_failed -> requeue)
// to a terminal no_bin failure — the regression at the heart of the
// ALN_002 -> SMN_003 silent-claim-failure class.
//
// Bin-claim is the last side effect (gates and slot-claims run earlier in the
// caller); on a later failure the order's terminal cleanup unclaims, same as the
// live loop. There is no mid-apply rollback.
func (d *Dispatcher) ApplyComplexPlan(order *orders.Order, plan *ComplexPlan, payloadCode string, remainingUOP *int) error {
	// processNode names the line node whose claim drives this order — set
	// explicitly for swap orders, else the source node. Only the bin claimed
	// there receives the operator's RemainingUOP signal; it is also the bin
	// written to Order.BinID for late-bind manifest sync.
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}

	var (
		claimed     []claimedBin
		pickupSteps int
		stepSkips   []pickupSkip
		// anyRaced reports whether at least one pickup step lost a SQL claim
		// race — a candidate that passed BinUnavailableReason on read but lost
		// ClaimForDispatch under the claimed_by IS NULL guard. It is the signal
		// that discriminates a transient (requeue) from a structural (terminal)
		// failure when nothing was claimed.
		anyRaced bool
	)

	for i, c := range plan.ResolvedSteps {
		if c.Action != protocol.ActionPickup {
			continue
		}
		pickupSteps++
		stepIndex := i
		node, err := d.db.GetNodeByDotName(c.Node)
		if err != nil {
			reason := fmt.Sprintf("cannot resolve node %s: %v", c.Node, err)
			log.Printf("complex apply: order %d pickup step %d at %s — %s", order.ID, stepIndex, c.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{stepIndex, c.Node, reason})
			continue
		}
		bins, err := d.db.ListBinsByNode(node.ID)
		if err != nil {
			reason := fmt.Sprintf("ListBinsByNode failed: %v", err)
			log.Printf("complex apply: order %d pickup step %d at %s — %s", order.ID, stepIndex, c.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{stepIndex, c.Node, reason})
			continue
		}
		if len(bins) == 0 {
			reason := emptyNodeSkipReason
			log.Printf("complex apply: order %d pickup step %d at %s — %s", order.ID, stepIndex, c.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{stepIndex, c.Node, reason})
			continue
		}

		// Only the process-node (outgoing) bin receives the operator's
		// remaining-UOP; storage pickups get a plain claim.
		var stepUOP *int
		if c.Node == processNode {
			stepUOP = remainingUOP
		}

		// Empty pickup leg (produce node's "bring an empty to fill"): claim an
		// EMPTY carrier and drop the payload context. BinUnavailableReason
		// accepts both an empty and a payload-matching full, so without this
		// filter the walk could grab a full of the part bound for a produce
		// node — exactly the wrong bin.
		candidates := bins
		claimPayload := payloadCode
		if c.Empty {
			candidates = emptyBinsOnly(bins)
			claimPayload = ""
			if len(candidates) == 0 {
				reason := "no empty carrier at node for empty pickup leg"
				log.Printf("complex apply: order %d pickup step %d at %s — %s", order.ID, stepIndex, c.Node, reason)
				stepSkips = append(stepSkips, pickupSkip{stepIndex, c.Node, reason})
				continue
			}
		}

		picked, rejects, raced := claimFirstAvailable(candidates, claimPayload, func(b *binsstore.Bin) error {
			return d.binManifest.ClaimForDispatch(b.ID, order.ID, stepUOP)
		})
		if raced {
			anyRaced = true
		}
		if picked == nil {
			reason := fmt.Sprintf("no candidate among %d bin(s); rejects: [%s]",
				len(bins), joinRejects(rejects))
			log.Printf("complex apply: order %d pickup step %d at %s — %s", order.ID, stepIndex, c.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{stepIndex, c.Node, reason})
			continue
		}
		d.dbg("complex apply: claimed bin %d (%s) at %s for order %d", picked.ID, picked.Label, c.Node, order.ID)
		d.db.AppendAudit("bin", picked.ID, "claimed",
			"", fmt.Sprintf("complex order %d pickup at %s", order.ID, c.Node), "system")
		claimed = append(claimed, claimedBin{binID: picked.ID, stepIndex: stepIndex, nodeName: c.Node})
	}

	if len(claimed) == 0 {
		// Same three-way discrimination as the live loop, re-derived from
		// Apply's own attempts: a lost race is retry-eligible (claim_failed),
		// a genuinely empty source is a no-op (no_source_bin -> Skip), and bins
		// present but unclaimable is terminal (no_bin -> Fail).
		if anyRaced {
			return asPlanningError(nil, fmt.Sprintf("lost claim race at all pickup nodes for order %d", order.ID))
		}
		if allStepSkipsAreEmptyNode(stepSkips) {
			return &planningError{Code: codeNoSourceBin, Detail: fmt.Sprintf("no bin at pickup node(s) for order %d — source was emptied externally", order.ID)}
		}
		return &planningError{Code: codeNoBin, Detail: fmt.Sprintf("no available bin at pickup node(s) for order %d", order.ID)}
	}

	// Partial coverage: surface the missed steps loudly so the late-bind
	// manifest fallback has a paired diagnostic instead of being the only signal.
	if len(stepSkips) > 0 {
		log.Printf("complex apply: order %d claimed %d/%d pickup step(s); %d step(s) missed: %v",
			order.ID, len(claimed), pickupSteps, len(stepSkips), stepSkipSummaries(stepSkips))
	}

	// Order.BinID tracks the bin claimed at the process node when one was
	// claimed there, else the first claimed bin — the index walks the bins
	// Apply actually claimed, not the plan's predicted set.
	primaryIdx := 0
	for i, cb := range claimed {
		if cb.nodeName == processNode {
			primaryIdx = i
			break
		}
	}
	order.BinID = &claimed[primaryIdx].binID
	if err := d.db.UpdateOrderBinID(order.ID, claimed[primaryIdx].binID); err != nil {
		d.dbg("complex apply: WARNING order %d UpdateOrderBinID(bin=%d) failed — order.BinID will read NULL on next load: %v",
			order.ID, claimed[primaryIdx].binID, err)
	}

	// Multi-bin: record per-bin destinations in order_bins, resolved from the
	// bins Apply actually claimed (a sibling fallback changes which bin lands
	// where, so the destination map must be built from the real claims).
	// Compound children never use the junction — each child is a single-bin
	// order handled by the legacy path.
	if len(claimed) > 1 && order.ParentOrderID == nil {
		claimedMap := make(map[string]int64, len(claimed))
		for _, cb := range claimed {
			claimedMap[cb.nodeName] = cb.binID
		}
		destinations := resolvePerBinDestinations(plan.ResolvedSteps, claimedMap)
		for _, cb := range claimed {
			destNode := destinations[cb.binID]
			if err := d.db.InsertOrderBin(order.ID, cb.binID, cb.stepIndex, protocol.ActionPickup, cb.nodeName, destNode); err != nil {
				log.Printf("dispatch: insert order_bin for order %d bin %d: %v", order.ID, cb.binID, err)
			}
		}
		log.Printf("dispatch: complex order %d has %d pickups — per-bin destinations recorded in order_bins",
			order.ID, len(claimed))
	} else if len(claimed) > 1 {
		log.Printf("dispatch: complex order %d has %d pickups — Order.BinID tracks first bin %d only (compound child, no junction table)",
			order.ID, len(claimed), claimed[0].binID)
	}
	return nil
}
