package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// complex_reserve.go — the 1c plan-time reserve/confirm split (commit 4, D39).
//
// The old ApplyComplexPlan claimed a complex order's bins in one live re-walk:
// list-fresh + claim per pickup step, all-or-classify-zero. Commit 4 makes the
// plan durable across ticks instead: reserveComplexPlan soft-holds the order's
// distinct source bins (reconciling against what it already holds), the order
// stays `sourcing` while any distinct bin is still missing, and confirmComplexPlan
// commits the COMPLETE reserved set to hard claims (apply-as-confirm — no live
// re-walk). GO is gated on a complete distinct-bin set (D5): a robot never starts
// a job it can't finish; give-up is operator-driven (D18-Q4), never a timer.

// reservedPickup is one TRUE-source pickup step matched to a reserved bin by the
// reconcile: the step index (recorded in order_bins.step_index), the pickup node,
// the reserved bin, and whether the reservation is already confirmed (already
// claimed by this order on a prior tick, so confirm skips the re-claim).
type reservedPickup struct {
	stepIndex int
	nodeName  string
	binID     int64
	confirmed bool
}

// reserveOutcome is the closed disposition of a reserve attempt.
type reserveOutcome int

const (
	// reserveComplete — every distinct need is reserved; go confirm + dispatch.
	reserveComplete reserveOutcome = iota
	// reserveHolding — a need is still missing but its source is sourceable
	// eventually (bins present-but-taken, or a node that may be restocked/relayed
	// into) — hold the partials and retry (D5/D18-Q4).
	reserveHolding
	// reserveMoot — the order reserved NOTHING and every missing need's node is
	// genuinely empty (no bins). This is the old "no_source_bin" case (e.g. a swap
	// evac whose line bin was removed to quality hold before dispatch): the work is
	// void, so skip it (Edge's HandleOrderSkipped advances the linked changeover
	// task) rather than hold forever. A moot evac is not "demand," so D18-Q4's
	// hold-and-retry does not apply.
	reserveMoot
)

// heldReservation is a reservation the order already holds, resolved to its bin's
// current node + empty-status so the reconcile can match it to a need by DATA.
type heldReservation struct {
	binID     int64
	nodeName  string
	empty     bool
	confirmed bool
	used      bool
}

