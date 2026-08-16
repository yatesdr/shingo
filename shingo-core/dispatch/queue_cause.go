package dispatch

// QueueCause is the engineer-facing call-site tag that lands on
// orders.queue_cause — WHERE in the pipeline a wait arose, as opposed to
// protocol.QueueCode, which is WHY in words an operator reads.
//
// It is a type rather than a string so the set is enumerable and a typo is a
// compile error. Both matter more than they look: these values are what an
// engineer groups by when a queue code trends wrong, so a value that exists in
// one spelling at one site and another spelling at another is a forensic record
// that quietly under-counts.
//
// NEVER CROSSES THE WIRE. domain.Order documents it as Core-side only, and
// nothing in Edge, the UI, or a template reads it — checked before this type
// existed. So the underlying strings are free to change; they are a Core
// vocabulary, not a contract. They are NOT changing here.
//
// ── SCOPE: EVERY CAUSE THIS PACKAGE AND fulfillment/ SET ──────────────────
//
// It was the lane/gate family only, and the rest were left VISIBLE rather than
// named: setQueueReason takes this type, so an un-named cause appeared at its
// call site as an explicit QueueCause("…") conversion, and that conversion was
// the to-do list. The list is now worked — every cause a Core wait can carry is
// a constant below, and TestNoLiteralQueueCauseConversions keeps it that way.
//
// The reason it had to be finished is causeReleasers (queue_releasers.go): the
// inventory pairs each cause with what ends the wait, and it can only be TOTAL
// over a set that is enumerable. A literal conversion is a cause no table can
// see and no drift test can miss.
//
// NEVER CROSSES THE WIRE — see above; that is unchanged and is what makes the
// naming safe to do in bulk. The strings are a Core vocabulary, and NOT ONE OF
// THEM CHANGES HERE. Naming a value is not renaming it: the histogram an
// engineer groups by has to stay continuous across this commit, so every
// constant below carries the literal its call site already wrote.
//
// ── TWO COLLISIONS, BOTH KEPT AND BOTH NOW VISIBLE ────────────────────────
//
// CauseLaneLockRace and CauseBinLockRace are BOTH "lock-race" — a lane dig-lock
// race and a bin reservation race, two facts one string. CauseComplexSlotReserve
// ("slot-reserve") and CauseStoreSlotContended ("slot-reserved") are two facts
// one CHARACTER apart.
//
// Neither is collapsed and neither is re-spelled: changing either value rewrites
// what the forensic record means for one of the pair, which is a behaviour change
// this commit does not get to make. What changes is that both collisions are now
// declared in one block where a reader trips over them, instead of living in two
// files that never mention each other. The lock-race pair is carried into
// causeReleasers as a single row that says so, and is reported as a finding.
type QueueCause string

