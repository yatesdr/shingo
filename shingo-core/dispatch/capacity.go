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
}

// CapacityBlock is the structured result of a blocked dropoff-capacity check —
// the queue code (always QueueWaitingForSlot when blocked), the engineer-only
// cause naming which shape of block it was, and the params the operator sentence
// is generated from. Replaces the pre-formatted reason string so a caller parks
// the order through the shared formatter door, never with free text.
type CapacityBlock struct {
	Cause  string
	Params QueueParams
}

// CheckDropoffCapacity returns (false, "") when the named delivery node
// can accept a bin right now, or (true, reason) when it can't. The
// reason string is suitable for storing on orders.queue_reason and
// rendering to operators.
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
// Lookup failure → not blocked, but logged via the returned error so
// callers can surface diagnostics. We choose "not blocked" rather than
// blocking on a lookup failure to preserve forward progress: a typoed
// node name should fail at the actual dispatch with a clearer error,
// not silently queue forever.
func CheckDropoffCapacity(db CapacityDB, deliveryNode string, excludeOrderID int64) (blocked bool, block CapacityBlock) {
	if deliveryNode == "" {
		return false, CapacityBlock{}
	}
	node, err := db.GetNodeByDotName(deliveryNode)
	if err != nil || node == nil {
		// Treat lookup failure as "not blocked" — see doc above.
		return false, CapacityBlock{}
	}
	params := QueueParams{Destination: deliveryNode}
	if node.IsSynthetic {
		if node.NodeTypeCode == protocol.NodeClassNGRP {
			return checkNGRPCapacity(db, node, deliveryNode, excludeOrderID)
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
		return true, CapacityBlock{Cause: "capacity-check-failed", Params: params}
	}
	if count > 0 {
		// Carry the count into the sentence. "A bin is sitting there" and "an
		// order is already on its way" are different operator situations —
		// go clear it, versus wait — and both used to render as the bare
		// "Waiting for a slot at X". The discriminator existed only in
		// queue_cause, which no surface renders and which never leaves Core.
		p := params
		p.BlockingBins = count
		return true, CapacityBlock{Cause: "dropoff-occupied", Params: p}
	}
	inFlight, err := db.CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode, excludeOrderID)
	if err != nil {
		// Fail closed on the in-flight read as well.
		return true, CapacityBlock{Cause: "capacity-check-failed", Params: params}
	}
	if inFlight > 0 {
		p := params
		p.InboundOrders = inFlight
		return true, CapacityBlock{Cause: "dropoff-inflight", Params: p}
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
func checkNGRPCapacity(db CapacityDB, ngrp *nodes.Node, ngrpName string, excludeOrderID int64) (blocked bool, block CapacityBlock) {
	children, err := db.ListChildNodes(ngrp.ID)
	if err != nil || len(children) == 0 {
		// Empty or unreadable NGRP — treat as not blocked. The
		// resolver will return a clearer failure if it really has no
		// candidate.
		return false, CapacityBlock{}
	}
	enabledCount := 0
	freeCount := 0
	for _, child := range children {
		if !child.Enabled || child.IsSynthetic {
			continue
		}
		enabledCount++
		if c, err := db.CountBinsByNode(child.ID); err == nil && c > 0 {
			continue
		}
		if inflight, err := db.CountInFlightOrdersByDeliveryNodeExcluding(child.Name, excludeOrderID); err == nil && inflight > 0 {
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
	if freeCount == 0 {
		return true, CapacityBlock{Cause: "ngrp-full", Params: QueueParams{Destination: ngrpName}}
	}
	return false, CapacityBlock{}
}