// reserveComplexPlan reconciles the order's held reservations against its distinct
// source needs (D5) and returns the per-need bin assignment plus complete=true iff
// EVERY distinct need is covered.
//
//   - keep:    a held bin still matching a need — reused, no re-Acquire.
//   - acquire: a fresh available bin for an unmatched need — Acquired now.
//   - release: a held bin matching no current need (a re-resolution moved a
//     source) — coupled release if it was confirmed, plain release if pending.
//
// It is OWNER-AWARE: it loads the order's own holds first and matches them by
// (node, empty-status), NOT through the owner-blind BinUnavailableReason, which
// would reject the order's own reservations and report them missing every tick —
// the reservations unique index is per-BIN, so re-Acquiring an own hold conflicts
// on its own row (see reservations.Acquire's doc, THE landmine). Occupancy (one
// bin per node) keeps the (node, empty-status) match key unambiguous.
//
// On incomplete, the order holds its partial reservations and the caller keeps it
// in `sourcing` for the scanner to retry. Pure of transitions/claims — the hard
// claim happens later, in confirmComplexPlan.
func (d *Dispatcher) reserveComplexPlan(order *orders.Order, plan *ComplexPlan) (assigned []reservedPickup, outcome reserveOutcome, err error) {
	pickups := complexPickups(plan.ResolvedSteps)

	rows, err := d.db.ListReservationsByOrder(order.ID)
	if err != nil {
		return nil, reserveHolding, fmt.Errorf("list reservations for order %d: %w", order.ID, err)
	}
	held := d.resolveHeldReservations(rows)

	missing := 0
	anyMissWithBins := false // a missing need whose node had bins (present-but-taken → sourceable)
	for _, pk := range pickups {
		// 1. Reuse a held bin that already satisfies this pickup (same node, same
		//    empty-status). Owner-aware — does not go through BinUnavailableReason.
		if hb := matchHeldToNeed(held, pk.step); hb != nil {
			hb.used = true
			assigned = append(assigned, reservedPickup{
				stepIndex: pk.stepIndex, nodeName: pk.step.Node, binID: hb.binID, confirmed: hb.confirmed,
			})
			continue
		}
		// 2. Acquire a fresh available bin at the node. findAvailableForNeed uses
		//    BinUnavailableReason — owner-blind is CORRECT here: it skips every
		//    reserved bin (ours and others'), so we only ever Acquire an UNreserved
		//    bin, which cannot self-conflict.
		bin, nodeHadBins, ferr := d.findAvailableForNeed(pk.step, order.PayloadCode)
		if ferr != nil {
			return nil, reserveHolding, ferr
		}
		if bin == nil {
			// D5 relay = potential relay (earlier dropoff at N) AND node empty at
			// reserve. An empty potential-relay node is the relay target (its bin
			// hasn't relayed there yet) — skip, not a miss. A node WITH bins is a
			// real source (bin!=nil would have reserved it), so bin==nil with bins
			// present means present-but-taken → a genuine miss.
			if pk.potentialRelay && !nodeHadBins {
				continue
			}
			missing++
			if nodeHadBins {
				anyMissWithBins = true // bins present but unavailable — sourceable eventually
			}
			continue
		}
		aerr := d.binManifest.ReserveForDispatch(bin.ID, order.ID)
		if errors.Is(aerr, reservations.ErrReservationConflict) {
			missing++
			anyMissWithBins = true // the bin exists, another order holds it — retry next tick
			continue
		}
		if aerr != nil {
			return nil, reserveHolding, fmt.Errorf("reserve order=%d bin=%d: %w", order.ID, bin.ID, aerr)
		}
		d.db.AppendAudit("bin", bin.ID, "reserved", "",
			fmt.Sprintf("complex order %d reserve at %s", order.ID, pk.step.Node), "system")
		assigned = append(assigned, reservedPickup{
			stepIndex: pk.stepIndex, nodeName: pk.step.Node, binID: bin.ID, confirmed: false,
		})
	}

	// Release held bins not matched to any current need.
	for _, hb := range held {
		if hb.used {
			continue
		}
		if hb.confirmed {
			// Claim + reservation together (a re-resolution abandoned a confirmed
			// source before dispatch — rare, but keep them coupled).
			if rerr := d.db.ReleaseClaimForBin(hb.binID, order.ID); rerr != nil {
				log.Printf("dispatch: reserve release-claim order=%d bin=%d: %v", order.ID, hb.binID, rerr)
			}
		} else if rerr := d.db.ReleaseReservation(order.ID, hb.binID); rerr != nil {
			log.Printf("dispatch: reserve release order=%d bin=%d: %v", order.ID, hb.binID, rerr)
		}
	}

	switch {
	case missing == 0:
		return assigned, reserveComplete, nil
	case len(assigned) == 0 && !anyMissWithBins:
		// Reserved nothing and every missing need's node is genuinely empty — the
		// order's work is moot (source removed), not merely momentarily unsourceable.
		return assigned, reserveMoot, nil
	default:
		return assigned, reserveHolding, nil
	}
}

// resolveHeldReservations loads each held reservation's bin and current node so
// the reconcile can match by (node, empty-status). A bin sits at its source node
// until the order dispatches; an unresolvable one is left with an empty node so it
// matches nothing and gets released as a stray.
func (d *Dispatcher) resolveHeldReservations(rows []reservations.Reservation) []*heldReservation {
	out := make([]*heldReservation, 0, len(rows))
	for _, r := range rows {
		hb := &heldReservation{binID: r.BinID, confirmed: r.State == "confirmed"}
		if b, err := d.db.GetBin(r.BinID); err == nil && b != nil {
			hb.empty = b.PayloadCode == ""
			if b.NodeID != nil {
				if node, nerr := d.db.GetNode(*b.NodeID); nerr == nil && node != nil {
					hb.nodeName = node.Name
				}
			}
		}
		out = append(out, hb)
	}
	return out
}

