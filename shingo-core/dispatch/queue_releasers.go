package dispatch

// THE RELEASER INVENTORY — for every wait Core can record, what ends it.
//
// ── WHY IT IS A TABLE AND NOT PROSE ───────────────────────────────────────
//
// The doctrine is that every machine-owned wait has (a) a named event releaser,
// (b) a periodic floor that re-evaluates it, and (c) a record when the floor is
// what freed it. Prose cannot be asserted. F-22 was not a missing idea — the
// evaluator's own doc said "a dropped event costs only latency until the next
// firing" — it was that nothing checked whether a next firing could exist. Every
// individual comment was defensible; nothing connected them.
//
// So the connection is data. causeReleasers is TOTAL over the QueueCause
// constants (TestEveryQueueCauseHasAReleaser), the populations it references
// each carry an event set and a floor (TestEveryWaitPopulationHasBothPaths), and
// the events it names are really subscribed to the re-driver that serves that
// population (engine's TestDeclaredReleaserEventsAreSubscribed).
//
// ── AND IT IS THE FLOOR'S VOCABULARY ──────────────────────────────────────
//
// The liveness floor records the cause an order was carrying when the floor —
// rather than an event — freed it. `what` is the sentence that record prints:
// it says what SHOULD have ended the wait, so the record reads as "this event
// did not fire" rather than "something was slow". The histogram of floor
// releases grouped by cause is therefore a ranked worklist of missing emitters,
// which is the artifact the emitter hunt runs on.
//
// ── HONEST ENTRIES ONLY ───────────────────────────────────────────────────
//
// A cause whose row cannot be written truthfully carries a `finding` instead of
// a plausible sentence. Two exist today and both are reported rather than
// papered over: the fleet-refusal cause has NO event (nothing emits "the fleet
// became willing"), and one declared cause has no producer at all.

// WaitPopulation names a set of orders that wait together because one mechanism
// re-drives all of them. It is the unit the wiring and the floors are built on —
// causes are what a wait MEANS, populations are how it ends.
type WaitPopulation string

const (
	// PopAcquiring is {queued, sourcing}: pre-dispatch, no robot committed. The
	// fulfillment scanner is both its event consumer and its floor.
	PopAcquiring WaitPopulation = "acquiring"
	// PopGateStaged is an order `staged` at a lane's mark — a robot physically
	// parked, holding an unsealed waybill only Core can append to. The lane-gate
	// evaluator releases it.
	PopGateStaged WaitPopulation = "gate-staged"
	// PopCompoundLeg is a dig leg Core has not yet handed to the fleet
	// (orders.AwaitingFleetSQL). It writes no status while it waits, so the lane
	// re-drive is the only thing that can find it.
	PopCompoundLeg WaitPopulation = "compound-leg"
	// PopCompoundParent is a compound parent sitting in `reshuffling` while its
	// children run. It is in the table because the status partition classifies
	// `reshuffling` to it.
	//
	// IT CARRIES NO CAUSE, and briefly it carried one. A parent in `reshuffling`
	// waits because its children are not finished, which is structural rather than
	// a refusal, and structure needs no cause. The exception was a dig holding its
	// lane for the bin it had uncovered — finished children, still waiting — and
	// that hold has been replaced by a handoff to the order collecting the bin, so
	// the exception is gone and this population is structural again.
	PopCompoundParent WaitPopulation = "compound-parent"
	// PopStationWait is an order `staged` at a wait the STATION owns — the swap
	// choreography's own gates, WaitKindStation. It is the fourth population, and
	// it was invisible until W1 made ownership explicit.
	//
	// IT IS DELIBERATELY UNFLOORED, which is the whole reason it needed naming.
	// Every other population here is machine-owned: Core knows the precondition
	// and may re-drive it. This one's precondition is a fact only the station can
	// observe — the line has cleared, the tooling is done, the operator is ready
	// — so a periodic pass that "freed" one would drive a robot into a cell
	// somebody is still working in. The floor is not missing; it is refused.
	//
	// What it needs instead is DWELL MONITORING: somebody should be told a
	// station wait has been standing for a long time, because the usual cause is
	// that nobody knows it is their turn. That surface is item 7's, and the
	// Core-side hard release (W3) is the escape hatch until it exists.
	PopStationWait WaitPopulation = "station-wait"
	// PopNone is the honest answer for a declared cause nothing produces. It is
	// not a population; it is the absence of one, named so the totality test can
	// tell "no orders wait under this" from "nobody wrote the row".
	PopNone WaitPopulation = "none"
)

