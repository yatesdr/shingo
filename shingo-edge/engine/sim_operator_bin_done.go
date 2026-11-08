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
// swap at a position the changeover changes (the guards' own test,
// changesNothingAt false), a node with no bin on it, or a produce node whose
// capacity is unknown. A swap at a position the changeover changes nothing at
// waits for its cart to be done, as with no changeover open: a person runs a
// common cart down (decision 50; it sent a common cart back at 13 mid-changeover
// before). A consume count Core cannot answer for waits for the next sweep,
// since the release would declare from it.
func (op *simOperator) binNotDone(nodeID, orderID int64) string {
	node, rt, claim, err := loadActiveNode(op.e.db, nodeID)
	if err != nil || node == nil || rt == nil || claim == nil || claim.IsLoaderNode() {
		return ""
	}
	if co, err := op.e.db.GetActiveProcessChangeover(node.ProcessID); err == nil && co != nil {
		if task, _, terr := op.e.db.FindChangeoverNodeTaskByOrderID(orderID); terr == nil && task != nil {
			return ""
		}
		if co.FromStyleID == nil || !op.e.changesNothingAt(newGuardReads(node), co, node) {
			return ""
		}
	}
	switch claim.Role {
	case protocol.ClaimRoleConsume:
		// A cart recorded as a part the claim does not take is never drawn (the
		// walk holds the seat's strokes for a cart of the claim's part), so it
		// never runs down: a person sends it back on the parked-ticks notice.
		if recordedOtherPart(rt, claim) {
			return ""
		}
		// The count consumeDisposition will declare from: the parts a person
		// counts in the bin, an oracle read of the simulated floor
		// (readSimFloorBins), as consumeDisposition reads it.
		bins, err := readSimFloorBins(op.e.cfg.SimCoreAPI, []string{node.CoreNodeName})
		b := bins[node.CoreNodeName]
		switch {
		case err != nil:
			return "the bin's count is unreadable"
		case !b.Occupied:
			return ""
		case b.UOPRemaining > op.linesidePullAt(b.PayloadCode, claim.UOPCapacity):
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