// matchHeldToNeed returns the first unused held bin that satisfies the need —
// same node, same empty-status. Owner-aware (the held bin is the order's own).
func matchHeldToNeed(held []*heldReservation, step resolvedStep) *heldReservation {
	for _, hb := range held {
		if hb.used || hb.nodeName == "" {
			continue
		}
		if hb.nodeName == step.Node && hb.empty == step.Empty {
			return hb
		}
	}
	return nil
}

// findAvailableForNeed returns the first UNRESERVED, claimable bin at the need's
// node (empty carrier for an empty leg, else a payload match) — the same per-step
// selection the old live claim path made, minus the claim. Returns nil when the
// node has no available bin yet (the reconcile counts it missing and retries).
// nodeHadBins reports whether the node held ANY bins (before the empty/payload
// filter) — the reconcile uses it to tell "genuinely empty node" (contributes to
// a moot outcome) from "bins present but unavailable" (sourceable, hold + retry).
func (d *Dispatcher) findAvailableForNeed(step resolvedStep, payloadCode string) (bin *binsstore.Bin, nodeHadBins bool, err error) {
	node, err := d.db.GetNodeByDotName(step.Node)
	if err != nil || node == nil {
		return nil, false, nil // node unresolvable → treat as empty (miss), retry next tick
	}
	bins, err := d.db.ListBinsByNode(node.ID)
	if err != nil {
		return nil, false, fmt.Errorf("list bins at %s: %w", step.Node, err)
	}
	nodeHadBins = len(bins) > 0
	candidates := bins
	claimPayload := payloadCode
	if step.Empty {
		// Empty pickup leg (produce's "bring an empty to fill"): an empty carrier,
		// dropping the payload context — same filter as the old claim path.
		candidates = emptyBinsOnly(bins)
		claimPayload = ""
	}
	for _, b := range candidates {
		if BinUnavailableReason(b, claimPayload) == "" {
			return b, nodeHadBins, nil
		}
	}
	return nil, nodeHadBins, nil
}

