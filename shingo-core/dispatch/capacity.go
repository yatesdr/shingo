// capacity.go — dispatch's face on the dropoff-capacity gate.
//
// The gate, its history and its account of every arm live with the planner
// since B2 (dispatch/plan/capacity.go, plan.DropoffCapacity). This file says
// the planner's verdict in dispatch's queue vocabulary, so the gate's eleven
// callers ask it exactly as they did.

package dispatch

import (
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
)

// CapacityDB is the gate's read interface; see plan.CapacityDB.
type CapacityDB = plan.CapacityDB

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
// accept a bin right now, and (true, block) when it cannot: plan.DropoffCapacity's
// verdict, with its cause under dispatch's name (the same string,
// TestPlanCausesAreDeclaredQueueCauses) and its params copied field for field.
//
// CALLERS MUST WRITE block.Cause THROUGH, not a coarse tag of their own. The
// five causes have different releasers; see plan.DropoffCapacity.
//
// excludeOrderID is the caller's own order, excluded from the count so a gate
// checking from inside the order's own dispatch/retry path cannot see itself;
// 0 when the caller has no order of its own (previews).
func CheckDropoffCapacity(db CapacityDB, deliveryNode string, excludeOrderID int64) (blocked bool, block CapacityBlock) {
	blocked, v := plan.DropoffCapacity(db, deliveryNode, excludeOrderID)
	if !blocked {
		return false, CapacityBlock{}
	}
	return true, CapacityBlock{Cause: queueCauseOf(v.Cause), Params: queueParamsOf(v.Params)}
}

// CheckDropoffCapacityForType is CheckDropoffCapacity for a caller that holds
// the arriving cart: plan.DropoffCapacityForType, said the same way.
func CheckDropoffCapacityForType(db CapacityDB, deliveryNode string, excludeOrderID int64,
	stated binresolver.BinTypeStatement) (blocked bool, block CapacityBlock) {
	blocked, v := plan.DropoffCapacityForType(db, deliveryNode, excludeOrderID, stated)
	if !blocked {
		return false, CapacityBlock{}
	}
	return true, CapacityBlock{Cause: queueCauseOf(v.Cause), Params: queueParamsOf(v.Params)}
}
