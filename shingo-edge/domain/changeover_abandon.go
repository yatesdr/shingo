package domain

import (
	"errors"
	"fmt"

	"shingo/protocol"
)

// ErrPartnerInFlight rejects a plain abandon while the partner evacuation is
// with the fleet. Cancelling the supply would cascade a cancel onto an evac a
// robot is moving, or one that faulted mid-move with its robot's state
// unknown — the divergence family the faulted-release fix closed. The
// operator's ways out are to wait, or to accept the half-swap (evac completes,
// supply alone is cancelled).
var ErrPartnerInFlight = errors.New("partner evacuation is already executing on the fleet; wait for it to finish or accept the half-swap")

// AbandonRefusal is why a changeover node's waiting supply cannot be abandoned
// right now, or nil when it can. ONE RULE, TWO READERS: the abandon itself
// (Engine.AbandonChangeoverSupply) refuses on it, and the station view decides
// from it whether to offer the operator's ABANDON - SKIP THIS STEP button, so
// the button is never shown for a step the server would refuse.
//
// supply and evac are the task's live orders, nil when absent or already
// gone. A terminal supply has nothing left to cancel; abandoning it only
// settles the task, which is the recovery such a dead end needs.
//
//   - The task must be parked waiting for material.
//   - Material already in motion arrives; it is not abandoned. The
//     awaiting_material stamp does not revert when Core un-parks the order, so
//     the supply's live status is what decides.
//   - A plain abandon cascades its cancel onto the partner evac, so it is
//     refused while the evac is with the fleet, faulted included
//     (IsVendorTracked). acceptHalf keeps the evac, so it does not ask.
func AbandonRefusal(task NodeTask, supply, evac *Order, acceptHalf bool) error {
	if task.State != NodeTaskAwaitingMaterial {
		return fmt.Errorf("node %s is not awaiting material (state %s)", task.NodeName, task.State)
	}
	if task.NextMaterialOrderID == nil {
		return fmt.Errorf("node %s has no supply order to abandon", task.NodeName)
	}
	if supply != nil && !protocol.IsTerminal(supply.Status) && !protocol.IsWaiting(supply.Status) {
		return fmt.Errorf("supply order %d is now in motion (%s) — material is on its way", supply.ID, supply.Status)
	}
	if !acceptHalf && evac != nil && protocol.IsVendorTracked(evac.Status) {
		return ErrPartnerInFlight
	}
	return nil
}
