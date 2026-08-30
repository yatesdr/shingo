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
// naming safe to do in bulk. The strings are a Core vocabulary, and the bulk
// NAMING commit changed none of them: naming a value is not renaming it, and the
// histogram an engineer groups by had to stay continuous across it.
//
// (Since then exactly one value HAS been re-spelled, deliberately and on its own
// argument — see the one-character pair below. The rule is not "values never
// change", it is "a value changes only when the ambiguity costs more than the
// discontinuity", and it is decided one value at a time.)
//
// ── TWO COLLISIONS, BOTH NOW RESOLVED ─────────────────────────────────────
//
// CauseLaneLockRace and CauseBinLockRace were BOTH "lock-race" — a lane dig-lock
// race and a bin reservation race, two facts one string. The pair was kept for a
// while on the grounds that re-spelling either value rewrites what rows already
// in a plant's orders table mean, which is true and still is.
//
// It is resolved by DELETION rather than by re-spelling, and only because the
// census made that available: CauseLaneLockRace never had a production writer,
// so no row in any plant carries it and nothing historical is reinterpreted by
// its removal. "lock-race" now means exactly one thing — the bin reservation
// race — which is what it has always meant in the data. If the lane dig-lock
// race ever needs to be observable, it gets its OWN value ("lane-lock-race");
// that is safe for the same reason the deletion was, and re-declaring the
// colliding string would not be.
//
// THE ONE-CHARACTER PAIR IS NOT KEPT EITHER, by a different argument. It read:
// "CauseComplexSlotReserve ("slot-reserve") and CauseStoreSlotContended
// ("slot-reserved") are two facts one CHARACTER apart... neither is re-spelled".
// Both have live writers, both land in durable rows, and one character is not a
// distinction anybody makes on a board read under pressure — the reason the two
// were documented together is the reason to fix one of them. The complex value
// is now "complex-slot-reserve", which also makes it match its own constant
// name; the store value is untouched, so exactly one histogram series is
// interrupted rather than two, and an old "slot-reserve" row is unambiguously
// pre-change rather than newly meaning something else.
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
	// CauseLaneHeldSource — the lane is held EXCLUSIVELY by an ordinary demand
	// that resolved onto a bin in it (§R.101's source lock), not by an excavation.
	//
	// SPLIT FROM CauseLaneHeldDig AND CauseLaneDigActive ON THE RELEASER, which is
	// the only reason a cause ever splits. A dig clears when the reshuffle ends —
	// several legs, a teardown, minutes. A source lock clears when ONE robot
	// carries ONE bin out of the lane, and the order it clears for is already
	// dispatched. Different waits, different lengths, different things to go and
	// look at.
	//
	// It exists because §R.101 generalized the source hold to mode='dig' and every
	// reader kept saying "dig". An engineer sent to find the excavation behind a
	// lane-dig-active wait finds no reshuffle, no parent, no legs — because there
	// is none, and the wait was correct about the lane and wrong about the reason.
	// Not an alarm that fails to fire; an alarm with the wrong name on it.
	//
	// THE REFUSAL IT LABELS IS UNCHANGED. §R.101 rules that a demand owns the lane
	// it sources from until the bin leaves by its mover, and it still does.
	CauseLaneHeldSource QueueCause = "lane-held-source"
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
	// CauseDigHoldsParking — right of way (§R.61): the group HAS room, and the room
	// is inside a lane another dig is holding, so this dig may not plan into it.
	//
	// SPLIT FROM CauseNoShuffleSlot ON THE RELEASER, WHICH IS THE ONLY REASON A
	// CAUSE EVER SPLITS. "The group is full" clears when ANY order anywhere places
	// or picks; this clears when ONE NAMED ORDER releases ONE lane. An operator
	// reading a full group looks at the plant; an operator reading this looks at a
	// dig. Where names that lane.
	//
	// It is not CauseLaneDigActive either, and the difference is the moment: that
	// one is admission refusing a robot ENTRY to a lane it is standing at, this one
	// is the planner refusing to write a plan that would need one. Same fact about
	// the world, opposite side of the commit.
	CauseDigHoldsParking QueueCause = "dig-holds-parking"
	// CauseDemandHoldsParking — right of way refused this dig, and the lane it was
	// refused by is held by an ordinary DEMAND sourcing from it (§R.101), not by
	// an excavation.
	//
	// THE SAME SPLIT AS CauseLaneHeldSource, at the planner instead of at the
	// mouth, and split for the same reason: the releaser. CauseDigHoldsParking's
	// releaser is a reshuffle ending — several legs and a teardown. This one's is
	// one robot carrying one bin out of a lane it is already dispatched for.
	//
	// It is reachable and was measured rather than reasoned: a source lock on a
	// sibling lane produced "lane LS_SIB is held by dig 2" out of
	// DigParkingHeldError, naming an excavation that did not exist. §R.61's
	// right-of-way rule is written about digs — "a dig must not plan into a lane
	// another dig holds" — and §R.101 widened the population it removes without
	// widening the words it removes them under.
	CauseDemandHoldsParking QueueCause = "demand-holds-parking"
	// CauseEpisodeAlreadyDigging — this demand already has an excavation running,
	// so a second one is not raised for it.
	//
	// READ IT ON THE FLOOR AS "already digging for this". An operator seeing it
	// should understand that work IS happening for this part, on another lane, and
	// that nothing is stuck.
	//
	// It is NOT CauseLaneLocked, which says somebody ELSE holds the corridor. This
	// one says the wait is on our own excavation, and the distinction matters
	// because the two clear on different events and only one of them is anybody
	// else's fault.
	CauseEpisodeAlreadyDigging QueueCause = "episode-already-digging"
	// CauseDigBlockerClaimed — a bin the dig must move is hard-claimed by an order
	// outside the compound, most often a dispatched retrieve whose robot is already
	// carrying it out of the lane. Distinct from CauseLaneLocked (a whole lane
	// reserved for someone else's dig) and from CauseNoShuffleSlot (nowhere to put
	// the blockers): here the lane is diggable and the parking is there, one
	// specific bin is spoken for. It clears when that order finishes, which is why
	// it must not be filed under lock-race — the wait is a robot's drive time, not
	// a lost microsecond.
	CauseDigBlockerClaimed QueueCause = "dig-blocker-claimed"
	// CauseDigBlockerStopped — the same wall, and the opposite fact about it: the
	// order hard-claiming the bin the dig must move HAS STOPPED WITHOUT
	// TERMINATING. Nothing in the plant is going to move that bin.
	//
	// IT IS A SEPARATE CAUSE BECAUSE IT HAS A SEPARATE RELEASER, which is the only
	// thing this vocabulary is for. CauseDigBlockerClaimed's releaser is a robot
	// finishing its drive; this one's is A PERSON — §R.115: "it's really a config
	// error or an engineer needs to resolve the stopped order." Writing this
	// population under the congestion tag would tell an operator to wait for a
	// robot that is not coming, and a wait naming the wrong releaser is worse than
	// one naming none.
	//
	// FOURTH MEMBER OF THE FAMILY §R.45 OPENED: a config-fault-class wait,
	// diagnosed by a human from a named row rather than cleared by a sweep. Like
	// the slot attached to no lane, it stays LOUD with the subject named — the
	// stopped order's id is in the operator sentence and in the alarm row, because
	// a wait whose releaser is a person is worth nothing if the person cannot see
	// it.
	//
	// NOT terminal-status machinery and NOT a reaper. §R.115 refused both by name:
	// an order that stopped without terminating is usually a config fault or a
	// real breakdown, and dissolving it automatically is the machine guessing at
	// something it cannot classify.
	CauseDigBlockerStopped QueueCause = "dig-blocker-order-stopped"
	// CauseDigBlockerPromised — the third fact about the same wall (§7): a bin the
	// dig must move is PROMISED to a demand that OUTRANKS it, so the dig yielded.
	//
	// A SEPARATE CAUSE BECAUSE IT HAS A SEPARATE RELEASER, which is the only thing
	// this vocabulary is for. dig-blocker-claimed's is a robot finishing its
	// drive; dig-blocker-order-stopped's is a person. This holder has no robot at
	// all — it holds a promise, not ink — so the wait ends when that demand takes
	// its bin or ends. Filing it under dig-blocker-claimed would tell an operator
	// to wait for a drive that has not started.
	//
	// The wait is shorter than it looks: a promise on a bin is a plan to remove
	// it, so the dig's lane is cleared by the holder's own drive.
	CauseDigBlockerPromised QueueCause = "dig-blocker-promised"
	// CauseStagedOwnDig — a robot is standing at a lane's mark while the order it
	// belongs to digs that lane open with its OWN children (§R.104).
	//
	// It is the acceptance arm's wait, and it is the first cause ever written onto
	// an order that is `staged` and child-bearing at the same time — a shape this
	// tree had never seen. The robot is not stuck and nobody is refusing it: its
	// own excavation is running, and when the chapter closes Core appends its tail
	// where it stands.
	//
	// NOT CauseLaneTargetBuried, which is what the classifier said one moment
	// earlier. That cause means "a bin is in front of me and somebody should move
	// it"; this one means "and that somebody is me, and I have started". The
	// releasers differ in exactly the way that matters to whoever reads the board.
	CauseStagedOwnDig QueueCause = "staged-own-dig"
	// CauseChapterLegInFlight — the demand is in `reshuffling` and one of its own
	// dig legs is a mission the fleet still holds. §R.91 made the demand wear the
	// status its folder used to, and PopCompoundParent's "structure needs no
	// cause" reading stopped being complete the moment a real order sat there: a
	// parent whose chapter has stopped is a wait an operator can see, and it was
	// rendering blank.
	//
	// It is written by the stalled-chapter watchdog and by nothing else, which is
	// why it means what it says: NOT "a leg is running" (the ordinary case, still
	// structural and still causeless) but "the whole family has been quiet for
	// three floor ticks AND a vehicle is committed to a leg". A chapter quiet that
	// long with NO vehicle committed does not get a cause — it gets dissolved.
	CauseChapterLegInFlight QueueCause = "chapter-leg-in-flight"
	// CauseStagedDigFailed — the order is standing at its mark in `staged` (§R.104)
	// and its OWN dig chapter stopped short: a leg failed or the plan went stale
	// and was dissolved. The robot is committed and its plan is intact — the
	// splice-append resume — so it takes no transition out of `staged`, which
	// would be illegal, and no Cancel, which would end a demand whose own work is
	// not what failed. It waits for the machinery that re-asks the lane.
	//
	// The difference from CauseStagedOwnDig is the chapter's direction: that cause
	// says "my dig is running, wait for it to close"; this one says "my dig
	// STOPPED, wait for the re-ask". Two different sentences because two different
	// releasers — the operator reading the board needs to know whether the lane's
	// own excavation is coming or Core is re-raising one.
	//
	// The difference from CauseChapterLegInFlight is the waiter's shape: that one
	// is a `reshuffling` parent whose leg is a live mission; this one is a staged
	// dweller whose legs have already landed terminal. Same physical fact — a dig
	// for this demand is not running — opposite sides of the commit.
	CauseStagedDigFailed QueueCause = "staged-dig-failed"

	// ── The gate's own failures ───────────────────────────────────────────

	// CauseGateRebindUnavailable — a gate-staged order's bin has no slot to rebind to.
	CauseGateRebindUnavailable QueueCause = "gate-rebind-unavailable"
	// CauseGateAppendFailed — the fleet refused the tail append past the retry threshold.
	CauseGateAppendFailed QueueCause = "gate-append-failed"
	// CauseStationWait — the robot is parked at a wait the STATION owns, and Core
	// is not going to advance it. The line has to clear, the tooling has to
	// finish, somebody has to press Release.
	//
	// IT IS A WAIT WITH NO CORE RELEASER, AND THAT IS THE POINT. Every other
	// cause here names something Core is waiting to observe; this one names
	// something Core is waiting to be TOLD. It exists because the population had
	// no cause at all: an order dwelling at a station wait carried a blank row,
	// which is indistinguishable from one nobody had evaluated — the shape that
	// held three robots for a soak while the investigation looked for a fence
	// that was refusing them (§12.49). Nothing was refusing them; nothing could
	// see them.
	CauseStationWait QueueCause = "station-wait"
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
	// This one is a plain store's single slot; the multi-slot complex reserve is
	// CauseComplexSlotReserve, which the two used to be one character apart from.
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
	// CauseComplexSlotReserve — the multi-slot reserve did not complete. It was
	// "slot-reserve", one character from CauseStoreSlotContended's "slot-reserved"
	// and a different fact; see the type doc for why this one moved and that one
	// did not.
	CauseComplexSlotReserve QueueCause = "complex-slot-reserve"
	// CauseDropoffCapacity — a concrete storage dropoff is full or has inbound
	// traffic already committed to it.
	//
	// ── IT HAS NO LIVE WRITER, AND IT IS KEPT ANYWAY ──────────────────────
	//
	// It was the COARSE tag the two complex capacity arms wrote while throwing
	// away the answer they had just computed: CheckDropoffCapacity returns a
	// CapacityBlock whose Cause is one of dropoff-occupied / dropoff-inflight /
	// ngrp-full / capacity-check-failed, and both arms passed cap.Params through
	// while substituting this constant for cap.Cause — so the discriminator
	// survived into the operator SENTENCE and was erased from the column an
	// engineer groups by. The plain path (fulfillment/scanner.go) and the
	// planning service had always written the fine cause; only complex did not,
	// so one physical fact was filed two ways depending on which door parked it.
	//
	// The four causes have four different releasers — a bin leaving, another
	// order placing, a child of the group freeing, and a read succeeding — and
	// the inventory carries a row for each. Collapsing them cost the releaser.
	//
	// NOT DELETED, and the difference from CauseLaneLockRace is the whole reason
	// this paragraph exists. That constant was deleted because the census proved
	// it had NEVER had a production writer, so removing it reinterpreted no row
	// in any plant. This one HAS been written, for as long as complex orders have
	// queued on a full dropoff, so rows carrying "dropoff-capacity" exist at both
	// plants and mean exactly what they meant. Deleting the constant would orphan
	// them. It stays declared, with its releaser row, as a LEGACY value: read it
	// in a histogram as "a complex dropoff refusal from before the split", and do
	// not write it from new code.
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
	// CauseNGRPAtLevel — a maintained group is holding what it was told to hold,
	// so it will not take another carrier of this type.
	//
	// NOT CauseNGRPFull, which says every position is physically occupied. This
	// group has empty positions and they are spoken for by a number somebody
	// configured — an operator who read "full" would go and look at a group with
	// space in it. The two clear on different things too: full clears when a
	// carrier leaves, this clears when a carrier leaves OR when somebody raises
	// the level.
	CauseNGRPAtLevel QueueCause = "ngrp-at-level"
	// CauseFinderGroupFenced — the need named a STRICT maintained group it is not
	// supported at. The group holds carriers; they are not this asker's.
	//
	// A DISPOSITION, NOT A FILTER, and that is the whole reason it is a separate
	// cause from finder-group-empty. A plant-wide scan that cannot see a fenced
	// group's carriers should say nothing about them — it looked everywhere it
	// was allowed to and found nothing. A need that NAMED the group is different:
	// somebody configured a claim to source from there, and the honest answer is
	// "that group is not yours", not "that group is empty". The second sends an
	// operator to look for material that is standing right there.
	//
	// NEVER SILENTLY WIDENED EITHER. The need does not fall through to the
	// plant-wide scan; a scoped need that widens is the Hopkinsville
	// wrong-supermarket pull, and being fenced is not a reason to start doing it.
	CauseFinderGroupFenced QueueCause = "finder-group-fenced"
	// CauseFinderSourceUnreadable — an empty or full search did not RUN. The
	// query errored; Core has no idea whether material is there.
	//
	// IT IS A DIFFERENT FACT FROM AN EMPTY PLANT, and separating the two is the
	// whole of MG3-1a. Every finder call site used to read `err != nil || bin ==
	// nil` as one condition, which made "the query threw" indistinguishable from
	// "nothing matched" — and the MG2 campaign proved that is not a theoretical
	// concern: a query naming a CTE that did not exist threw on every call, every
	// caller read it as "no empty found", and the whole gate came back clean
	// (SIM-CAMPAIGN-mg2 §2).
	//
	// The store side already drew the line — sql.ErrNoRows for none-found, a
	// wrapped error otherwise. The call sites were the ones collapsing it.
	//
	// It sits in the undetermined family beside CauseLoaderSourceUnreadable and
	// CauseFinderAccessibilityUnreadable, for the same reason all three exist: a
	// histogram must be able to say "Core declined to answer" apart from "the
	// plant is out of material", or an outage reads as a shortage.
	CauseFinderSourceUnreadable QueueCause = "finder-source-unreadable"
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
	//
	// These four were declared and then not adopted — capacity.go kept writing
	// the literals. It compiled because the field is typed QueueCause and an
	// untyped string constant converts implicitly, so nothing flagged it and the
	// constants read as dead for a while. They are wired now; a typo is a
	// compile error rather than a histogram bucket that silently matches nothing.

	// CauseDropoffOccupied — the destination already holds a bin.
	CauseDropoffOccupied QueueCause = "dropoff-occupied"
	// CauseDropoffInflight — another order is already inbound to it.
	CauseDropoffInflight QueueCause = "dropoff-inflight"
	// CauseNGRPFull — every child of the destination group is spoken for.
	CauseNGRPFull QueueCause = "ngrp-full"
	// CauseCapacityCheckFailed — the capacity read failed; undetermined, not full.
	CauseCapacityCheckFailed QueueCause = "capacity-check-failed"
)