// WaitOwner is the axis W1 added: who may advance the waits in a population.
// It is what decides whether a population may have a floor at all.
type WaitOwner string

const (
	// OwnerCore — the precondition is a fact Core observes, so Core may re-drive
	// it and MUST have a floor for it. Every F-22 instance lives here.
	OwnerCore WaitOwner = "core"
	// OwnerStation — the precondition is a fact only the station observes. Core
	// must NOT auto-release these; the correct backstop is telling a human, not
	// moving a robot.
	OwnerStation WaitOwner = "station"
)

// populationReleaser is the mechanism half: which events re-ask the question for
// this population, which periodic pass backstops them, and the function both go
// through. The event names are STRINGS because this package cannot import
// engine (engine imports dispatch); engine's subscription test resolves them.
type populationReleaser struct {
	population WaitPopulation
	// owner decides whether this population may be re-driven at all. OwnerCore
	// means Core observes the precondition and therefore owes it a floor;
	// OwnerStation means Core must not, and `floor` states the refusal instead of
	// naming a pass.
	owner WaitOwner
	// redriver is the function an event handler and the floor BOTH call. One
	// name here is the claim that the floor is a trigger for existing
	// level-triggered machinery rather than a second decision-maker.
	redriver string
	events   []string
	floor    string
	// noFloorBecause is required when owner is OwnerStation and forbidden
	// otherwise: an unfloored population must say why the absence is a decision
	// rather than the F-22 gap, and a floored one has nothing to excuse.
	noFloorBecause string
}

