package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/service"
	"shingocore/store"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// allocator.go — the reservation Allocator (D16). Extracted from the Dispatcher in
// 1d commit 5: it owns the plan-time reserve/confirm reconcile for BOTH resource
// kinds (bin pickups + destination slots). The Dispatcher keeps the gates,
// re-resolution, lifecycle/MoveToSourcing, fleet dispatch, compound orchestration,
// and queue_reason; it delegates the reserve/confirm to d.allocator. Pure lift — no
// behavior change.
//
// The 1c reserve/confirm split (commit 4, D39): the old ApplyComplexPlan claimed a
// complex order's bins in one live re-walk. Now the plan is durable across ticks —
// reserveComplexPlan/reserveComplexSlots soft-hold the order's distinct source bins
// and destination slots (reconciling against what it already holds), the order stays
// `sourcing` while any distinct need is still missing, and confirmComplexPlan commits
// the COMPLETE reserved set to hard claims (apply-as-confirm — no live re-walk). GO is
// gated on a complete set (D5): a robot never starts a job it can't finish; give-up is
// operator-driven (D18-Q4), never a timer.

// Allocator reserves and confirms a complex order's resources against live state. It
// holds only what the reconcile needs — the store, the bin-manifest service, and a
// debug logger — so the future kind-agnostic Claim aggregate (D45 §4) is a lift from
// here, not from the whole Dispatcher.
type Allocator struct {
	db          *store.DB
	binManifest *service.BinManifestService
	dbg         func(string, ...any)
}

func newAllocator(db *store.DB, binManifest *service.BinManifestService, dbg func(string, ...any)) *Allocator {
	return &Allocator{db: db, binManifest: binManifest, dbg: dbg}
}

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

// heldReservation is a reservation the order already holds, resolved so the
// reconcile can match it to a need by DATA. A bin row carries its bin's current
// node + empty-status; a slot row carries the reserved node directly.
type heldReservation struct {
	kind      reservations.Kind
	binID     int64  // bin rows: the reserved bin
	nodeID    int64  // slot rows: the reserved node
	nodeName  string // bin: the bin's current node name; slot: the reserved node's name
	empty     bool   // bin rows only
	confirmed bool
	used      bool
}

// slotNeed is one concrete storage-dropoff step that needs a destination slot
// reservation — the same node set the old hard-claim slot loop iterated. group is
// the NGRP origin ("" for a fixed-concrete dropoff), used to revert-and-re-resolve
// a fungible slot on conflict.
type slotNeed struct {
	stepIndex int
	nodeName  string
	group     string
}

