// capacity.go — shared dropoff-capacity gate for the queue-on-capacity
// work (Phase 4 of bin-transit-state).
//
// Pre-Phase-4 the only dropoff-capacity gate in the codebase lived
// inside fulfillment.Scanner.tryFulfill — i.e., it gated REPLAY of
// already-queued orders, not initial dispatch. Simple retrieves to a
// full destination dispatched and raced the scanner; complex orders
// failed at planning rather than queueing. Both behaviors are bugs we
// haven't seen in production at scale because:
//
//   - Production swap orders (the only ones with realistic capacity
//     pressure) are complex orders that fail-rather-than-queue today.
//   - Simple retrieves go to/from loader/unloader nodes where
//     destination-full is operator-paced or NGRP-resolved (many
//     slots).
//
// But edge cases exist (loader rapid cycles, NGRP saturation, manual
// moves, auto-return, side-cycle L2/U2). Phase 4 adds this shared gate
// at every planning-time dispatch path so they all queue cleanly
// instead of racing.
//
// NGRP dropoffs are intentionally not gated by this helper today —
// the binresolver picks a free child at dispatch time and returns an
// error if all are full. For the Phase 4 work we leave that path as-
// is (it doesn't queue today, but it doesn't race either; the resolver
// rejects). Concrete-node dropoffs are the targets here.

package dispatch

import (
	"database/sql"
	"errors"

	"shingo/protocol"
	"shingocore/store/nodes"
)

// CapacityDB is the read interface used by the capacity gate. Kept
// narrow so the gate can be exercised against a fake store in tests
// without spinning up the full dispatcher harness. The concrete *store.DB
// satisfies it.
type CapacityDB interface {
	GetNodeByDotName(name string) (*nodes.Node, error)
	CountBinsByNode(nodeID int64) (int, error)
	CountInFlightOrdersByDeliveryNodeExcluding(name string, excludeID int64) (int, error)
	// ListChildNodes returns the children of an NGRP for saturation
	// checking — when every child is full, the NGRP destination as a
	// whole is "blocked" and the order should queue rather than fail at
	// dispatch.
	ListChildNodes(parentID int64) ([]*nodes.Node, error)

	// ── The maintained-group level (MG4-3) ──────────────────────────────────
	//
	// The gate and the resolver must not disagree about whether a group can take
	// a carrier, so the gate reads the level through the SAME two calls the
	// resolver does. Without this, a group at its declared level looks free here
	// — it has empty positions, they are just spoken for — and the order is
	// admitted, resolved, refused, and parked one layer deeper with a cause that
	// says nothing about the level.
	ListMaintainLevels(groupNodeID int64) ([]nodes.MaintainLevel, error)
	CountEmptyBinsOfTypeInGroup(binTypeCode string, groupNodeID int64) (int, error)
}

// CapacityBlock is the structured result of a blocked dropoff-capacity check —
// the queue code (always QueueWaitingForSlot when blocked), the engineer-only
// cause naming which shape of block it was, and the params the operator sentence
// is generated from. Replaces the pre-formatted reason string so a caller parks
// the order through the shared formatter door, never with free text.
type CapacityBlock struct {
	Cause  QueueCause
	Params QueueParams
}

// CheckDropoffCapacity returns (false, zero) when the named delivery node can
// accept a bin right now, and (true, block) when it cannot — where block names
// WHICH shape of refusal it was (block.Cause) and carries the values the
// operator sentence is generated from (block.Params).
//
// IT RETURNS NO SENTENCE, and this comment used to say it did: "(true, reason)
// … the reason string is suitable for storing on orders.queue_reason and
// rendering to operators". That stopped being true when the pre-formatted string
// became a CapacityBlock so callers would park through the shared formatter door
// — which the CapacityBlock doc twelve lines above already says. Two comments in
// one file, disagreeing about this function's own return type.
//
// CALLERS MUST WRITE block.Cause THROUGH, not a coarse tag of their own. The
// four causes below have four different releasers, and the two complex arms
// substituted CauseDropoffCapacity for all of them until 2026-08-30 — passing
// block.Params so the SENTENCE was right while the column an engineer groups by
// was wrong. See CauseDropoffCapacity's own note.
//
// excludeOrderID is the caller's own order — its in-flight status is
// excluded from the count to prevent self-collision when a gate checks
// capacity from inside the order's own dispatch/retry path. Every real
// caller now passes its order.ID: the intake planners always did, and the
// fulfillment scanner must too since its retry set was widened to
// include `sourcing` orders (which the in-flight tally counts) — passing 0
// there would let a self-retrying order block itself forever. Pass 0 only
// from preview paths that have no order row yet.
//
// "Capacity" here is the same predicate the fulfillment scanner has
// used for queued retrieves: zero physical bins at the node AND zero
// in-flight orders headed there. Either condition makes the slot
// unsafe for a fresh dispatch.
//
// Empty deliveryNode → not blocked (the order has no concrete dropoff
// to gate on; auto-confirm or fleet-resolved destination orders fall
// into this bucket).
//
// Synthetic-node deliveryNode:
//   - NGRP: walk children, treat as blocked iff EVERY child is
//     occupied or has an in-flight order inbound. The resolver picks
//     a free child at dispatch time when one exists; this gate is what
//     makes "all children full" produce a queue rather than a fail.
//   - LANE / _TRANSIT / other synthetic types: pass through. LANE
//     gating is handled inside the lane-aware planners (depth/buried
//     reshuffle); _TRANSIT is never a real dropoff.
//
// Node not found → not blocked, on purpose. A typoed node name should fail at
// the actual dispatch with a clearer error than "waiting for a slot at a place
// that does not exist", which would queue forever on a reason that can never
// clear.
//
// Node lookup ERRORED → blocked, capacity-check-failed. That is a different
// fact and it used to be folded in with the one above, so a database blip on
// this read passed the gate while the identical blip on either of the two reads
// below fails it closed. The three reads now agree: if occupancy cannot be read,
// do not risk the drop.
func CheckDropoffCapacity(db CapacityDB, deliveryNode string, excludeOrderID int64) (blocked bool, block CapacityBlock) {
	return CheckDropoffCapacityForType(db, deliveryNode, excludeOrderID, nil)
}

