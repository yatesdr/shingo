package store

// Phase 5b delegate file: process_node_runtime CRUD now lives in
// store/processes/. This file preserves the *store.DB method surface
// so external callers do not need to change.

import "shingoedge/store/processes"

// EnsureProcessNodeRuntime returns the runtime row for a process_node,
// inserting a fresh row when none exists yet.
func (db *DB) EnsureProcessNodeRuntime(processNodeID int64) (*processes.RuntimeState, error) {
	return processes.EnsureRuntime(db.DB, processNodeID)
}

// GetProcessNodeRuntime returns the runtime row for a process_node.
func (db *DB) GetProcessNodeRuntime(processNodeID int64) (*processes.RuntimeState, error) {
	return processes.GetRuntime(db.DB, processNodeID)
}

// ProcessNodeRuntimes returns the runtime rows for a set of process_nodes in
// one query, keyed by process_node_id. A node with no row yet is absent from
// the map — which is what a caller looping over GetProcessNodeRuntime saw as
// sql.ErrNoRows, and it does NOT insert one. Used by the per-node walkers.
func (db *DB) ProcessNodeRuntimes(processNodeIDs []int64) (map[int64]*processes.RuntimeState, error) {
	return processes.RuntimesForNodes(db.DB, processNodeIDs)
}

// SetProcessNodeRuntime updates the active claim and remaining UOP on
// a runtime row. Does not touch active_bin_id — callers that need
// atomic bin-pointer turnover should use SetProcessNodeRuntimeWithBin.
func (db *DB) SetProcessNodeRuntime(processNodeID int64, activeClaimID *int64, remainingUOP int) error {
	return processes.SetRuntime(db.DB, processNodeID, activeClaimID, remainingUOP)
}

// SetProcessNodeActiveClaim points the runtime row at a claim, leaving the
// count, the pointers and the hold alone. See processes.SetRuntimeClaim.
func (db *DB) SetProcessNodeActiveClaim(processNodeID int64, activeClaimID *int64) error {
	return processes.SetRuntimeClaim(db.DB, processNodeID, activeClaimID)
}

// SetProcessNodeRuntimeClaimCountAndEpoch writes the claim and the count
// and advances the stamp for the carrier the write names, leaving the bin
// pointer alone. Used by the clear routes, where Core starts the carrier's
// next life and returns the new stamp but the carrier stays put.
func (db *DB) SetProcessNodeRuntimeClaimCountAndEpoch(processNodeID int64, activeClaimID *int64, remainingUOP int, binID, deltaEpoch int64) error {
	return processes.SetRuntimeClaimCountAndEpoch(db.DB, processNodeID, activeClaimID, remainingUOP, binID, deltaEpoch)
}

// SetProcessNodeRuntimeWithBin updates active_claim_id, active_bin_id,
// and remaining_uop_cached atomically. No production caller (see
// processes.SetRuntimeWithBin); test fixtures seed runtime rows with it.
func (db *DB) SetProcessNodeRuntimeWithBin(processNodeID int64, activeClaimID, activeBinID *int64, remainingUOP int) error {
	return processes.SetRuntimeWithBin(db.DB, processNodeID, activeClaimID, activeBinID, remainingUOP)
}

// SetProcessNodeActiveBinID writes only the active bin pointer on a
// runtime row. Used by the bin-pickup handler to clear ownership
// without disturbing the claim or count.
func (db *DB) SetProcessNodeActiveBinID(processNodeID int64, activeBinID *int64) error {
	return processes.SetActiveBinID(db.DB, processNodeID, activeBinID)
}

// ClearProcessNodeActiveBinAndCount clears the bin pointer and zeroes the
// cached count atomically — the pointer/count half of the carrier-left verb.
// See processes.ClearActiveBinAndCount.
func (db *DB) ClearProcessNodeActiveBinAndCount(processNodeID int64) error {
	return processes.ClearActiveBinAndCount(db.DB, processNodeID)
}

// LeaveProcessNodeAtPickup is the pickup's carrier-left statement (Lane H,
// D11): see processes.LeaveAtPickup.
func (db *DB) LeaveProcessNodeAtPickup(processNodeID, pickupEpoch int64, hold int, holdPart string) error {
	return processes.LeaveAtPickup(db.DB, processNodeID, pickupEpoch, hold, holdPart)
}

// SetProcessNodeActiveBinIDAndEpoch writes the active bin pointer and
// epoch together. Used by BindActiveBin (loader L1 confirm) where
// Core's LoadBin response provides the epoch.
func (db *DB) SetProcessNodeActiveBinIDAndEpoch(processNodeID int64, activeBinID *int64, deltaEpoch int64) error {
	return processes.SetActiveBinIDAndEpoch(db.DB, processNodeID, activeBinID, deltaEpoch)
}

// SetProcessNodeRuntimeWithBinAndEpoch updates active_claim_id,
// active_bin_id, active_bin_epoch, and remaining_uop_cached atomically.
// Used by ManualLoad (operator imprint, epoch from Core's LoadBin response)
// and BindFromCore (Core's announcement of the carrier at the slot).
func (db *DB) SetProcessNodeRuntimeWithBinAndEpoch(processNodeID int64, activeClaimID, activeBinID *int64, deltaEpoch int64, remainingUOP int) error {
	return processes.SetRuntimeWithBinAndEpoch(db.DB, processNodeID, activeClaimID, activeBinID, deltaEpoch, remainingUOP)
}

// SetProcessNodeCountFenced writes a fenced count for the carrier bound at
// the slot. Reports whether it landed. See processes.SetCountFenced.
func (db *DB) SetProcessNodeCountFenced(processNodeID, binID, deltaEpoch int64, remainingUOP int, asOfSeq int64) (bool, error) {
	return processes.SetCountFenced(db.DB, processNodeID, binID, deltaEpoch, remainingUOP, asOfSeq)
}

