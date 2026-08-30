package dispatch

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/orders"
)

// ── ONE BURIAL HANDLER, TWO ENTRY POINTS ──────────────────────────────────
//
// A complex order's bin can be found buried at two moments: at intake, and again
// on the scanner's re-resolve. They were two near-identical functions for a long
// time, and F-04 was born in the gap — a contention arm fixed at one site and not
// the other, which is a class this file has now paid for twice
// (complex_no_shuffle_slot_docker_test.go was written about it).
//
// The two differ in exactly two things, and neither is a decision. (This read
// "After the two-shape ruling" — the collapse happened under it and survives
// §R.91 untouched, because it is about the two ENTRY POINTS rather than about
// what the demand becomes.)
//
//   - WHERE THE PAYLOAD CODE COMES FROM. Intake is handed it with the request,
//     before the row is necessarily readable for it; the replay reads it off the
//     order. A parameter.
//   - WHETHER A PARK IS ANNOUNCED. Intake announces, because the station is
//     waiting on an answer to a request it just made. The replay does not: it is
//     entered from the scanner with the demand already acquiring, so leaving it
//     queued with a cause IS the retry and there is nothing new to tell anyone.
//     A callback, nil for silence.
//
// Everything else — the read split, the cause, all seven dispositions — is one
// body now, so a fix lands at both sites or neither.