// CheckDropoffCapacityForType is CheckDropoffCapacity for a caller that knows
// which carrier type is arriving.
//
// A SECOND ENTRY POINT RATHER THAN A WIDER SIGNATURE, because the type is known
// at exactly one of the eight call sites. Threading a nil through the other
// seven would be seven edits that each say "I do not know", and the reader of
// any one of them would have to go and check that nil means what they hope.
//
// The type only changes the LEVEL question — physical occupancy is physical
// whatever is arriving.
func CheckDropoffCapacityForType(db CapacityDB, deliveryNode string, excludeOrderID int64,
	binTypeID *int64) (blocked bool, block CapacityBlock) {
	if deliveryNode == "" {
		return false, CapacityBlock{}
	}
	params := QueueParams{Destination: deliveryNode}
	node, err := db.GetNodeByDotName(deliveryNode)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && node == nil:
		// No such node — let dispatch produce the real error.
		return false, CapacityBlock{}
	case err != nil:
		return true, CapacityBlock{Cause: CauseCapacityCheckFailed, Params: params}
	}
	if node.IsSynthetic {
		if node.NodeTypeCode == protocol.NodeClassNGRP {
			return checkNGRPCapacity(db, node, deliveryNode, excludeOrderID, binTypeID)
		}
		// LANE / _TRANSIT / future synthetic types — defer to whoever
		// resolves them at dispatch time. _TRANSIT is never a legit
		// dropoff; LANE depth/buried handling lives inside the
		// lane-aware planners.
		return false, CapacityBlock{}
	}
	count, err := db.CountBinsByNode(node.ID)
	if err != nil {
		// Fail closed: if occupancy can't be read, don't risk dropping onto a
		// possibly-full node — gate the order so it queues until the check works.
		return true, CapacityBlock{Cause: CauseCapacityCheckFailed, Params: params}
	}
	if count > 0 {
		// Carry the count into the sentence. "A bin is sitting there" and "an
		// order is already on its way" are different operator situations —
		// go clear it, versus wait — and both used to render as the bare
		// "Waiting for a slot at X". The discriminator existed only in
		// queue_cause, which no surface renders and which never leaves Core.
		p := params
		p.BlockingBins = count
		return true, CapacityBlock{Cause: CauseDropoffOccupied, Params: p}
	}
	inFlight, err := db.CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode, excludeOrderID)
	if err != nil {
		// Fail closed on the in-flight read as well.
		return true, CapacityBlock{Cause: CauseCapacityCheckFailed, Params: params}
	}
	if inFlight > 0 {
		p := params
		p.InboundOrders = inFlight
		return true, CapacityBlock{Cause: CauseDropoffInflight, Params: p}
	}
	return false, CapacityBlock{}
}