// The lane/gate family. Values are unchanged from the literals they replace —
// this commit renames nothing, it only makes the names exist.
const (
	// ── Ordering: the move is safe, it is not this order's turn ───────────
	// Set by the tiered-entry classifier (lane_entry.go). These two answer
	// "which of the waiting ones goes first", NOT "may this move happen" —
	// the distinction admission is built around, kept visible here because
	// the two families otherwise look alike in a queue_cause histogram.

	// CauseLaneDeeperPending — Tier 2: a deeper cross-origin store has not placed yet.
	CauseLaneDeeperPending QueueCause = "lane-deeper-pending"
	// CauseLaneGroupActive — Tier 3: an active cross-origin group holds the lane.
	CauseLaneGroupActive QueueCause = "lane-group-active"

	// ── Admission: the lane cannot take this move now ─────────────────────

	// CauseLaneDigActive — someone else's dig holds the lane (gate retrieve classifier).
	CauseLaneDigActive QueueCause = "lane-dig-active"
	// CauseLaneTargetBuried — a shallower bin still sits in front of the wanted slot.
	CauseLaneTargetBuried QueueCause = "lane-target-buried"
	// CauseLaneHeldDig — the mouth acquire lost to a dig holder.
	CauseLaneHeldDig QueueCause = "lane-held-dig"
	// CauseLaneHeldTraffic — the mouth acquire lost to a different-mode holder.
	// DEFINITE: every one of the order's lanes was read and none held a dig.
	CauseLaneHeldTraffic QueueCause = "lane-held-traffic"
	// CauseLaneHeldUnreadable — the mouth acquire was refused and at least one of
	// the order's lanes could not be read, so a dig cannot be ruled out. It is
	// NOT a third kind of contention; it is the absence of an answer, and it
	// exists so that absence stops being reported as lane-held-traffic.
	CauseLaneHeldUnreadable QueueCause = "lane-held-unreadable"
	// CauseLaneOccupied — a robot from another order is inside the lane (Hold B).
	// Distinct from CauseLaneHeldDig: a dig CLAIMS a lane for a whole reshuffle,
	// occupancy says somebody is physically in it right now, and the two clear on
	// different signals.
	CauseLaneOccupied QueueCause = "lane-occupied"
	// CauseLaneLocked — the reshuffle planner found the lane already dug by another.
	CauseLaneLocked QueueCause = "lane-locked"
	// CauseLaneLockRace — TryLock lost the race for the lane's dig lock. See the
	// type doc: fulfillment sets this same string for an unrelated BIN race.
	CauseLaneLockRace QueueCause = "lock-race"
	// CauseIntakeBuried — the target was buried at intake; the arms after it
	// narrow to a more specific cause when they can.
	CauseIntakeBuried QueueCause = "intake-buried"
	// CauseReshuffleCongestion — the blanket tag the two scanner arms wrote for
	// every transient reshuffle refusal. Kept as the fallback for a wait whose
	// specific cause is not mapped, never as a first choice.
	CauseReshuffleCongestion QueueCause = "reshuffle-congestion"
	// CauseNoShuffleSlot — the dig had nowhere to park its blockers (findShuffleSlots
	// came up short). Clears when any order releases a slot anywhere in the group.
	CauseNoShuffleSlot QueueCause = "no-shuffle-slot"
	// CauseDigBlockerClaimed — a bin the dig must move is hard-claimed by an order
	// outside the compound, most often a dispatched retrieve whose robot is already
	// carrying it out of the lane. Distinct from CauseLaneLocked (a whole lane
	// reserved for someone else's dig) and from CauseNoShuffleSlot (nowhere to put
	// the blockers): here the lane is diggable and the parking is there, one
	// specific bin is spoken for. It clears when that order finishes, which is why
	// it must not be filed under lock-race — the wait is a robot's drive time, not
	// a lost microsecond.
	CauseDigBlockerClaimed QueueCause = "dig-blocker-claimed"

	// ── The gate's own failures ───────────────────────────────────────────

	// CauseGateRebindUnavailable — a gate-staged order's bin has no slot to rebind to.
	CauseGateRebindUnavailable QueueCause = "gate-rebind-unavailable"
	// CauseGateAppendFailed — the fleet refused the tail append past the retry threshold.
	CauseGateAppendFailed QueueCause = "gate-append-failed"
	// CauseGateReleaseFailed — the classifier ADMITTED this dweller and the release
	// itself then failed: the re-bind found no slot, the segment could not be
	// built, the append errored below the retry threshold. The order stays a
	// candidate and the next pass retries.
	//
	// It exists because that arm wrote nothing at all. A dweller whose release
	// keeps failing is indistinguishable, on the row, from one nobody has
	// evaluated — and worse than blank on a repeat pass, since the cause is
	// CLEARED on entry and this arm runs after admission, so a previously-refused
	// order loses its old cause and gains none. Measured on the lane-stress rig
	// 2026-08-10: orders 6 and 40 dwelling 13-16m under "no empty slot in lane",
	// with nothing on the row to say so.
	CauseGateReleaseFailed QueueCause = "gate-release-failed"
	// CauseGatePickupElsewhere — the bin this entry's PICKUP is for is not in this
	// lane. A definite answer about the plant, not a failed read, and therefore a
	// refusal: it clears when the bin comes back, when the plan re-binds against
	// where it actually sits, or when the demand is re-planned.
	//
	// It exists because that condition used to arrive as a bare error, which the
	// evaluator can only log and skip — no usable cause, no heal-dig proposal, and
	// no abandon-sweep bound (a gate-staged order is exempt). Ten hours of one
	// robot on the lane-stress rig.
	CauseGatePickupElsewhere QueueCause = "gate-pickup-elsewhere"

	// ── The fleet refused the CREATE ──────────────────────────────────────

	// CauseFleetRefusedCreate — the fleet would not take the order. Congestion in
	// the robot system, not a fault in the plan: the vocabulary already says so
	// (protocol.QueueFleetUnavailable, "Robot system not responding — retrying").
	//
	// THE VALUE IS THE ONE THE PLAIN PATH ALREADY WROTE. fulfillment/scanner.go
	// set the literal "fleet-error" at its two fleet-refusal arms long before this
	// type existed; naming it here rather than minting a new string keeps the
	// histogram continuous across the two paths, which is the whole reason an
	// engineer groups by this column. Typing it is the change; the string is not.
	CauseFleetRefusedCreate QueueCause = "fleet-error"

	// ── Undetermined: a read failed, so the answer is not known ───────────
	// These are the fail-closed arms, and they are their own group on purpose.
	// A wait tagged with one of these is NOT a routine wait — it is Core
	// declining to answer, and an engineer reading a histogram needs to be able
	// to tell those apart from a lane that was honestly busy.

	// CauseLaneAcquireError — the mouth acquire could not be read (fulfillment).
	CauseLaneAcquireError QueueCause = "lane-acquire-error"
	// CauseLaneEntryError — the tiered-entry check could not be read (fulfillment).
	CauseLaneEntryError QueueCause = "lane-entry-error"
	// CauseAdmissionError — a physical question could not be read, so admission
	// declined to answer and the caller held the move. The arm that had no cause
	// at all: a compound leg held on an unreadable lane wrote nothing to its row,
	// so it was indistinguishable from a leg nobody had looked at yet — the same
	// defect the refusal arm beside it had already been given a cause for.
	CauseAdmissionError QueueCause = "admission-error"
	// CauseReadFailed — a read Core needed to make a decision did not answer, so
	// the order is holding until it does. NOT lane-busy, and the separation is
	// the point: during a database outage dozens of orders park at once, and an
	// operator surface that renders that as congestion sends someone to look at
	// lanes. This says "the system could not read", which is a different
	// investigation and a different fix.
	CauseReadFailed QueueCause = "read-failed"
	// CauseLoaderSourceUnreadable — the dedicated-loader pool could not be read
	// (the source node, the loader home, the loader, its members, or the bins on
	// them). Kept out of finder-pool-empty on purpose: an empty pool is a fact
	// about the plant that an operator can act on, this is a failed read that
	// says nothing about whether material is there.
	CauseLoaderSourceUnreadable QueueCause = "loader-source-unreadable"

	// ── Sourcing and reservation contention (fulfillment/) ────────────────
	// The pre-dispatch family. Every one of these parks an order in the
	// ACQUIRING set, whose floor is the fulfillment scanner's own periodic
	// sweep — which is why none of them was ever part of the F-22 class.

	// CauseDestNodeUnresolved — the delivery node could not be resolved right now.
	// A DESTINATION failure, deliberately parked under waiting_for_slot rather
	// than waiting_for_material: it used to point the operator at inventory for a
	// node lookup (F6 of the 2026-07-20 queue-reason study).
	CauseDestNodeUnresolved QueueCause = "dest-node-unresolved"
	// CauseStoreSlotContended — ReserveStorageDropoff lost its destination slot.
	// ONE CHARACTER from CauseComplexSlotReserve and a different fact; see the
	// type doc. This one is a plain store's single slot.
	CauseStoreSlotContended QueueCause = "slot-reserved"
	// CauseBinLockRace — the bin was reserved by a concurrent order in the
	// Find→Reserve window. SAME STRING as CauseLaneLockRace and a different fact;
	// see the type doc.
	CauseBinLockRace QueueCause = "lock-race"
	// CauseClaimFailed — a pending hold was reaped, or a bin was claimed by
	// another order between reserve and confirm. Set on both the plain and the
	// complex path, which is why it is one constant rather than two.
	CauseClaimFailed QueueCause = "claim-failed"
	// CauseHeldBinMissing — the order reached the held-bin dispatch path with no
	// bin_id. The routing into that path is `order.BinID != nil`, so this is a
	// CONSTRUCTION BUG rather than a wait, and it is a cause only so the row is
	// not blank while it repeats: the scanner re-enters every tick and would
	// otherwise leave an order sitting queued forever with nothing on it. Its
	// releaser is a person.
	CauseHeldBinMissing QueueCause = "held-bin-missing"

	// ── Complex-order preflight (complex_dispatch.go) ─────────────────────
	// Also pre-dispatch, and also floored by the scanner: DispatchPreparedComplex
	// is gated on IsAcquiring, so an order carrying any of these is by
	// construction still in the acquiring set.

	// CauseNGRPResolve — a step still names a node GROUP that has no free child.
	CauseNGRPResolve QueueCause = "ngrp-resolve"
	// CauseReserveHolding — the complex reserve is incomplete. QueueParams.Partial
	// distinguishes "holding part of the set" from "holding nothing and blocked on
	// every need" — the SPR ALN_006 lie was rendering the second as the first.
	CauseReserveHolding QueueCause = "reserve-holding"
	// CauseComplexSlotReserve — the multi-slot reserve did not complete. ONE
	// CHARACTER from CauseStoreSlotContended and a different fact; see the type doc.
	CauseComplexSlotReserve QueueCause = "slot-reserve"
	// CauseDropoffCapacity — a concrete storage dropoff is full or has inbound
	// traffic already committed to it.
	CauseDropoffCapacity QueueCause = "dropoff-capacity"
	// CauseSwapHold — a two-robot swap leg is waiting on its sibling.
	CauseSwapHold QueueCause = "swap-hold"

	// ── The finder's tiers (source_finder.go) ─────────────────────────────
	//
	// FOUND BY THE OBSERVED-VS-DECLARED CHECK, ON THE RIG, NOT BY THE GREP.
	// These reach setQueueReason through SourceOutcome.QueueCause, which was a
	// bare `string` — so no call site ever wrote QueueCause("…"), the type never
	// forced a name, and the literal-conversion guard could not see them. The
	// plant's own rows could: soakstat's cross-check reported orders parked under
	// finder-node-empty and finder-group-empty with no row in the inventory.
	//
	// That is the check working exactly as intended, and it is the argument for
	// having built it: a vocabulary audited only from the code found 12 of 18.
	// The field is QueueCause-typed now, which is what makes these six
	// enumerable at all.
	//
	// EVERY ONE IS THE SAME KIND OF WAIT — no material of the wanted shape in the
	// scope this tier searched — differing only in the scope, which is why the
	// tier is the whole distinction and the tags are kept separate rather than
	// collapsed to "no material".

	// CauseFinderNodeEmpty — the named source node holds nothing usable.
	CauseFinderNodeEmpty QueueCause = "finder-node-empty"
	// CauseFinderGroupEmpty — no child of the source node group holds one.
	CauseFinderGroupEmpty QueueCause = "finder-group-empty"
	// CauseFinderPoolEmpty — a dedicated loader's pool is dry.
	CauseFinderPoolEmpty QueueCause = "finder-pool-empty"
	// CauseFinderPlantEmpty — the widest search found nothing anywhere.
	CauseFinderPlantEmpty QueueCause = "finder-plant-empty"
	// CauseFinderNoFullCarrier — carriers exist but none is full.
	CauseFinderNoFullCarrier QueueCause = "finder-no-full-carrier"
	// CauseFinderAccessibilityUnreadable — a candidate's reachability could not be
	// read, so the finder declined to answer rather than guessing. The finder's
	// member of the undetermined family.
	CauseFinderAccessibilityUnreadable QueueCause = "finder-accessibility-unreadable"
	// CauseFinderNoEmptyOfType — the group has empties, none of the TYPE a loader
	// declared. It waits rather than taking another: "a declared mix that is
	// abandoned when inconvenient is not a mix".
	CauseFinderNoEmptyOfType QueueCause = "finder-no-empty-of-type"

	// ── Intake ────────────────────────────────────────────────────────────

	// CauseIntakeResolve — a complex order's steps could not be resolved at
	// intake; it queues and the scanner re-resolves.
	CauseIntakeResolve QueueCause = "intake-resolve"

	// ── Dropoff capacity (capacity.go) ────────────────────────────────────
	// Same story as the finder tiers: CapacityDetail.Cause was a bare string.

	// CauseDropoffOccupied — the destination already holds a bin.
	CauseDropoffOccupied QueueCause = "dropoff-occupied"
	// CauseDropoffInflight — another order is already inbound to it.
	CauseDropoffInflight QueueCause = "dropoff-inflight"
	// CauseNGRPFull — every child of the destination group is spoken for.
	CauseNGRPFull QueueCause = "ngrp-full"
	// CauseCapacityCheckFailed — the capacity read failed; undetermined, not full.
	CauseCapacityCheckFailed QueueCause = "capacity-check-failed"
)
