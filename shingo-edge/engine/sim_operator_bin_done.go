//go:build sim

package engine

import "shingo/protocol"

// binNotDone reports why the sim operator leaves a staged same-part swap alone
// for now, or "" when it presses RELEASE.
//
// A person at the line exchanges a bin when it is done: a consume bin when it
// has run out, or when what is left is few enough to pull into the lineside
// bucket (the lineside_pull_percent knob); a produce bin when it is full. Until
// then the robot waits at the station and the operator keeps working the bin.
// Production does not gate a release on this: an operator or an auto call can
// release whenever. Only the sim's stand-in for the person follows it, and it
// looks again on its reconcile sweep, which re-schedules every staged order.
//
// A changeover's legs are not same-part swaps and go as before, and so does a
// node with no bin on it, or a produce node whose capacity is unknown. A
// consume count Core cannot answer for waits for the next sweep, since the
// release would declare from it.
func (op *simOperator) binNotDone(nodeID int64) string {
	node, rt, claim, err := loadActiveNode(op.e.db, nodeID)
	if err != nil || node == nil || rt == nil || claim == nil || claim.IsLoaderNode() {
		return ""
	}
	if _, err := op.e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		return ""
	}
	switch claim.Role {
	case protocol.ClaimRoleConsume:
		// The count consumeDisposition will declare from: Core's bin.
		bins, reachable, err := op.e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
		switch {
		case err != nil || !reachable:
			return "the bin's count is unreadable"
		case len(bins) == 0 || !bins[0].Occupied:
			return ""
		case bins[0].UOPRemaining > op.linesidePullAt(bins[0].PayloadCode, claim.UOPCapacity):
			return "parts left in the bin"
		}
	case protocol.ClaimRoleProduce:
		// No bin on the seat: the pair's evac took the full away and this leg
		// brings the empty. Nothing is filling.
		if rt.ActiveBinID == nil {
			return ""
		}
		capacity := claim.UOPCapacity
		if entry, err := op.e.db.GetPayloadCatalogByCode(claim.PayloadCode); err == nil && entry != nil && entry.UOPCapacity > 0 {
			capacity = entry.UOPCapacity
		}
		if capacity > 0 && rt.RemainingUOPCached < capacity {
			return "the bin is not full"
		}
	}
	return ""
}