// waitPopulations is the mechanism table. Every population has both paths, which
// is the doctrine's (a) and (b), and TestEveryWaitPopulationHasBothPaths asserts
// it rather than trusting this comment.
var waitPopulations = []populationReleaser{
	{
		population: PopAcquiring,
		owner:      OwnerCore,
		redriver:   "fulfillment.Scanner.RunOnce",
		events: []string{
			"EventBinUpdated", "EventOrderCompleted", "EventOrderCancelled",
			"EventOrderFailed", "EventOrderSkipped", "EventOrderQueued",
			"EventBlockCompleted",
		},
		floor: "fulfillment.Scanner.StartPeriodicSweep (60s)",
	},
	{
		population: PopGateStaged,
		owner:      OwnerCore,
		redriver:   "Dispatcher.EvaluateLaneReleases",
		events: []string{
			"EventBlockCompleted", "EventBinEnteredTransit", "EventBinUpdated",
			"EventOrderCompleted", "EventOrderCancelled", "EventOrderFailed",
			"EventOrderSkipped",
		},
		floor: "Dispatcher.SweepLaneWaiters (60s)",
	},
	{
		population: PopCompoundLeg,
		owner:      OwnerCore,
		redriver:   "Dispatcher.RedriveHeldCompoundLegs",
		events: []string{
			"EventBlockCompleted", "EventBinEnteredTransit", "EventBinUpdated",
			"EventOrderCompleted", "EventOrderCancelled", "EventOrderFailed",
			"EventOrderSkipped",
		},
		floor: "Dispatcher.SweepLaneWaiters (60s)",
	},
	{
		// Already floored before this batch — AdvanceStuckReshuffleParents was the
		// second instance of the shape the lane floor is the third and fourth of.
		// Listed so the doctrine's table is the whole picture rather than the part
		// this batch happened to build.
		population: PopCompoundParent,
		owner:      OwnerCore,
		redriver:   "Dispatcher.AdvanceCompoundOrder",
		events: []string{
			"EventOrderCompleted", "EventOrderCancelled", "EventOrderFailed",
			"EventOrderSkipped",
		},
		floor: "ReconciliationService.AdvanceStuckReshuffleParents (reconcile interval)",
	},
	{
		// THE FOURTH POPULATION, AND THE ONE THAT MUST NOT BE FLOORED.
		//
		// It was invisible until W1: an order `staged` at a WaitKindStation wait
		// belongs to no machine-owned set — IsGateStaged is false (not a lane
		// wait), it is not a pending leg, and it is not acquiring — so no floor
		// swept it and no inventory row described it. Three robots sat in exactly
		// this gap for a whole soak while everyone looked for the fence that was
		// refusing them (§12.49). Nothing was refusing them; nothing could see
		// them.
		//
		// Naming it does NOT mean giving it a floor. The events are real — the
		// station releases, and Core's fence lets it — but the periodic pass that
		// backstops every other population is refused here, because "nobody has
		// pressed Release" is not a condition Core may resolve by pressing it.
		population: PopStationWait,
		owner:      OwnerStation,
		redriver:   "Dispatcher.HandleOrderRelease (station-initiated)",
		events:     []string{"protocol.TypeOrderRelease (from the station)"},
		floor:      "",
		noFloorBecause: "the precondition is a fact only the station can observe — the line has " +
			"cleared, the tooling is done, the operator is ready. A periodic pass that released one " +
			"would drive a robot into a cell somebody is still working in, which is the exact " +
			"collision the wait exists to prevent. The right backstop is DWELL MONITORING (item 7): " +
			"tell a human that a station wait has been standing, because the usual cause is that " +
			"nobody knows it is their turn. Core's hard release (W3) is the escape hatch meanwhile, " +
			"and it is deliberately an operator action rather than a timer.",
	},
}

// causeReleaser is the meaning half: which populations can carry this cause, the
// sentence describing what ends it, and the two flags that make the table
// answerable rather than merely descriptive.
type causeReleaser struct {
	cause QueueCause
	// populations is every set an order carrying this cause can be sitting in.
	// More than one is normal and not a smell: the same physical refusal reads
	// the same whether the order is parked pre-dispatch or dwelling at a mark,
	// which is the point of sharing the vocabulary across both.
	populations []WaitPopulation
	// what the floor's recovery record prints — what SHOULD have ended this wait.
	what string
	// bridgeNote, when non-empty, means THE A BATCH MUST READ THIS ROW: the
	// releaser chain or the frequency of this cause involves the expose-bridge
	// machinery (pending_lane_extensions, HandleBinTransitForLaneLock, the
	// transferred lock outliving its compound). The marked subset is exactly what
	// the deletion has to re-cover, which turns "nothing depends on the bridge"
	// from a hope into a checked list.
	bridgeNote string
	// finding, when non-empty, is why this row could not be written honestly. It
	// is data, not a TODO: the entry still exists so totality holds, and the text
	// is the defect.
	finding string
}