// confirmComplexPlan commits a COMPLETE reserved assignment to hard claims — the
// apply-as-confirm half of the split. For each reserved pickup it ConfirmClaims the
// bin (claim under the demoted-CAS seatbelt + confirm the reservation), then
// records order.BinID and the order_bins junction exactly as the old live-claim
// path did. It never re-walks live candidates: the bins were chosen at reserve.
//
// Idempotency (crash / partial-confirm-then-requeue): a reserved pickup already
// confirmed AND claimed by THIS order is skipped (re-claiming would false-fail the
// claimed_by IS NULL seatbelt). A confirmed reservation whose bin is claimed by
// anyone else is NOT ours — it falls through to ConfirmClaim and fails claim_failed.
//
// A ConfirmClaim failure (the pending reservation was reaped, or the bin was
// claimed by another order) returns codeClaimFailed so the caller requeues and
// reconciles next tick — no bin is ever claimed without its reservation.
func (d *Dispatcher) confirmComplexPlan(order *orders.Order, plan *ComplexPlan, assigned []reservedPickup) error {
	steps := plan.ResolvedSteps
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}

	var claimed []claimedBin
	for _, rp := range assigned {
		// Is the bin ALREADY hard-claimed by THIS order? A prior tick may have
		// claimed it and crashed/errored before confirming its reservation (the D45
		// wedge half-state: claimed_by=order, reservation still pending). Check by
		// DATA, not by rp.confirmed — a claimed-but-pending bin is still ours.
		claimedByUs := false
		if b, gerr := d.db.GetBin(rp.binID); gerr == nil && b != nil &&
			b.ClaimedBy != nil && *b.ClaimedBy == order.ID {
			claimedByUs = true
		}
		if claimedByUs {
			// Already ours — never re-claim (re-claiming would risk a false 0-rows
			// failure). If the reservation is still pending, confirm it in place;
			// if already confirmed this is a no-op. The bin is not re-touched.
			if !rp.confirmed {
				if err := d.binManifest.ConfirmHeldReservation(order.ID, rp.binID); err != nil {
					return &planningError{
						Code:   codeClaimFailed,
						Detail: fmt.Sprintf("confirm held reservation bin %d for order %d: %v", rp.binID, order.ID, err),
					}
				}
			}
		} else {
			// Not ours (unclaimed, or claimed by ANOTHER order — which fails the
			// demoted-CAS seatbelt with 0 rows ⇒ claim_failed, unchanged).
			// RemainingUOP is nil for complex intake (Edge threads it at release,
			// not intake) — same as the old ApplyComplexPlan call.
			if err := d.binManifest.ConfirmClaim(rp.binID, order.ID, nil); err != nil {
				return &planningError{
					Code:   codeClaimFailed,
					Detail: fmt.Sprintf("confirm claim bin %d for order %d: %v", rp.binID, order.ID, err),
				}
			}
			d.db.AppendAudit("bin", rp.binID, "claimed", "",
				fmt.Sprintf("complex order %d confirm at %s", order.ID, rp.nodeName), "system")
		}
		claimed = append(claimed, claimedBin{binID: rp.binID, stepIndex: rp.stepIndex, nodeName: rp.nodeName})
	}

	if len(claimed) == 0 {
		// A complete reserve with zero needs is a malformed complex order (no true
		// source pickup) — surface it rather than dispatch a bin-less order.
		return &planningError{Code: codeNoBin, Detail: fmt.Sprintf("complex order %d has no source pickup", order.ID)}
	}

	// order.BinID tracks the bin claimed at the process node, else the first —
	// the index over the bins we actually claimed (same as ApplyComplexPlan).
	primaryIdx := 0
	for i, cb := range claimed {
		if cb.nodeName == processNode {
			primaryIdx = i
			break
		}
	}
	order.BinID = &claimed[primaryIdx].binID
	if err := d.db.UpdateOrderBinID(order.ID, claimed[primaryIdx].binID); err != nil {
		d.dbg("complex confirm: WARNING order %d UpdateOrderBinID(bin=%d) failed: %v",
			order.ID, claimed[primaryIdx].binID, err)
	}

	// Multi-bin: per-bin destinations in order_bins, resolved from the bins we
	// claimed (a sibling fallback would change which bin lands where). Compound
	// children never use the junction — each is a single-bin legacy-path order.
	if len(claimed) > 1 && order.ParentOrderID == nil {
		claimedMap := make(map[string]int64, len(claimed))
		for _, cb := range claimed {
			claimedMap[cb.nodeName] = cb.binID
		}
		destinations := resolvePerBinDestinations(steps, claimedMap)
		for _, cb := range claimed {
			destNode := destinations[cb.binID]
			if err := d.db.InsertOrderBin(order.ID, cb.binID, cb.stepIndex, protocol.ActionPickup, cb.nodeName, destNode); err != nil {
				log.Printf("dispatch: insert order_bin for order %d bin %d: %v", order.ID, cb.binID, err)
			}
		}
		log.Printf("dispatch: complex order %d has %d pickups — per-bin destinations recorded in order_bins",
			order.ID, len(claimed))
	} else if len(claimed) > 1 {
		log.Printf("dispatch: complex order %d has %d pickups — Order.BinID tracks first bin %d only (compound child, no junction)",
			order.ID, len(claimed), claimed[0].binID)
	}
	return nil
}
