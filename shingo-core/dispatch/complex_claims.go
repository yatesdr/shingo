package dispatch

import (
	"fmt"
	"log"

	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
)

// claimComplexBins resolves and claims bins for pickup steps in a complex order.
// For single-pickup orders (the most common pattern), it sets Order.BinID so
// that the normal completion flow — ApplyBinArrival (moves bin to delivery
// node in the DB) and maybeCreateReturnOrder (auto-return on cancel/fail) —
// works correctly.
//
// For multi-pickup orders, per-bin destinations are computed via
// resolvePerBinDestinations and recorded in the order_bins junction table.
// handleOrderCompleted uses these rows to move each bin to its correct
// destination instead of blindly using Order.DeliveryNode.
//
// The claim is best-effort: if no unclaimed bin matching the payload is found
// at a pickup node, the order still dispatches (same as prior behavior).
//
// Compound order children (ParentOrderID != nil) never populate the junction
// table — each child is a single-bin order handled by the legacy path.
func (d *Dispatcher) claimComplexBins(order *orders.Order, steps []resolvedStep, payloadCode string, remainingUOP *int) error {
	// processNode names the line node whose claim drives this order — the
	// node where the operator releases / confirms and where the bin used
	// for late-bind manifest sync lives. Edge sets it explicitly via
	// ComplexOrderRequest.ProcessNode (= claim.CoreNodeName) for swap
	// orders; falls back to SourceNode for orders without a distinct line
	// node (the conventional "first pickup is the line bin" pattern).
	//
	// Pre-fix this used SourceNode unconditionally, so for swap orders that
	// pick up at InboundSource (= SourceNode) and pick up *again* at the
	// line, only the inbound bin got remainingUOP and order.BinID. The
	// operator's release-time RemainingUOP=0 then cleared the wrong bin
	// (the full inbound), and that bin landed at the line with manifest=0.
	// Plant 2026-04: "bin lineside reset to 0 after one-robot swap".
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}

	// Per-step skip-reason capture. We track every pickup step and record a
	// reason if no bin was claimed for it. Surfaced via log.Printf below so
	// production logs explain WHY a step missed (already-claimed bin, payload
	// mismatch, ClaimForDispatch SQL guard fail, no bins at node, etc.) —
	// previously this was silent and produced the ALN_002 → SMN_003 incident
	// (2026-04-23) where order.BinID stayed nil and the release-time manifest
	// sync silently fell through to the source-node fallback.
	var (
		claimed     []claimedBin
		pickupSteps int
		stepSkips   []pickupSkip
		// anyRaced reports whether at least one pickup step lost a SQL
		// claim race (BinUnavailableReason passed but ClaimForDispatch
		// failed under the WHERE claimed_by IS NULL guard). Used to
		// discriminate transient (re-queue) from structural (terminal)
		// failures when no claim succeeded — see #4 in the UOP audit.
		anyRaced bool
	)

	for i, s := range steps {
		if s.Action != "pickup" {
			continue
		}
		pickupSteps++
		node, err := d.db.GetNodeByDotName(s.Node)
		if err != nil {
			reason := fmt.Sprintf("cannot resolve node %s: %v", s.Node, err)
			d.dbg("complex: order %d pickup step %d at %s — %s", order.ID, i, s.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{i, s.Node, reason})
			continue
		}
		bins, err := d.db.ListBinsByNode(node.ID)
		if err != nil {
			reason := fmt.Sprintf("ListBinsByNode failed: %v", err)
			d.dbg("complex: order %d pickup step %d at %s — %s", order.ID, i, s.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{i, s.Node, reason})
			continue
		}
		if len(bins) == 0 {
			reason := emptyNodeSkipReason
			d.dbg("complex: order %d pickup step %d at %s — %s", order.ID, i, s.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{i, s.Node, reason})
			continue
		}

		// Only apply remainingUOP at the process node (outgoing bin).
		// Storage pickups and other steps get a plain claim (nil).
		var stepUOP *int
		if s.Node == processNode {
			stepUOP = remainingUOP
		}
		picked, rejects, raced := claimFirstAvailable(bins, payloadCode, func(b *binsstore.Bin) error {
			return d.binManifest.ClaimForDispatch(b.ID, order.ID, stepUOP)
		})
		if raced {
			anyRaced = true
		}
		if picked == nil {
			reason := fmt.Sprintf("no candidate among %d bin(s); rejects: [%s]",
				len(bins), joinRejects(rejects))
			d.dbg("complex: order %d pickup step %d at %s — %s",
				order.ID, i, s.Node, reason)
			stepSkips = append(stepSkips, pickupSkip{i, s.Node, reason})
			continue
		}
		d.dbg("complex: claimed bin %d (%s) at %s for order %d",
			picked.ID, picked.Label, s.Node, order.ID)
		d.db.AppendAudit("bin", picked.ID, "claimed",
			"", fmt.Sprintf("complex order %d pickup at %s", order.ID, s.Node), "system")
		claimed = append(claimed, claimedBin{binID: picked.ID, stepIndex: i, nodeName: s.Node})
	}

	if len(claimed) == 0 {
		// Discriminate three terminal cases by the per-step skip reasons
		// already captured in stepSkips:
		//   - claim_failed: at least one step lost a SQL claim race (the
		//     bin existed and matched, but ClaimForDispatch failed under
		//     the claimed_by IS NULL guard). Retry-eligible: a winning
		//     order's completion or release frees the bin for the next
		//     scanner tick.
		//   - no_source_bin: every step reported "no bins at node" — the
		//     source nodes are genuinely empty. This is the "work was
		//     never needed" condition (bin removed externally before
		//     dispatch, e.g. quality hold). DispatchPreparedComplex routes
		//     this to lifecycle.Skip instead of Fail so the operator
		//     surface treats it as a no-op rather than an alarm.
		//   - no_bin: bins existed but were rejected for other reasons
		//     (already claimed, payload mismatch, status). Terminal
		//     failure — operator must reconcile.
		if anyRaced {
			return &planningError{Code: "claim_failed", Detail: fmt.Sprintf("lost claim race at all pickup nodes for order %d", order.ID)}
		}
		if allStepSkipsAreEmptyNode(stepSkips) {
			return &planningError{Code: "no_source_bin", Detail: fmt.Sprintf("no bin at pickup node(s) for order %d — source was emptied externally", order.ID)}
		}
		return &planningError{Code: "no_bin", Detail: fmt.Sprintf("no available bin at pickup node(s) for order %d", order.ID)}
	}

	// Order proceeded with claims for some steps but missed others. This is
	// the silent-failure path that produces order.BinID-correct-but-misleading
	// or order.BinID-nil-on-the-relevant-step. Surface it loudly so the
	// late-bind manifest fallback (HandleOrderRelease's findFallbackBinAtSource)
	// has a paired diagnostic in the log instead of being the only signal that
	// something went wrong.
	if len(stepSkips) > 0 {
		d.dbg("complex: order %d claimed %d/%d pickup step(s); %d step(s) missed: %v",
			order.ID, len(claimed), pickupSteps, len(stepSkips), stepSkipSummaries(stepSkips))
	}

	// Set Order.BinID to the bin claimed at the process (line) node when
	// one was claimed there — that's the bin the operator releases at the
	// HMI, and HandleOrderRelease syncs its manifest. For non-swap orders
	// (no process node distinct from source) and for orders where the
	// process-node pickup was skipped, fall back to the first claimed bin
	// — the legacy behavior that was correct for single-pickup orders.
	primaryIdx := 0
	for i, c := range claimed {
		if c.nodeName == processNode {
			primaryIdx = i
			break
		}
	}
	order.BinID = &claimed[primaryIdx].binID
	if err := d.db.UpdateOrderBinID(order.ID, claimed[primaryIdx].binID); err != nil {
		// Second silent path the late-bind fallback was working around: an
		// in-memory order.BinID that never made it to the DB row. Surface as
		// WARNING so it stands out from the per-step skip lines above.
		d.dbg("complex: WARNING order %d UpdateOrderBinID(bin=%d) failed — order.BinID will read NULL on next load: %v",
			order.ID, claimed[primaryIdx].binID, err)
	}

	// Multi-bin: populate the order_bins junction table with per-bin destinations.
	// Compound children never use this — each child is a single-bin order.
	if len(claimed) > 1 && order.ParentOrderID == nil {
		// Build the claimedBins map for destination resolution: pickupNode → binID
		claimedMap := make(map[string]int64, len(claimed))
		for _, c := range claimed {
			claimedMap[c.nodeName] = c.binID
		}

		destinations := resolvePerBinDestinations(steps, claimedMap)

		for _, c := range claimed {
			destNode := destinations[c.binID]
			if err := d.db.InsertOrderBin(order.ID, c.binID, c.stepIndex, "pickup", c.nodeName, destNode); err != nil {
				log.Printf("dispatch: insert order_bin for order %d bin %d: %v", order.ID, c.binID, err)
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

// resolvePerBinDestinations simulates the step sequence to determine where each
// claimed bin ends up after all pickups and dropoffs complete. The bin identity
// is tracked by location: a pickup at node X grabs whichever bin was last
// dropped there.
//
// Returns a map of binID → final destination node name.
//
// Edge cases handled:
//   - Empty robot dropoff (pre-positioning): carrying == 0, dropoff is a no-op
//   - Ghost pickup (no bin at node): carrying stays 0
//   - Bin re-pickup: a bin dropped at staging then picked up again gets a new dest
func resolvePerBinDestinations(steps []resolvedStep, claimedBins map[string]int64) map[int64]string {
	// Which bin the robot is currently carrying (0 = empty)
	var carrying int64

	// Which bin is sitting at which node after being dropped
	binAtNode := make(map[string]int64, len(claimedBins))
	for nodeName, binID := range claimedBins {
		binAtNode[nodeName] = binID
	}

	// Last known dropoff destination per bin
	dest := make(map[int64]string, len(claimedBins))

	for _, step := range steps {
		switch step.Action {
		case "pickup":
			if binID, ok := binAtNode[step.Node]; ok {
				carrying = binID
				delete(binAtNode, step.Node) // bin leaves this node
			}
			// If no bin at this node, robot picks up nothing (ghost/pre-position)

		case "dropoff":
			if carrying != 0 {
				dest[carrying] = step.Node       // update final dest
				binAtNode[step.Node] = carrying  // bin is now at this node
				carrying = 0
			}
			// If robot is empty, this is a pre-position drive (no-op for bin tracking)

		case "wait":
			// No bin movement
		}
	}

	return dest
}