var causeReleasers = []causeReleaser{
	// ── Ordering: the move is safe, it is not this order's turn ───────────
	{
		cause:       CauseLaneDeeperPending,
		populations: []WaitPopulation{PopAcquiring, PopGateStaged},
		what:        "the deeper cross-origin store PLACES its bin, dropping its inbound mouth row",
	},
	{
		cause:       CauseLaneGroupActive,
		populations: []WaitPopulation{PopAcquiring, PopGateStaged},
		what:        "the active cross-origin group finishes with the lane",
	},

	// ── Admission: the lane cannot take this move now ─────────────────────
	{
		cause:       CauseLaneDigActive,
		populations: []WaitPopulation{PopAcquiring, PopGateStaged, PopCompoundLeg},
		// TWO RELEASE MOMENTS, ONE RELEASER. An expose dig's lock used to be
		// TRANSFERRED to the complex parent and dropped later, on the parent's
		// pickup — so this cause had two release CHAINS and the second one lived in
		// machinery outside the compound. That transfer is gone with the hand-back it
		// existed for. What replaced it is not a second chain but an earlier moment
		// on the same one: flip 2 drops the claim when the dig's last blocker leaves
		// the lane (maybeReleaseDigOnLastBlockerOut), and the compound's teardown
		// still covers every case it does not (unlockLaneForCompound). Both wake the
		// lane they free, which is what makes this cause's releaser real rather than
		// eventual.
		//
		// AMENDED BY ARM 2, and the amendment lengthens this wait for one shape.
		// A SERVICE dig — one raised to clear a lane for somebody else — now holds
		// past its last blocker until the bin it uncovered is collected, because
		// dropping the claim there left that bin exposed to the next order's
		// shuffle slot with only its claim protecting it. So a waiter under this
		// cause behind a service dig waits for a retrieval as well as an
		// excavation. The releaser is the same call; what it asks is one question
		// longer.
		//
		// IT MATTERS MORE SINCE THE OUTBOUND DWELL, because the population carrying
		// this cause now includes a robot standing in a lane holding a bin: its
		// release-time resolver was refused by a foreign dig's claim, and it walks
		// its remaining candidates before it settles here. A dweller under this cause
		// has been told there is nowhere it may legally put the blocker down — not
		// that the group is full, which is CauseNoShuffleSlot and clears differently.
		what: "the dig holding this lane releases it — at its last blocker's exit (flip 2, or for a " +
			"service dig when the bin it uncovered is collected) or at " +
			"its teardown (unlockLaneForCompound); both evaluate the lane they free",
	},
	{
		cause:       CauseLaneTargetBuried,
		populations: []WaitPopulation{PopAcquiring, PopGateStaged, PopCompoundLeg},
		what:        "the bin in front is moved — by a dig, or by whoever claimed it carrying it out",
	},
	{
		cause:       CauseLaneHeldDig,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the dig holding the mouth releases it",
	},
	{
		cause:       CauseLaneHeldTraffic,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the different-mode holder releases its mouth row (placement, pickout, or terminal)",
	},
	{
		cause:       CauseLaneHeldUnreadable,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the lane becomes readable — this is an absence of an answer, not a busy lane",
	},
	{
		cause:       CauseLaneOccupied,
		populations: []WaitPopulation{PopAcquiring, PopGateStaged, PopCompoundLeg},
		what:        "the robot inside the lane places or picks, releasing its occupancy row",
	},
	{
		cause:       CauseLaneLocked,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the other reshuffle finishes and drops its lane lock",
	},
	{
		// ONE ROW FOR TWO CONSTANTS, and that is not a modelling choice — it is
		// forced. CauseLaneLockRace and CauseBinLockRace are both "lock-race", the
		// table is keyed by the VALUE an order actually carries, and TOTALITY
		// refuses a second row for the same key. The collision cannot be papered
		// over here even if somebody wanted to.
		cause:       CauseLaneLockRace,
		populations: []WaitPopulation{PopAcquiring},
		what:        "immediate — whichever race was lost, the winner proceeds and this order re-plans on the next scan",
		finding: "ONE STRING, TWO FACTS. This value is written by CauseLaneLockRace (a lane DIG-LOCK " +
			"race, dispatch) and by CauseBinLockRace (a BIN reservation race in the Find→Reserve " +
			"window, fulfillment). Different waits, different things being contended — and a " +
			"queue_cause histogram cannot separate them, so this row describes both and is exact " +
			"about neither. Kept rather than collapsed: re-spelling either value rewrites what rows " +
			"already in the plant's orders table mean. The two DO share a releaser shape (both clear " +
			"as soon as the winner moves on, both are floored by the scanner), which is why the " +
			"collision costs forensics rather than liveness.",
	},
	{
		cause:       CauseIntakeBuried,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the dig planned for this burial completes and the parent re-resolves",
	},
	{
		cause:       CauseReshuffleCongestion,
		populations: []WaitPopulation{PopAcquiring},
		what:        "whatever the more specific arms would have named — this is the unmapped fallback",
	},
	{
		cause: CauseNoShuffleSlot,
		// TWO POPULATIONS SINCE THE OUTBOUND DWELL, and the second one is a ROBOT.
		//
		// PopAcquiring is the old one: a dig that cannot be PLANNED because the
		// group has no room, parked as a row with nothing committed. PopGateStaged
		// is new and is the expensive one — a dig leg that has already lifted a
		// blocker and is standing in the lane it is digging while Core looks for
		// somewhere to put it. Same physical fact, same releaser, and that is
		// exactly why it is one cause and not two: the table's own rule is that a
		// refusal reads the same whether the order is parked pre-dispatch or
		// dwelling at a mark.
		//
		// BOTH OF LAW 8'S PATHS ALREADY HOLD FOR THE NEW POPULATION, which is what
		// made the dwell safe to build on the existing identity rather than a new
		// one: the dweller's WaitLane is the DUG lane — a real lane — so
		// gateStagedForLane sees it, SweepLaneWaiters (60s) sweeps it, and the
		// floor's loud arm reports it as a committed robot standing still. The
		// EVENTS are the half that had to be widened: a dweller is woken by a slot
		// freeing anywhere in its GROUP, not by its own lane clearing (which it is
		// itself blocking) — DwellerLanesSharingGroupWith, wired onto the same
		// pickout, bin-moved and terminal events this population already listens to.
		populations: []WaitPopulation{PopAcquiring, PopGateStaged},
		// FREQUENCY NOTE, kept because a soak reads this row's count. The shuffle
		// pool is narrowed by the burial exclusion, which used to read the expose
		// bridge's table and now reads CLAIMS (SlotsBlockedByHardClaims). The
		// releaser is unchanged either way; what changed is which bins are
		// protected, so a shifted count here is expected rather than alarming.
		what: "any order anywhere in the group releases a slot",
	},
	{
		cause:       CauseDigBlockerClaimed,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the order holding the blocker finishes carrying it out of the lane",
	},
	{
		cause: CauseEpisodeAlreadyDigging,
		// PopAcquiring only. Nothing is committed when this fires: the guard is
		// asked before the plan and before the parent order, so the demand is a
		// parked row and no lane, leg or bin is held on its behalf.
		populations: []WaitPopulation{PopAcquiring},
		// THE RELEASER IS OUR OWN DIG, WHICH IS THE POINT. This is the only cause
		// in the table whose releaser belongs to the same demand that is waiting,
		// and it is worth reading twice: the wait exists because the plant was
		// ALREADY working on this, and the alternative to waiting was a second
		// excavation racing the first for the same parking (digs 2 and 8 on the
		// lane-stress rig 2026-08-13, mutual hold, neither able to finish).
		//
		// The dig's terminal event re-drives every waiter through the ordinary
		// path, and if the demand's bin is reachable by then no second dig is
		// raised at all — which is the saving, not just the safety.
		what: "the excavation already running for this demand finishes — its terminal " +
			"event re-drives the scanner, which re-resolves this demand against a lane " +
			"that is now open, or raises the next dig if one is still needed",
	},
	{
		cause: CauseDigHoldsParking,
		// TWO POPULATIONS, AND THEY SIT ON OPPOSITE SIDES OF THE COMMIT.
		//
		// PopAcquiring is right of way doing its job: a dig that could not count a
		// dig-free pool and therefore DID NOT START — no lane taken, no leg
		// dispatched, no bin claimed, a row parked with nothing committed. That is
		// the population the construction exists to create, and it is the cheap one.
		//
		// PopGateStaged is the residual the outbound dwell leaves: a leg already
		// holding a blocker, re-asking for a destination against a pool that has
		// narrowed since its dig planned. Expensive — a robot is standing still —
		// and deliberately not hidden under the cheap one, because the two answer
		// different questions in a soak. A rising PopAcquiring count is right of way
		// working; a rising PopGateStaged count is the residual firing, which is the
		// measurement that takes C3 out of the drawer.
		populations: []WaitPopulation{PopAcquiring, PopGateStaged},
		// FLOOR COVERAGE, both arms, checked rather than assumed (law 8). The parked
		// proposer is re-asked by the planning scan that raised it, on the same clock
		// as every other CauseNoShuffleSlot waiter. The dweller's WaitLane is the DUG
		// lane — a real lane — so gateStagedForLane sees it and SweepLaneWaiters (60s)
		// floors it, exactly as it does for that cause's dwelling population.
		what: "the dig holding the parking lane releases it — at its last blocker's exit (flip 2, or " +
			"for a service dig when the bin it uncovered is collected) or at " +
			"its teardown (unlockLaneForCompound); both evaluate the lane they free, and the group " +
			"evaluate wakes dwellers whose pool that lane is in",
	},

	// ── The gate's own failures ───────────────────────────────────────────
	{
		cause:       CauseGateRebindUnavailable,
		populations: []WaitPopulation{PopGateStaged},
		what:        "a slot in the lane frees, so the dweller's bin has somewhere to re-bind to",
	},
	{
		cause:       CauseGatePickupElsewhere,
		populations: []WaitPopulation{PopGateStaged},
		what: "the bin returns to this lane, or the release re-binds the pickup against where it " +
			"actually sits (rebindGatedPickup) — the wait is about the PLANT, not about a failed read",
	},
	{
		cause:       CauseStationWait,
		populations: []WaitPopulation{PopStationWait},
		what: "the STATION releases it — the line clears, the tooling finishes, an operator presses " +
			"Release. Core does not advance this one and must not: the precondition is a fact only " +
			"the station can observe, so a floor here would send a robot into a working cell. A row " +
			"standing under this cause for a long time is a DWELL to surface to a human (item 7), " +
			"not a wait to re-drive; Core's hard release is the escape hatch until that exists",
	},
	{
		cause:       CauseGateReleaseFailed,
		populations: []WaitPopulation{PopGateStaged},
		what: "whatever the release tripped on clears — most often a slot in the lane frees so the " +
			"re-bind has somewhere to land; the order stayed a candidate and the next pass retries",
	},
	{
		cause:       CauseGateAppendFailed,
		populations: []WaitPopulation{PopGateStaged},
		what:        "the fleet accepts the tail append on a later pass — a robot-system condition, not a lane one",
	},

	// ── Undetermined: a read failed, so the answer is not known ───────────
	{
		cause:       CauseLaneAcquireError,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the mouth read succeeds — Core declining to answer, not a busy lane",
	},
	{
		cause:       CauseLaneEntryError,
		populations: []WaitPopulation{PopNone},
		what:        "",
		finding: "DECLARED, NEVER SET. No production site writes this cause — verified by grep across " +
			"dispatch/ and fulfillment/; the only references are its own declaration and the value test " +
			"that pins its string. Nothing can wait under it, so it is not a liveness hole; it is dead " +
			"vocabulary that makes the cause surface look wider than it is. Left in place because " +
			"queue_cause_pure_test.go pins the value and deleting it is a decision for the owner, not " +
			"for the batch that noticed.",
	},
	{
		cause:       CauseAdmissionError,
		populations: []WaitPopulation{PopGateStaged, PopCompoundLeg},
		what:        "the read Core needed succeeds — an undetermined answer, not a busy lane",
	},
	{
		cause:       CauseReadFailed,
		populations: []WaitPopulation{PopAcquiring, PopCompoundLeg},
		what:        "the database answers again",
	},
	{
		cause:       CauseHeldBinMissing,
		populations: []WaitPopulation{PopAcquiring},
		finding: "NO EVENT RELEASES THIS, and that is correct rather than a gap: the order reached " +
			"the held-bin path with no bin_id, which the routing into that path makes impossible, so " +
			"it is a construction bug and not a wait. Nothing in the plant will change it. A floor " +
			"release under this cause means the order moved for some other reason; a row SITTING " +
			"under it means somebody needs to look at how that order was built. It carries a cause " +
			"at all only because the scanner re-enters this arm every tick, and a blank row that " +
			"repeats forever is the one thing the cause vocabulary exists to prevent.",
	},
	{
		cause:       CauseLoaderSourceUnreadable,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the loader pool becomes readable — says nothing about whether material is there",
	},

	// ── The fleet refused the create ──────────────────────────────────────
	{
		cause:       CauseFleetRefusedCreate,
		populations: []WaitPopulation{PopAcquiring, PopCompoundLeg},
		what:        "NOTHING — no event exists; the floor is the only thing that re-asks",
		finding: "ABSENCE-CLASS, AND THE FLOOR IS THE ANSWER. \"The fleet became willing\" is not an " +
			"event: no Core subscription fires when a saturated or disconnected RDS starts accepting " +
			"creates again, and inventing one would mean polling the vendor to manufacture a signal " +
			"whose only consumer is this wait. So this row's honest releaser is the periodic pass, and " +
			"the doctrine's (a) is genuinely unsatisfiable here rather than merely unbuilt. " +
			"CONSEQUENCE FOR THE TRIPWIRE: floor releases under this cause are EXPECTED and are not " +
			"emitter gaps. They are the one cause that must be read as a fleet-health signal rather " +
			"than as a missing subscription, which is why the histogram is grouped by cause.",
	},

	// ── Sourcing and reservation contention (fulfillment/) ────────────────
	{
		cause:       CauseDestNodeUnresolved,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the delivery node resolves — a node-graph fact, not an inventory one",
	},
	{
		cause:       CauseStoreSlotContended,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the contended destination slot frees, or a sibling slot opens",
	},
	{
		cause:       CauseClaimFailed,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the bin frees, or the finder picks a different one on the next scan",
	},

	// ── Complex-order preflight ───────────────────────────────────────────
	{
		cause:       CauseNGRPResolve,
		populations: []WaitPopulation{PopAcquiring},
		what:        "a child of the node group frees",
	},
	{
		cause:       CauseReserveHolding,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the rest of the reserve set becomes available",
	},
	{
		cause:       CauseComplexSlotReserve,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the remaining destination slots free",
	},
	{
		cause:       CauseDropoffCapacity,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the storage dropoff frees, or its committed inbound traffic lands",
	},
	{
		cause:       CauseSwapHold,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the sibling swap leg claims its bin, clearing the gate",
	},

	// ── The finder's tiers ────────────────────────────────────────────────
	//
	// THESE TWELVE WERE FOUND BY THE SOAK, NOT BY THE CODE AUDIT, and that is
	// worth recording where the next person will read it. They reach the row
	// through SourceOutcome.QueueCause and CapacityDetail.Cause, both of which
	// were bare `string` fields — so no site ever wrote QueueCause("…"), the type
	// never demanded a name, and the literal-conversion guard was structurally
	// unable to see them. The audit found 12 of 24. The observed-vs-declared
	// check found the rest on its first run against a live rig, which is the
	// argument for having an instrument that reads the PLANT rather than the
	// source.
	//
	// They share a releaser and differ only in the SCOPE that came up empty,
	// which is why they are separate tags and one sentence.
	{
		cause:       CauseFinderNodeEmpty,
		populations: []WaitPopulation{PopAcquiring},
		what:        "material of the wanted shape arrives at the named source node",
	},
	{
		cause:       CauseFinderGroupEmpty,
		populations: []WaitPopulation{PopAcquiring},
		what:        "material arrives at any child of the source group",
	},
	{
		cause:       CauseFinderPoolEmpty,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the dedicated loader's pool is replenished",
	},
	{
		cause:       CauseFinderPlantEmpty,
		populations: []WaitPopulation{PopAcquiring},
		what:        "material of this payload appears anywhere in the plant",
	},
	{
		cause:       CauseFinderNoFullCarrier,
		populations: []WaitPopulation{PopAcquiring},
		what:        "a carrier is filled — carriers exist, none of them is full",
	},
	{
		cause:       CauseFinderNoEmptyOfType,
		populations: []WaitPopulation{PopAcquiring},
		what:        "an empty of the DECLARED TYPE appears; it waits rather than taking another",
	},
	{
		cause:       CauseFinderAccessibilityUnreadable,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the reachability read succeeds — the finder declining to answer, not an empty plant",
	},

	// ── Dropoff capacity ──────────────────────────────────────────────────
	{
		cause:       CauseDropoffOccupied,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the bin at the destination leaves",
	},
	{
		cause:       CauseDropoffInflight,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the order already inbound to that destination places its bin",
	},
	{
		cause:       CauseNGRPFull,
		populations: []WaitPopulation{PopAcquiring},
		what:        "a child of the destination group frees",
	},
	{
		cause:       CauseCapacityCheckFailed,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the capacity read succeeds — undetermined, not full",
	},

	// ── Intake ────────────────────────────────────────────────────────────
	{
		cause:       CauseIntakeResolve,
		populations: []WaitPopulation{PopAcquiring},
		what:        "the step's node group resolves on a later scan",
	},
}