// handleComplexBurial records why a complex demand is waiting and then hands it
// its own excavation (§R.91).
//
// ITS HEADER SAID THE OPPOSITE: "It never re-plans, re-parents or moves the
// demand; see the service-dig note on proposeLaneClearDig for why." The cause is
// still written first and still stands on the row — an operator reading the
// board while the dig runs needs it — but the demand is re-parented, wears
// `reshuffling`, and comes back through `queued`.
//
// announce may be nil.
func (d *Dispatcher) handleComplexBurial(order *orders.Order, payloadCode string, buried *BuriedError, announce func()) {
	park := func(code protocol.QueueCode, cause QueueCause, params QueueParams) {
		d.setQueueReason(order, code, cause, params)
		if announce != nil {
			announce()
		}
	}

	// A READ THAT FAILED IS NOT A LANE THAT IS MISSING — see read_vs_missing.go.
	// Releaser for the park: the demand stays `queued`, which is in the acquiring
	// set, so the fulfillment scanner's ordinary retry brings it back here.
	lane, err := d.db.GetNode(buried.LaneID)
	if readFailed(err) {
		d.dbg("complex: could not read buried lane %d for demand %d (%v) — holding", buried.LaneID, order.ID, err)
		park(protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{Payload: payloadCode})
		return
	}
	if err != nil || lane == nil {
		d.failOrderInternal(order, codeInvalidNode, configFailureID("lane node", buried.LaneID))
		return
	}

	// The demand is queued because its bin is buried, so say so BEFORE any of the
	// dispositions below. It used to be recorded only when a contention arm fired,
	// so the ORDINARY burial — lane free, dig dispatches — was the one case that
	// recorded nothing.
	// NOT ANNOUNCED, and the distinction is the one the collapse nearly lost: this
	// WRITES the cause, the arms below decide the OUTCOME, and the station is told
	// once per outcome rather than once per write. Announcing here as well made
	// intake emit twice for one refusal, which the intake site's own event-count
	// assertion caught.
	d.setQueueReason(order, protocol.QueueStorageRearranging, CauseIntakeBuried,
		QueueParams{Lane: lane.Name, Payload: payloadCode})

	// ── THE DEMAND BECOMES THE DIG (§R.91) ────────────────────────────────
	//
	// THE HEADER HERE SAID THE OPPOSITE and is quoted rather than deleted: "THE
	// DEMAND DOES NOT BECOME THE DIG. It used to: CreateCompoundOrder(order,
	// plan) re-parented this complex order, moved it to `reshuffling`, and
	// brought it back through ResumeCompound. A dig is a SERVICE TO A LANE (plan
	// §12.40), and the one carve-out — a plain retrieve, where the dig's last leg
	// IS the demand's whole job — is not this path. So the demand stays where it
	// is and something else digs."
	//
	// The owner's ruling restates the rule the other way round: a dig is owned by
	// the demand that caused it, UNLESS a vehicle is already committed to that
	// demand. Nothing is committed here — this order is `queued`, no vendor
	// order, no robot — so it takes its own excavation. It comes back through
	// ResumeCompound exactly as the quoted paragraph describes, and that path was
	// never deleted.
	//
	// What that buys, concretely: the excavation cannot outlive the reason for
	// it. A folder's requester can cancel and leave it digging towards a bin
	// nobody wants, which is the whole of the dig_target_abandoned population;
	// a demand that IS its dig takes the dig with it when it goes.
	res := d.proposeLaneClearDig(lane, buried.Slot, order)
	switch res.outcome {
	case laneClearStarted:
		d.dbg("complex: service dig %d proposed for demand %d — %d step(s) clearing %s to reach %s",
			res.parent.ID, order.ID, res.steps, lane.Name, buried.Slot.Name)

	case laneClearLaneBusy:
		// Very often a dig serving the same wall for somebody else, which is the
		// 1:many shape a service dig is FOR. Its completion re-drives every waiter.
		//
		// NAME THAT DIG. "Somebody else's excavation on your lane" is precisely
		// the wait an operator cannot resolve from the board: the lane is not
		// theirs, the dig is not theirs, and the only actionable fact is which
		// excavation has to finish.
		park(protocol.QueueStorageRearranging, CauseLaneLocked,
			QueueParams{Lane: lane.Name, Payload: payloadCode,
				DigOrderID: digWaitFor(d.laneLock, lane.ID)})

	case laneClearNoShuffleSlot:
		// Congestion. A freed slot anywhere in the group releases it. The complex
		// sites lacked this arm while the plain path had it, so identical congestion
		// terminated a complex demand and waited for a plain one.
		park(protocol.QueueStorageRearranging, CauseNoShuffleSlot,
			QueueParams{Lane: lane.Name, Payload: payloadCode})

	case laneClearParkingHeldByDig:
		// Right of way. The lane NAMED here is the one the rule refused, not the one
		// being dug — an operator asking "why is nothing happening" needs the lane
		// that has to free, and it is somebody else's.
		parkingLane := parkingLaneOf(res.err, lane.Name)
		park(protocol.QueueStorageRearranging, CauseDigHoldsParking,
			QueueParams{Lane: parkingLane, Payload: payloadCode,
				DigOrderID: digWaitByLaneName(d.db, d.laneLock, parkingLane)})

	case laneClearBlockerClaimed:
		if res.blockerPromised {
			// The ranked take (§7) refused the steal: the holder's demand outranked
			// this dig, and it holds a PROMISE — no robot, nothing driving that bin
			// anywhere. The claimed cause would promise a drive that has not started.
			//
			// AND NO DigOrderID. That pointer names whoever holds the lane's
			// excavation lock, and on this arm creation failed and the lock was
			// dropped — so there is no dig of ours to name. Naming somebody else's
			// would point the operator at an excavation that is not what they are
			// waiting for.
			park(protocol.QueueStorageRearranging, res.blockerCause,
				QueueParams{Lane: lane.Name, Payload: payloadCode,
					HolderOrderID: res.blockerClaimant})
			return
		}
		// The commonest holder is a robot already carrying that bin out of the lane.
		park(protocol.QueueStorageRearranging, res.blockerCause,
			QueueParams{Lane: lane.Name, Payload: payloadCode,
				DigOrderID: digWaitFor(d.laneLock, lane.ID)})

	case laneClearEpisodeAlreadyDigging:
		// This demand is already being dug for, somewhere else. Not a refusal by
		// anybody else and not congestion: the plant is working on it, and raising
		// a second excavation for one bin is what put two of them in a mutual hold
		// on the rig. The releaser is that dig finishing, which re-drives this.
		// THE DIG IS THE WHOLE ANSWER HERE. "This demand is already being dug
		// for, somewhere else" is unactionable without naming where: the
		// excavation is on a DIFFERENT lane than the one just refused, so the
		// lane in this sentence is not the one that has to free.
		park(protocol.QueueStorageRearranging, CauseEpisodeAlreadyDigging,
			QueueParams{Lane: lane.Name, Payload: payloadCode,
				DigOrderID: digWaitForEpisode(res)})

	case laneClearLaneOccupied:
		// A robot from another order is inside the corridor. Congestion with the
		// shortest releaser on the board — that machine places or picks and the
		// occupancy row goes — and it is a DIFFERENT wait from CauseLaneLocked
		// above: nobody is excavating, the lane is simply in use. Filing it under
		// the dig cause would send an operator looking for an excavation that
		// does not exist.
		park(protocol.QueueStorageRearranging, CauseLaneOccupied,
			QueueParams{Lane: lane.Name, Payload: payloadCode})

	case laneClearNothingInTheWay:
		// The lane moved between the resolve and the plan, which is the outcome we
		// wanted. Keep CauseIntakeBuried; the next scan finds the bin reachable.
		d.dbg("complex: nothing left in the way of %s for demand %d — re-asking on the next scan",
			buried.Slot.Name, order.ID)

	case laneClearReadFailed:
		// A STUTTER IS NOT A FACT ABOUT THE LANE (PLAN §R.45).
		d.dbg("complex: could not read %s while planning a dig for demand %d (%v) — holding",
			lane.Name, order.ID, res.err)
		park(protocol.QueueWaitingForSlot, CauseReadFailed, QueueParams{Payload: payloadCode})

	case laneClearNoGroup:
		d.failOrderInternal(order, codeInvalidNode, fmt.Sprintf(
			"config failure: lane %s is not in a node group, so it has nowhere to park a blocker", lane.Name))

	case laneClearSlotNotInLane, laneClearUnplannable:
		// Only a person editing configuration can fix this, so no amount of waiting
		// changes it (§R.45: "config error? yeah fail loudly so the engineer can fix").
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", res.err))
	}
}

// planBuriedReshuffleAtIntake is the intake entry: the station is waiting on an
// answer to the request it just made, so every park is announced.
func (d *Dispatcher) planBuriedReshuffleAtIntake(order *orders.Order, payloadCode, stationID string, buried *BuriedError) {
	d.handleComplexBurial(order, payloadCode, buried, func() {
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
	})
}

// handleComplexBuriedOnReplay is the scanner entry: the demand is already
// acquiring, so leaving it queued with a cause IS the retry and there is nothing
// new to announce.
func (d *Dispatcher) handleComplexBuriedOnReplay(order *orders.Order, buried *BuriedError) {
	d.handleComplexBurial(order, order.PayloadCode, buried, nil)
}