// BindEmptySlotUnlessDeparted binds binID at deltaEpoch into a slot read as
// empty, unless binID is the bin that last left the slot and deltaEpoch is
// older than the stamp it left with. Reports whether it bound. See
// processes.BindEmptySlotUnlessDeparted.
func (db *DB) BindEmptySlotUnlessDeparted(processNodeID int64, activeClaimID *int64, binID, deltaEpoch int64, remainingUOP int) (bool, error) {
	return processes.BindEmptySlotUnlessDeparted(db.DB, processNodeID, activeClaimID, binID, deltaEpoch, remainingUOP)
}

// BindLandingAt binds the carrier a landing names at the slot, its refusals
// in its WHERE (Lane H, H.4). Reports whether it landed. See
// processes.BindLanding.
func (db *DB) BindLandingAt(processNodeID int64, l processes.LandingBind) (bool, error) {
	return processes.BindLanding(db.DB, processNodeID, l)
}

// SetProcessNodeRuntimeLinesidePayload records the identity of the carrier
// standing at this node, together with whether that identity could be
// established at all, who asserted it, and when.
//
// known is not derivable from payloadCode. A carrier known to be carrying
// nothing and a carrier nobody could read both spell themselves "", and the
// readers of this row fail open on the first and must not act on the second.
// Callers go through Engine.recordLinesideCarrier, which takes a
// domain.LinesideCarrier and cannot lose the distinction on the way here.
func (db *DB) SetProcessNodeRuntimeLinesidePayload(processNodeID int64, payloadCode string, known bool, source string) error {
	return processes.SetRuntimeLinesidePayload(db.DB, processNodeID, payloadCode, known, source)
}

// UpdateProcessNodeRuntimeOrders writes BOTH order pointers on a runtime row.
// Only for callers that genuinely decide both slots at once — see
// processes.UpdateRuntimeOrders. To set one pointer, use the partial setters
// below; passing nil here for the slot you are not changing destroys it.
func (db *DB) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return processes.UpdateRuntimeOrders(db.DB, processNodeID, activeOrderID, stagedOrderID)
}

// SetProcessNodeRuntimeActiveOrder writes the active order pointer, leaving
// staged alone.
func (db *DB) SetProcessNodeRuntimeActiveOrder(processNodeID int64, activeOrderID *int64) error {
	return processes.SetRuntimeActiveOrder(db.DB, processNodeID, activeOrderID)
}

// SetProcessNodeRuntimeStagedOrder writes the staged order pointer, leaving
// active alone.
func (db *DB) SetProcessNodeRuntimeStagedOrder(processNodeID int64, stagedOrderID *int64) error {
	return processes.SetRuntimeStagedOrder(db.DB, processNodeID, stagedOrderID)
}

// ClearProcessNodeRuntimeOrders drops both order pointers on one node.
func (db *DB) ClearProcessNodeRuntimeOrders(processNodeID int64) error {
	return processes.ClearRuntimeOrders(db.DB, processNodeID)
}

// ClearProcessNodeRuntimeOrderRefs nulls every runtime order pointer that
// references orderID, on whichever node rows hold it.
func (db *DB) ClearProcessNodeRuntimeOrderRefs(orderID int64) error {
	return processes.ClearRuntimeOrderRefs(db.DB, orderID)
}

// UpdateProcessNodeUOP writes the remaining UOP on a runtime row.
func (db *DB) UpdateProcessNodeUOP(processNodeID int64, remainingUOP int) error {
	return processes.UpdateRuntimeUOP(db.DB, processNodeID, remainingUOP)
}

// SetProcessNodeReleaseCount writes the outgoing carrier's count at its
// release and marks it released-empty when the count is 0. See
// processes.SetRuntimeReleaseCount.
func (db *DB) SetProcessNodeReleaseCount(processNodeID int64, remainingUOP int) error {
	return processes.SetRuntimeReleaseCount(db.DB, processNodeID, remainingUOP)
}

// AddPendingUOPDelta seeds a hold of no known part. NO PRODUCTION CALLER:
// the tick path holds through HoldPendingUOP, which records the part; test
// fixtures seed holds with this.
func (db *DB) AddPendingUOPDelta(processNodeID int64, delta int) error {
	return processes.AddPendingUOPDelta(db.DB, processNodeID, delta, "")
}

// ChargeProcessNodeSeat adds a tick's delta to the bound carrier's count, and
// with clearPending clears the hold it replays, on the seat as the tick read
// it (processes.ChargeSeatUOP). False: the seat changed, nothing written.
func (db *DB) ChargeProcessNodeSeat(processNodeID int64, delta int, clearPending bool, seat processes.SeatAsRead) (bool, error) {
	return processes.ChargeSeatUOP(db.DB, processNodeID, delta, clearPending, seat)
}

// HoldProcessNodeSeat adds a tick's count to the seat's hold for the next
// carrier of part, on the seat as the tick read it (processes.HoldSeatUOP).
// False: the seat changed, nothing written.
func (db *DB) HoldProcessNodeSeat(processNodeID int64, delta int, part string, seat processes.SeatAsRead) (bool, error) {
	return processes.HoldSeatUOP(db.DB, processNodeID, delta, part, seat)
}

// SetActivePull marks a node as the active pull point for A/B cycling.
// Only the active-pull node gets counter delta decrements.
func (db *DB) SetActivePull(processNodeID int64, active bool) error {
	return processes.SetActivePull(db.DB, processNodeID, active)
}