// DeclaredWaitPopulation is the exported view of one mechanism row, for the
// cross-package subscription check.
//
// It exists because the claim and its proof cannot live in the same package:
// dispatch declares which events release which population and cannot verify one
// of them, since engine owns the subscriptions and the import only runs one way.
// So the table is exported READ-ONLY — a shape engine's test can walk, with no
// setter and no way to reach the slice itself.
type DeclaredWaitPopulation struct {
	Population string
	Redriver   string
	Events     []string
	Floor      string
	// Owner lets the subscription cross-check skip the station-owned population
	// honestly rather than by name. Its releaser is a WIRE message the station
	// sends, handled in the messaging layer — there is no eventbus subscription
	// to find, and looking for one would fail forever.
	Owner string
}

// DeclaredWaitPopulations returns the mechanism table for engine's
// TestDeclaredReleaserEventsAreSubscribed. Copies, so a caller cannot edit the
// declaration it is checking.
func DeclaredWaitPopulations() []DeclaredWaitPopulation {
	out := make([]DeclaredWaitPopulation, 0, len(waitPopulations))
	for _, p := range waitPopulations {
		out = append(out, DeclaredWaitPopulation{
			Population: string(p.population),
			Redriver:   p.redriver,
			Events:     append([]string(nil), p.events...),
			Floor:      p.floor,
			Owner:      string(p.owner),
		})
	}
	return out
}

// DeclaredQueueCauses returns every cause the inventory covers.
//
// Exported for the soak's OBSERVED-VS-DECLARED cross-check: group the plant's
// own rows by queue_cause and subtract this set, and what is left is a hold
// class nobody designed a way out of. TestEveryQueueCauseHasAReleaser is what
// makes the subtraction meaningful — it guarantees these keys are exactly the
// declared constants, so "observed but not here" cannot mean "we forgot to add
// it to a second list".
func DeclaredQueueCauses() []string {
	out := make([]string, 0, len(causeReleasers))
	for _, r := range causeReleasers {
		out = append(out, string(r.cause))
	}
	return out
}

// FloorReleaseAction is the recovery_actions verb the liveness floor writes,
// exported so the soak can group by it.
const FloorReleaseAction = floorReleaseAction

// releaserFor returns the row for a cause, and whether one exists. The floor
// uses it to print `what` on its recovery record; a cause with no row prints a
// blunt fallback rather than a wrong sentence, and the totality test is what
// keeps that fallback unreachable.
func releaserFor(c QueueCause) (causeReleaser, bool) {
	for _, r := range causeReleasers {
		if r.cause == c {
			return r, true
		}
	}
	return causeReleaser{}, false
}