// slotNeeds returns the concrete storage-dropoff slots an order must reserve —
// exactly the set the retired ClaimSlot loop iterated (isConcreteStorageDropoff
// dropoffs, staging/relay included). Ordering is step order; the reconcile does not
// need the canonical node-ID sort the hard loop used (the ABBA class dissolves at
// the soft-acquire layer, D43 — a loser backs off holding revocable reservations).
func (a *Allocator) slotNeeds(steps []resolvedStep) []slotNeed {
	var out []slotNeed
	for i := range steps {
		s := steps[i]
		if s.Action != protocol.ActionDropoff || s.Node == "" || !isConcreteStorageDropoff(a.db, s.Node) {
			continue
		}
		out = append(out, slotNeed{stepIndex: i, nodeName: s.Node, group: s.Group})
	}
	return out
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
func (a *Allocator) reserveComplexPlan(order *orders.Order, plan *ComplexPlan) (assigned []reservedPickup, outcome reserveOutcome, err error) {
	pickups := complexPickups(plan.ResolvedSteps)

	rows, err := a.db.ListReservationsByOrder(order.ID)
	if err != nil {
		return nil, reserveHolding, fmt.Errorf("list reservations for order %d: %w", order.ID, err)
	}
	held := a.resolveHeldReservations(rows)

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
		bin, nodeHadBins, ferr := a.findAvailableForNeed(pk.step, order.PayloadCode)
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
		aerr := a.binManifest.ReserveForDispatch(bin.ID, order.ID)
		if errors.Is(aerr, reservations.ErrReservationConflict) {
			missing++
			anyMissWithBins = true // the bin exists, another order holds it — retry next tick
			continue
		}
		if aerr != nil {
			return nil, reserveHolding, fmt.Errorf("reserve order=%d bin=%d: %w", order.ID, bin.ID, aerr)
		}
		a.db.AppendAudit("bin", bin.ID, "reserved", "",
			fmt.Sprintf("complex order %d reserve at %s", order.ID, pk.step.Node), "system")
		assigned = append(assigned, reservedPickup{
			stepIndex: pk.stepIndex, nodeName: pk.step.Node, binID: bin.ID, confirmed: false,
		})
	}

	// Release held bins not matched to any current need. Slot rows are reconciled
	// (and released) by reserveComplexSlots — skip them here.
	for _, hb := range held {
		if hb.used || hb.kind == reservations.KindSlot {
			continue
		}
		if hb.confirmed {
			// Claim + reservation together (a re-resolution abandoned a confirmed
			// source before dispatch — rare, but keep them coupled).
			if rerr := a.db.ReleaseClaimForBin(hb.binID, order.ID); rerr != nil {
				log.Printf("dispatch: reservation-release failed (reconcile stray bin-claim) order=%d bin=%d: %v", order.ID, hb.binID, rerr)
			}
		} else if rerr := a.db.ReleaseReservation(order.ID, hb.binID); rerr != nil {
			log.Printf("dispatch: reservation-release failed (reconcile stray bin) order=%d bin=%d: %v", order.ID, hb.binID, rerr)
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

// reserveComplexSlots is the destination-slot reconcile — the reservation dual of
// the retired hard-claim slot loop, and the slot leg of the 1c owner-aware reconcile
// (D4 split-brain fix: an incomplete order now holds its slots as revocable
// RESERVATIONS across ticks, not hard nodes.claimed_by). It runs BEFORE the bin
// reserve so a relay/staging slot is held before the bin leg reads its emptiness
// (D40): a slot another order can't reserve can't take a stray resident, which is
// what makes "empty at reserve" a stable relay signal.
//
// Per concrete storage-dropoff need it keeps a held slot reservation (owner-aware,
// matched by node) or acquires a fresh one. On conflict (another order holds it): a
// FUNGIBLE NGRP dropoff reverts to its group so the next tick re-resolves to a free
// child (the escape valve, preserved from the hard loop); a FIXED-concrete dropoff
// holds and retries (Wait). Held slots matching no current need are released (coupled
// if confirmed, plain if pending). reserveComplete iff every slot need is reserved.
func (a *Allocator) reserveComplexSlots(order *orders.Order, resolvedSteps []resolvedStep) (reserveOutcome, error) {
	needs := a.slotNeeds(resolvedSteps)

	rows, err := a.db.ListReservationsByOrder(order.ID)
	if err != nil {
		return reserveHolding, fmt.Errorf("list reservations (slots) for order %d: %w", order.ID, err)
	}
	held := a.resolveHeldReservations(rows)

	missing := 0
	reverted := false
	for _, sn := range needs {
		// 1. Reuse a held slot reservation on this node (owner-aware — re-Acquiring
		//    would self-conflict on the per-node slot index).
		if hs := matchHeldSlot(held, sn.nodeName); hs != nil {
			hs.used = true
			continue
		}
		// 2. Acquire a fresh slot reservation.
		node, gerr := a.db.GetNodeByDotName(sn.nodeName)
		if gerr != nil || node == nil {
			missing++ // node unresolvable → hold and retry
			continue
		}
		aerr := a.db.ReserveSlot(node.ID, order.ID)
		if errors.Is(aerr, reservations.ErrReservationConflict) {
			missing++
			if sn.group != "" {
				// Fungible NGRP dropoff — revert to the group so reResolveComplexSteps
				// re-picks a free slot next tick (the escape valve). A fixed-concrete
				// dropoff (group=="") just holds and retries.
				resolvedSteps[sn.stepIndex].Node = sn.group
				reverted = true
			}
			continue
		}
		if aerr != nil {
			return reserveHolding, fmt.Errorf("reserve slot order=%d node=%s: %w", order.ID, sn.nodeName, aerr)
		}
		a.db.AppendAudit("node", node.ID, "slot_reserved", "",
			fmt.Sprintf("complex order %d slot reserve at %s", order.ID, sn.nodeName), "system")
	}

	// Release held slots no longer matching any need (a re-resolution moved a
	// dropoff) — coupled if confirmed, plain if pending; the slot dual of the bin
	// release in reserveComplexPlan.
	for _, hb := range held {
		if hb.used || hb.kind != reservations.KindSlot {
			continue
		}
		if hb.confirmed {
			if rerr := a.db.ReleaseSlotClaim(hb.nodeID, order.ID); rerr != nil {
				log.Printf("dispatch: reservation-release failed (reconcile stray slot-claim) order=%d node=%d: %v", order.ID, hb.nodeID, rerr)
			}
		} else if rerr := a.db.ReleaseSlotReservation(hb.nodeID, order.ID); rerr != nil {
			log.Printf("dispatch: reservation-release failed (reconcile stray slot) order=%d node=%d: %v", order.ID, hb.nodeID, rerr)
		}
	}

	// Persist any fungible reverts so the next tick re-resolves them to free slots.
	if reverted {
		if j, mErr := json.Marshal(resolvedSteps); mErr == nil {
			if uErr := a.db.UpdateOrderStepsJSON(order.ID, string(j)); uErr != nil {
				log.Printf("dispatch: update steps_json after slot revert for order %d: %v", order.ID, uErr)
			} else {
				order.StepsJSON = string(j)
			}
		}
	}

	if missing == 0 {
		return reserveComplete, nil
	}
	return reserveHolding, nil
}

// resolveHeldReservations loads each held reservation's bin and current node so
// the reconcile can match by (node, empty-status). A bin sits at its source node
// until the order dispatches; an unresolvable one is left with an empty node so it
// matches nothing and gets released as a stray.
func (a *Allocator) resolveHeldReservations(rows []reservations.Reservation) []*heldReservation {
	out := make([]*heldReservation, 0, len(rows))
	for _, r := range rows {
		hb := &heldReservation{kind: r.Kind, confirmed: r.State == reservations.StateConfirmed}
		// A lookup error degrades the hold to a stray (empty nodeName → matches no
		// need → released) rather than crashing the reconcile — but log it, so a
		// transient DB blip that silently drops a held resource is diagnosable and
		// not mistaken for a legitimate re-resolution.
		switch r.Kind {
		case reservations.KindSlot:
			// A slot row matches a need by node directly — no bin lookup.
			hb.nodeID = r.NodeID
			node, nerr := a.db.GetNode(r.NodeID)
			if nerr != nil {
				log.Printf("dispatch: resolveHeld slot node=%d lookup failed: %v (degrading to stray)", r.NodeID, nerr)
			} else if node != nil {
				hb.nodeName = node.Name
			}
		default: // bin
			hb.binID = r.BinID
			b, err := a.db.GetBin(r.BinID)
			if err != nil {
				log.Printf("dispatch: resolveHeld bin=%d lookup failed: %v (degrading to stray)", r.BinID, err)
			} else if b != nil {
				hb.empty = b.PayloadCode == ""
				if b.NodeID != nil {
					node, nerr := a.db.GetNode(*b.NodeID)
					if nerr != nil {
						log.Printf("dispatch: resolveHeld bin=%d node=%d lookup failed: %v", r.BinID, *b.NodeID, nerr)
					} else if node != nil {
						hb.nodeName = node.Name
					}
				}
			}
		}
		out = append(out, hb)
	}
	// Watch item (D45 §5): the bin (node, empty-status) match key rests on the
	// one-bin-per-node occupancy invariant. If two held BIN holds resolve to the same
	// (node, empty), the key is ambiguous — log it (debug) so a violation is visible.
	seen := make(map[string]bool, len(out))
	for _, hb := range out {
		if hb.kind == reservations.KindSlot || hb.nodeName == "" {
			continue
		}
		k := fmt.Sprintf("%s|%t", hb.nodeName, hb.empty)
		if seen[k] {
			a.dbg("reserve: WARNING two held bin reservations resolve to the same (node=%s, empty=%t) — occupancy invariant violated", hb.nodeName, hb.empty)
		}
		seen[k] = true
	}
	return out
}

// matchHeldToNeed returns the first unused held bin that satisfies the need —
// same node, same empty-status. Owner-aware (the held bin is the order's own).
// Skips slot rows (the slot reconcile matches those).
func matchHeldToNeed(held []*heldReservation, step resolvedStep) *heldReservation {
	for _, hb := range held {
		if hb.used || hb.kind == reservations.KindSlot || hb.nodeName == "" {
			continue
		}
		if hb.nodeName == step.Node && hb.empty == step.Empty {
			return hb
		}
	}
	return nil
}

// matchHeldSlot returns the first unused held SLOT reservation on nodeName —
// owner-aware (the held slot is the order's own; re-Acquiring it would self-conflict
// on the per-node slot index, the same landmine documented on reservations.Acquire).
func matchHeldSlot(held []*heldReservation, nodeName string) *heldReservation {
	for _, hb := range held {
		if hb.used || hb.kind != reservations.KindSlot || hb.nodeName == "" {
			continue
		}
		if hb.nodeName == nodeName {
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
//
// WATCH ITEM (D45 §5): this reads LIVE node occupancy (ListBinsByNode), so its
// nodeHadBins result — and therefore the moot/hold decision it feeds — is only
// non-racy because the whole reserve/confirm runs under the fulfillment scanner's
// scanMu (Scanner.RunOnce serializes scan(), fulfillment/scanner.go). Do NOT call
// the reserve reconcile from a non-serialized path, or two ticks could read
// occupancy across each other and mis-classify a moot vs a present-but-taken node.
func (a *Allocator) findAvailableForNeed(step resolvedStep, payloadCode string) (bin *binsstore.Bin, nodeHadBins bool, err error) {
	node, err := a.db.GetNodeByDotName(step.Node)
	if err != nil || node == nil {
		return nil, false, nil // node unresolvable → treat as empty (miss), retry next tick
	}
	bins, err := a.db.ListBinsByNode(node.ID)
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
func (a *Allocator) confirmComplexPlan(order *orders.Order, plan *ComplexPlan, assigned []reservedPickup) error {
	steps := plan.ResolvedSteps
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}

	// Confirm the reserved destination slots FIRST — before the bins. Each was
	// reserved (pending) by reserveComplexSlots; ConfirmSlotClaim hard-claims it
	// under the seatbelt (owner-idempotent + NOT EXISTS bins + EXISTS pending slot
	// reservation) and confirms the reservation in ONE tx. Slots-before-bins keeps a
	// slot↔bin cross-type claim cycle from forming. A slot already claimed by THIS
	// order (a prior tick) is confirmed-in-place, never re-claimed — the D46 honest
	// skip at the slot level. A conflict (slot taken between reserve and confirm)
	// returns codeClaimFailed so the caller requeues, exactly like a bin.
	for _, sn := range a.slotNeeds(steps) {
		node, gerr := a.db.GetNodeByDotName(sn.nodeName)
		if gerr != nil || node == nil {
			return &planningError{Code: codeClaimFailed, Detail: fmt.Sprintf("confirm slot %s for order %d: node unresolved", sn.nodeName, order.ID)}
		}
		if node.ClaimedBy != nil && *node.ClaimedBy == order.ID {
			// Already ours — confirm any still-pending reservation in place, no re-claim.
			if err := a.db.ConfirmSlotReservation(node.ID, order.ID); err != nil {
				return &planningError{Code: codeClaimFailed, Detail: fmt.Sprintf("confirm held slot %s for order %d: %v", sn.nodeName, order.ID, err)}
			}
			continue
		}
		if err := a.db.ConfirmSlotClaim(node.ID, order.ID); err != nil {
			return &planningError{Code: codeClaimFailed, Detail: fmt.Sprintf("confirm slot claim %s for order %d: %v", sn.nodeName, order.ID, err)}
		}
		a.db.AppendAudit("node", node.ID, "slot_claimed", "",
			fmt.Sprintf("complex order %d slot confirm at %s", order.ID, sn.nodeName), "system")
	}

	var claimed []claimedBin
	for _, rp := range assigned {
		// Is the bin ALREADY hard-claimed by THIS order? A prior tick may have
		// claimed it and crashed/errored before confirming its reservation (the D45
		// wedge half-state: claimed_by=order, reservation still pending). Check by
		// DATA, not by rp.confirmed — a claimed-but-pending bin is still ours.
		claimedByUs := false
		if b, gerr := a.db.GetBin(rp.binID); gerr == nil && b != nil &&
			b.ClaimedBy != nil && *b.ClaimedBy == order.ID {
			claimedByUs = true
		}
		if claimedByUs {
			// Already ours — never re-claim (re-claiming would risk a false 0-rows
			// failure). If the reservation is still pending, confirm it in place;
			// if already confirmed this is a no-op. The bin is not re-touched.
			if !rp.confirmed {
				if err := a.binManifest.ConfirmHeldReservation(order.ID, rp.binID); err != nil {
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
			if err := a.binManifest.ConfirmClaim(rp.binID, order.ID, nil); err != nil {
				return &planningError{
					Code:   codeClaimFailed,
					Detail: fmt.Sprintf("confirm claim bin %d for order %d: %v", rp.binID, order.ID, err),
				}
			}
			a.db.AppendAudit("bin", rp.binID, "claimed", "",
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
	if err := a.db.UpdateOrderBinID(order.ID, claimed[primaryIdx].binID); err != nil {
		a.dbg("complex confirm: WARNING order %d UpdateOrderBinID(bin=%d) failed: %v",
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
			if err := a.db.InsertOrderBin(order.ID, cb.binID, cb.stepIndex, protocol.ActionPickup, cb.nodeName, destNode); err != nil {
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