// checkNGRPCapacity walks the children of an NGRP destination and
// returns blocked=true only when every enabled, non-synthetic child
// is either occupied by a bin or has an in-flight order inbound. At
// least one free child means the resolver will be able to pick a
// concrete dropoff at dispatch time.
//
// Concurrency: there's a TOCTOU window between this check and the
// resolver's child pick at dispatch time — a different order could
// claim the free child between the two. The existing claim_failed →
// queueOrder path handles that race (the loser of the claim race
// re-queues), so this gate doesn't need to be perfectly atomic; it
// just needs to handle the steady-state "everything full" case.
//
// excludeOrderID propagates to the per-child in-flight count so an
// order checking its own NGRP destination doesn't self-collide.
//
// A CHILD THAT CANNOT BE READ IS NOT A FREE CHILD. All three reads in here used
// to fail open, and the two per-child ones failed open in the worst direction:
// `err == nil && count > 0` means a read error skips the `continue`, and the
// child falls through to be counted FREE — the answer that says "there is room",
// on no evidence. The outer gate's two reads have failed closed since they were
// written; these are the same reads one level down.
//
// "Cannot see" and "full" are kept apart. A group whose children could not be
// read reports capacity-check-failed, not ngrp-full: both queue the order, but
// only one of them sends an operator to go clear a group that may be empty.
func checkNGRPCapacity(db CapacityDB, ngrp *nodes.Node, ngrpName string, excludeOrderID int64,
	binTypeID *int64) (blocked bool, block CapacityBlock) {

	params := QueueParams{Destination: ngrpName}

	// ── THE LEVEL, BEFORE THE PHYSICS ───────────────────────────────────────
	//
	// A maintained group at its declared level is FULL in the sense that matters,
	// and it does not look full physically: it has empty positions, and they are
	// spoken for by a number somebody configured. Asking here means the order
	// parks with a cause that says so, instead of being admitted, resolved,
	// refused by MG4-1, and parked a layer deeper under a generic capacity
	// reason.
	//
	// SAME PREDICATE AS THE RESOLVER, read through the same two calls. Two
	// answers to "is this group full" is exactly the drift that puts an order in
	// a loop between a gate that admits it and a resolver that refuses it.
	if atLevel, err := ngrpAtDeclaredLevel(db, ngrp, binTypeID); err != nil {
		// Unknown is not permission, and it is not a claim of fullness either —
		// it is the check failing, which has its own cause.
		return true, CapacityBlock{Cause: CauseCapacityCheckFailed, Params: params}
	} else if atLevel {
		// AtLevel, so the sentence says the group is holding what it was told to
		// hold rather than "waiting for a slot" — there are free positions, and
		// an operator sent to look for room finds room and no explanation.
		atLevelParams := params
		atLevelParams.AtLevel = true
		return true, CapacityBlock{Cause: CauseNGRPAtLevel, Params: atLevelParams}
	}
	children, err := db.ListChildNodes(ngrp.ID)
	if err != nil {
		// The child list itself is unreadable, so nothing below can be judged.
		return true, CapacityBlock{Cause: CauseCapacityCheckFailed, Params: params}
	}
	if len(children) == 0 {
		// A genuinely empty group — pass through so the resolver's own failure
		// surfaces rather than being masked as a queue. Unchanged.
		return false, CapacityBlock{}
	}
	enabledCount := 0
	freeCount := 0
	unreadable := 0
	for _, child := range children {
		if !child.Enabled || child.IsSynthetic {
			continue
		}
		enabledCount++
		c, cErr := db.CountBinsByNode(child.ID)
		if cErr != nil {
			unreadable++
			continue // not counted free: we did not learn that it is
		}
		if c > 0 {
			continue
		}
		inflight, iErr := db.CountInFlightOrdersByDeliveryNodeExcluding(child.Name, excludeOrderID)
		if iErr != nil {
			unreadable++
			continue
		}
		if inflight > 0 {
			continue
		}
		freeCount++
	}
	if enabledCount == 0 {
		// No usable children at all — the resolver will fail; pass
		// through so the failure surfaces with the resolver's reason
		// rather than masking it as a queue.
		return false, CapacityBlock{}
	}
	if freeCount > 0 {
		return false, CapacityBlock{}
	}
	if unreadable > 0 {
		// No free child was FOUND, but at least one could not be looked at, so
		// "full" is not something this run is entitled to say.
		return true, CapacityBlock{Cause: CauseCapacityCheckFailed, Params: params}
	}
	return true, CapacityBlock{Cause: CauseNGRPFull, Params: params}
}

// ngrpAtDeclaredLevel is the gate's copy of the level question, and it is a copy
// of the CALL and not of the LOGIC: both this and the resolver's atDeclaredLevel
// read ListMaintainLevels and CountEmptyBinsOfTypeInGroup, which is where the
// one definition of "how full is this group" lives.
//
// The two cannot be one function without the resolver importing dispatch or
// dispatch reaching into binresolver's unexported half. They can and do share
// the reads, the per-type / group-total asymmetry, and the reason for it — see
// binresolver.atDeclaredLevel, which carries the full account.
func ngrpAtDeclaredLevel(db CapacityDB, ngrp *nodes.Node, binTypeID *int64) (bool, error) {
	levels, err := db.ListMaintainLevels(ngrp.ID)
	if err != nil {
		return false, err
	}
	if len(levels) == 0 {
		return false, nil
	}
	if binTypeID != nil {
		for _, l := range levels {
			if l.BinTypeID != *binTypeID {
				continue
			}
			held, cerr := db.CountEmptyBinsOfTypeInGroup(l.BinTypeCode, ngrp.ID)
			if cerr != nil {
				return false, cerr
			}
			return held >= l.Want, nil
		}
		return false, nil
	}
	want, held := 0, 0
	for _, l := range levels {
		want += l.Want
		n, cerr := db.CountEmptyBinsOfTypeInGroup(l.BinTypeCode, ngrp.ID)
		if cerr != nil {
			return false, cerr
		}
		held += n
	}
	return held >= want, nil
}
