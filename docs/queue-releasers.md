# Queue releasers

Verified against: fe06474a

Why an order is waiting, and what will end it. A queued order is one instance
of a **machine-owned wait**, and every such wait has to declare how it ends.
This page is that declaration: the doctrine, then the two tables Core keeps it
in, generated from `shingo-core/dispatch/queue_releasers.go`. The order statuses
around the wait are in [order-lifecycle.md](order-lifecycle.md); the floor's
place among Core's loops is in [sweeps-and-monitors.md](sweeps-and-monitors.md).

## The doctrine

Every machine-owned wait has:

1. a named **event releaser** — the thing that, when it happens, ends this wait;
2. a periodic **floor** that re-evaluates it, for when that event does not fire;
3. a **record** when the floor is what freed it, rather than the event.

It is a table rather than prose because prose cannot be asserted. A comment can
say "a dropped event costs only latency until the next firing", and every such
comment can be defensible on its own; only a table can be checked for whether a
next firing exists at all.

## Where it lives

`shingo-core/dispatch/queue_cause.go` holds the `QueueCause` constants — the
named reason an order is waiting, written on its row (`orders.queue_cause`). This
is **Core vocabulary and never crosses the wire** (the station hears the
`protocol.QueueCode` and the sentence), which is what makes the values safe to
rename in bulk but not safe to re-spell casually: they are written on rows in a
plant's orders table and are grouped by in forensic queries.

`shingo-core/dispatch/queue_releasers.go` holds two tables:

- `causeReleasers` — for each cause, the populations an order carrying it can be
  sitting in, and `what` should end the wait.
- `waitPopulations` — for each population, its owner, its re-driver, the events
  it is released by, and its floor.

Three totality tests keep them honest:

| Test | Asserts |
|---|---|
| `TestEveryQueueCauseHasAReleaser` | `causeReleasers` is total over the `QueueCause` constants |
| `TestEveryWaitPopulationHasBothPaths` | every population carries both an event set and a floor |
| `TestDeclaredReleaserEventsAreSubscribed` | the declared events are really subscribed to that population's re-driver |

## The floor-release histogram is a worklist

The floor records the cause an order was carrying when the **floor** — rather
than an event — freed it (the lane liveness floor writes a `lane_floor_release`
recovery action, `FloorReleaseAction`). `what` is the sentence that record prints: it says what
*should* have ended the wait, so the record reads as "this event did not fire"
rather than "something was slow."

So the histogram of floor releases grouped by cause is a **ranked worklist of
missing emitters**. That is the artifact an emitter hunt runs on, and it is the
main reason to look at this data at all.

## Honest entries only

A cause whose row cannot be written truthfully carries a `finding` instead of a
plausible sentence. They are the Finding column of the cause table below, and
they are not all defects.

Read the findings before trusting a histogram. Two of them say "this wait has no
event and that is fine"; a reader who assumes every cause has an event releaser
would draw the wrong conclusion from either.

## Wait populations

Each population, who owns it, the re-driver its events and its floor both call,
and the floor itself — or, for a station-owned population, why it has none.

<!-- generated: shingo-core/dispatch/queue_releasers.go waitPopulations -->

| Population | What waits there | Owner | Re-driver | Released by | Floor |
|---|---|---|---|---|---|
| `PopAcquiring` (`acquiring`) | PopAcquiring is the waiting orders (`queued`): pre-dispatch, no robot committed. | core | `fulfillment.Scanner.RunOnce` — RunOnce executes a single scan pass. | `EventBinUpdated`, `EventOrderCompleted`, `EventOrderCancelled`, `EventOrderFailed`, `EventOrderSkipped`, `EventBlockCompleted`, `EventBinEnteredTransit` | `fulfillment.Scanner.StartPeriodicSweep` (60s) |
| `PopGateStaged` (`gate-staged`) | PopGateStaged is an order waiting at a lane wait (waitingAtLane): a robot committed to it, holding an unsealed waybill only Core can append to. | core | `Dispatcher.EvaluateGroupReleases` — EvaluateGroupReleases runs one release pass over a node group's waiters: every gate-staged order waiting at one of the group's lanes (a waiter is keyed by its group, not its lane). | `EventBlockCompleted`, `EventBinEnteredTransit`, `EventBinUpdated`, `EventOrderCompleted`, `EventOrderCancelled`, `EventOrderFailed`, `EventOrderSkipped` | `Dispatcher.SweepLaneWaiters` (60s) |
| `PopCompoundLeg` (`compound-leg`) | PopCompoundLeg is a dig leg Core has not yet handed to the fleet (orders.AwaitingFleetSQL). | core | `Dispatcher.RedriveHeldCompoundLegs` — RedriveHeldCompoundLegs re-drives every compound parent holding a PENDING leg in this lane. | `EventBlockCompleted`, `EventBinEnteredTransit`, `EventBinUpdated`, `EventOrderCompleted`, `EventOrderCancelled`, `EventOrderFailed`, `EventOrderSkipped` | `Dispatcher.SweepLaneWaiters` (60s) |
| `PopCompoundParent` (`compound-parent`) | PopCompoundParent is a compound parent sitting in `reshuffling` while its children run. | core | `Dispatcher.AdvanceCompoundOrder` — AdvanceCompoundOrder dispatches the next pending child order in a compound sequence. | `EventOrderCompleted`, `EventOrderCancelled`, `EventOrderFailed`, `EventOrderSkipped` | `ReconciliationService.AdvanceStuckReshuffleParents` (all children terminal, reconcile interval) + `Dispatcher.SweepStalledChapters` (a leg still open, 60s) |
| `PopVacateHeld` (`vacate-held`) | PopVacateHeld is an order parked at a wait Core releases on the vacate fact (coreOwnsWait): a station wait whose release Core took and holds on a stamp until a named bin leaves a named node, or a stored vacate wait on a plan Core authors (a robot is committed), or a home clear's partial move Core holds before dispatch on the same fact (clear_home.go, clearDropHeld: no robot is sent until the home's carrier is lifted). | core | `Dispatcher.RedriveVacateOnLift` — RedriveVacateOnLift is EventBinEnteredTransit's re-drive: binID was lifted by orderID's pickup. | `EventBinEnteredTransit`, `EventOrderCompleted`, `EventOrderCancelled`, `EventOrderFailed`, `EventOrderSkipped` | `Dispatcher.SweepLaneWaiters` (60s) |
| `PopStationWait` (`station-wait`) | PopStationWait is an order `staged` at a wait the STATION owns — the swap choreography's own gates, WaitKindStation. | station | `Dispatcher.HandleOrderRelease` (station-initiated) — HandleOrderRelease processes a release request for a staged (dwelling) order. | `protocol.TypeOrderRelease` (from the station) | none: the precondition is a fact only the station can observe — the line has cleared, the tooling is done, the operator is ready. |

<!-- /generated -->

## Causes

Each cause, the value written on the order row (`queue_cause`), the populations
an order carrying it can sit in, what ends the wait (`what`, the sentence the
floor's record prints), and the finding's first sentence where the row has one.

<!-- generated: shingo-core/dispatch/queue_releasers.go causeReleasers -->

| Cause | Value | Waits in | What ends it | Finding |
|---|---|---|---|---|
| `CauseLaneDeeperPending` | `lane-deeper-pending` | `acquiring`, `gate-staged` | the deeper cross-origin store PLACES its bin, or (never dispatched) gives up its slot row there: a re-made choice that moved, or a park (wakeLanesOfSlots) |  |
| `CauseLaneGroupActive` | `lane-group-active` | `acquiring`, `gate-staged` | the active cross-origin group finishes with the lane |  |
| `CauseLaneDigActive` | `lane-dig-active` | `acquiring`, `gate-staged`, `compound-leg` | the dig holding this lane releases it — at its last blocker's exit (flip 2, or for a service dig when the bin it uncovered is collected) or at its teardown; both re-evaluate the lane they free |  |
| `CauseLaneTargetBuried` | `lane-target-buried` | `acquiring`, `gate-staged`, `compound-leg` | the bin in front is moved — by a dig, or by whoever claimed it carrying it out |  |
| `CauseLaneHeldDig` | `lane-held-dig` | `acquiring` | the dig holding the mouth releases it |  |
| `CauseLaneHeldSource` | `lane-held-source` | `acquiring`, `gate-staged`, `compound-leg` | the holding demand's robot carries its bin out of the lane, or that demand ends; either way its hold is dropped and the lane it frees is re-evaluated |  |
| `CauseLaneHeldTraffic` | `lane-held-traffic` | `acquiring` | the different-mode holder releases its mouth row (placement, pickout, or terminal) |  |
| `CauseLaneHeldUnreadable` | `lane-held-unreadable` | `acquiring` | the lane becomes readable — this is an absence of an answer, not a busy lane |  |
| `CauseLaneOccupied` | `lane-occupied` | `acquiring`, `gate-staged`, `compound-leg` | the robot inside the lane places or picks, releasing its occupancy row |  |
| `CauseLaneLocked` | `lane-locked` | `acquiring` | the lane's hold is dropped — by the excavation finishing, or by the demand holding its mouth releasing it | THE HOLD IS NOT ALWAYS AN EXCAVATION. |
| `CauseBinLockRace` | `lock-race` | `acquiring` | immediate — the winner of the bin race proceeds and this order re-plans on the next scan |  |
| `CauseIntakeBuried` | `intake-buried` | `acquiring` | the dig planned for this burial completes and the parent re-resolves |  |
| `CauseReshuffleCongestion` | `reshuffle-congestion` | `acquiring` | whatever the more specific arms would have named — this is the unmapped fallback |  |
| `CauseDigLegTurn` | `dig-leg-turn` | `compound-leg` | the dig's earlier leg ends and the chapter advances to this one |  |
| `CauseNoShuffleSlot` | `no-shuffle-slot` | `acquiring`, `gate-staged` | any order anywhere in the group releases a slot |  |
| `CauseDigBlockerClaimed` | `dig-blocker-claimed` | `acquiring`, `gate-staged` | the order holding the blocker finishes carrying it out of the lane — or, if that order has already ended, the reconciliation sweep releases the claim it left behind |  |
| `CauseDigBlockerPromised` | `dig-blocker-promised` | `acquiring`, `gate-staged` | the demand holding the promise takes its bin (pickup → \_TRANSIT, arrival clears the hold) or ends — and, when it instead re-sources away from the bin, the periodic floor, because a holder abandoning a promise emits nothing anyone listens for |  |
| `CauseDigBlockerHeldAtStation` | `dig-blocker-held-at-station` | `acquiring` | the holder's station releases it, and its robot lifts the bin (pickup → \_TRANSIT, a bin event the scanner listens on); or the holder ends and its claim is released |  |
| `CauseSlotTaken` | `slot-taken` | `acquiring` | the holder's next scanner pass, on which it re-resolves its destination elsewhere in the group; the periodic floor if that pass is missed |  |
| `CauseBinTaken` | `bin-taken` | `acquiring` | the scan pass the bin-move door runs after the taking move's acquire, on which the un-pointed holder re-finds through the finder; the periodic floor if that pass is missed |  |
| `CauseDigBlockerStopped` | `dig-blocker-order-stopped` | `gate-staged` | an engineer resolves the stopped order named in the wait — it moves, it is cancelled, or its claim is released — and the next lane pass plans the dig it was blocking. Core does NOT resolve it automatically: an order stopped without terminating is a config fault or a breakdown, and dissolving it would be the machine guessing at something it cannot classify |  |
| `CauseDigBlockerWaitsOnThisDig` | `dig-blocker-waits-on-this-dig` | `gate-staged` | the deadlock is broken by hand: this dig's lane is released so the order holding the blocker can reach it, after which the blocker leaves and the dig re-plans. Do NOT go looking for a fault in the holding order — it is refused on THIS dig's own lane lock and is behaving correctly. Core does not break the cycle itself: automatic dissolve is refused for this family |  |
| `CauseStagedOwnDig` | `staged-own-dig` | `gate-staged` | the order's OWN dig children finish — the chapter closes and Core appends the tail to the robot standing at the mark (the splice-append; no queue round-trip, because the plan was never re-planned, only preceded) |  |
| `CauseStagedDigFailed` | `staged-dig-failed` | `gate-staged` | Core re-asks the lane — on the next lane event or the 60s lane floor — and either raises a replacement dig chapter (the lane is still walled) or appends the tail where the robot stands (the lane is clear and the resume is the splice-append) |  |
| `CauseChapterLegInFlight` | `chapter-leg-in-flight` | `compound-parent`, `gate-staged` | the open leg's mission terminates — the fleet reports it delivered, failed or cancelled — and the chapter's terminal arm returns the demand |  |
| `CauseEpisodeAlreadyDigging` | `episode-already-digging` | `acquiring` | the excavation already running for this demand finishes — its terminal event re-drives the scanner, which re-resolves this demand against a lane that is now open, or raises the next dig if one is still needed |  |
| `CauseDigHoldsParking` | `dig-holds-parking` | `acquiring`, `gate-staged` | the dig holding the parking lane releases it — at its last blocker's exit (flip 2, or for a service dig when the bin it uncovered is collected) or at its teardown; both re-evaluate the lane they free, and dwellers whose parking pool that lane is in are woken |  |
| `CauseDemandHoldsParking` | `demand-holds-parking` | `acquiring`, `gate-staged` | the demand holding the parking lane finishes sourcing from it — its robot carries the bin out, or the demand ends; either way its hold is dropped, the lane it frees is re-evaluated, and dwellers whose parking pool that lane is in are woken |  |
| `CauseGateRebindUnavailable` | `gate-rebind-unavailable` | `gate-staged` | a slot in the lane frees, so the dweller's bin has somewhere to re-bind to |  |
| `CauseGatePickupElsewhere` | `gate-pickup-elsewhere` | `gate-staged` | the bin returns to this lane, or the release re-binds the pickup against where it actually sits — the wait is about the PLANT, not about a failed read |  |
| `CauseVacateWaitingLift` | `vacate-waiting-lift` | `vacate-held` | the stamped bin is lifted off the node — the lifter's pickup block (EventBinEnteredTransit), read back from bins.last_lift_at — or the lifter ends moot (skipped, the death rule's moot), or the Core hard release |  |
| `CauseVacateLifterGone` | `vacate-lifter-gone` | `vacate-held` | for a swap pair, the death rule cancels this leg (HandleSwapPeerTerminal), or the Core hard release; for a home clear's partial move, the clear's re-run (Dispatcher.RerunClears: on an order's end and on the vacate floor's tick, keyed on the empty standing on the home held by nobody) chooses again — a pull takes the empty or a buffer spot does, and the move waits on that lift — or ends the job |  |
| `CauseStationWait` | `station-wait` | `station-wait` | the STATION releases it — the line clears, the tooling finishes, an operator presses Release. Core does not advance this one and must not: the precondition is a fact only the station can observe, so a floor here would send a robot into a working cell. A row standing under this cause for a long time is a DWELL to surface to a human, not a wait to re-drive; Core's hard release is the escape hatch |  |
| `CauseSwapPartnerFinished` | `swap-partner-finished` | `station-wait` | the STATION releases it, exactly as for station-wait — but unlike an ordinary station wait, nothing further is COMING: this leg's swap partner already ran its half and confirmed, so no second robot will arrive and no line will clear. A release the operator already pressed is kept on the row as the Edge's release intent and goes when the partner's end re-plans it; otherwise a person presses, and Core's hard release is the escape hatch. Core does not advance it and must not — a station wait is never floored, and a partner that finished does not change whose fact the precondition is. A row standing under THIS cause is a dwell to surface immediately rather than a wait to wonder about |  |
| `CauseGateReleaseFailed` | `gate-release-failed` | `gate-staged` | whatever the release tripped on clears — most often a slot in the lane frees so the re-bind has somewhere to land; the order stayed a candidate and the next pass retries |  |
| `CauseGateAppendFailed` | `gate-append-failed` | `gate-staged` | the fleet accepts the tail append on a later pass — a robot-system condition, not a lane one |  |
| `CauseLaneAcquireError` | `lane-acquire-error` | `acquiring` | the mouth read succeeds — Core declining to answer, not a busy lane |  |
| `CauseAdmissionError` | `admission-error` | `gate-staged`, `compound-leg` | the read Core needed succeeds — an undetermined answer, not a busy lane |  |
| `CauseReadFailed` | `read-failed` | `acquiring`, `compound-leg` | the database answers again |  |
| `CauseHeldBinMissing` | `held-bin-missing` | `acquiring` | — | NO EVENT RELEASES THIS, and that is correct rather than a gap: the order reached the held-bin path naming no bin, which the routing into that path makes impossible, so it is a construction bug and not a wait. |
| `CauseLoaderSourceUnreadable` | `loader-source-unreadable` | `acquiring` | the loader pool becomes readable — says nothing about whether material is there |  |
| `CauseNGRPAtLevel` | `ngrp-at-level` | `acquiring`, `compound-leg` | a carrier LEAVES the group — or somebody raises the level — or, with an overflow named, a slot frees there and the next pass sends the order to it (at admission or once parked). The second is the one an operator can act on | IT CLEARS ON TWO DIFFERENT THINGS, which is why it is not CauseNGRPFull. |
| `CauseGroupHoldsEmptiesOnly` | `group-holds-empties-only` | `acquiring`, `compound-leg` | somebody names an overflow destination for the group, or the carrier is aimed at a group that takes payloads. NOT a carrier leaving — the group emptying changes nothing here | THE GROUP FREEING UP IS NOT THE RELEASER, which is what separates this from ngrp-at-level and ngrp-full. |
| `CauseFinderGroupFenced` | `finder-group-fenced` | `acquiring` | NOTHING IN THE MATERIAL — the group is not this asker's. Somebody adds the process to the group's supports, or turns strict sourcing off, or the claim stops naming that group | A CONFIGURATION WAIT, NOT A MATERIAL ONE, and the only one in this family. |
| `CauseFinderSourceMissing` | `finder-source-missing` | `acquiring` | NOTHING IN THE MATERIAL — the named source node does not exist. Somebody re-creates it, or changes the loader's or claim's inbound source, or the order is cancelled | A CONFIGURATION WAIT, beside finder-group-fenced. |
| `CauseFinderSourceUnreadable` | `finder-source-unreadable` | `acquiring` | the source search becomes readable — says nothing about whether material is there, because the search never ran |  |
| `CauseFleetRefusedCreate` | `fleet-error` | `acquiring`, `compound-leg` | NOTHING — no event exists; the floor is the only thing that re-asks | ABSENCE-CLASS, AND THE FLOOR IS THE ANSWER. |
| `CauseDestNodeUnresolved` | `dest-node-unresolved` | `acquiring` | the delivery node resolves — a node-graph fact, not an inventory one |  |
| `CauseStoreSlotContended` | `slot-reserved` | `acquiring` | the contended destination slot frees, or a sibling slot opens |  |
| `CauseClaimFailed` | `claim-failed` | `acquiring` | the bin frees, or the finder picks a different one on the next scan |  |
| `CauseLoaderParkNoSlot` | `loader-park-no-slot` | `acquiring` | the loader's home or one of its buffer slots frees |  |
| `CauseNGRPResolve` | `ngrp-resolve` | `acquiring` | a child of the node group frees |  |
| `CauseReserveHolding` | `reserve-holding` | `acquiring` | the rest of the reserve set becomes available |  |
| `CauseHoldingForOutside` | `holding-for-outside` | `acquiring` | the carrier coming to the position lands, or the move naming the carrier there lifts it |  |
| `CauseComplexSlotReserve` | `complex-slot-reserve` | `acquiring` | the remaining destination slots free |  |
| `CauseContainmentCapacity` | `containment-capacity` | `acquiring` | a bin leaves the containment node — an earlier containment move completes, or a verified bin's release walks one out |  |
| `CauseDropoffCapacity` | `dropoff-capacity` | `acquiring` | the storage dropoff frees, or its committed inbound traffic lands — a pre-split row, so which of the two it was waiting for is not recoverable from the cause alone |  |
| `CauseSwapHold` | `swap-hold` | `acquiring` | the partner's order reaches Core — its intake's acquire asks this pair's leader and the two legs go together — or a partner digging its own bin out finishes and resumes through pending. If Core refuses the partner at intake instead, nothing can arrive and the wait ends in failure: this leg fails carrying the partner's reason, at the refusal or on its own first pass after it |  |
| `CauseFinderNodeEmpty` | `finder-node-empty` | `acquiring` | material of the wanted shape arrives at the named source node |  |
| `CauseFinderGroupEmpty` | `finder-group-empty` | `acquiring` | material arrives at any child of the source group |  |
| `CauseFinderPoolEmpty` | `finder-pool-empty` | `acquiring` | the dedicated loader's pool is replenished |  |
| `CauseFinderPlantEmpty` | `finder-plant-empty` | `acquiring` | material of this payload appears anywhere in the plant |  |
| `CauseFinderNoFullCarrier` | `finder-no-full-carrier` | `acquiring` | a carrier is filled — carriers exist, none of them is full |  |
| `CauseFinderNoEmptyOfType` | `finder-no-empty-of-type` | `acquiring` | an empty of the DECLARED TYPE appears; it waits rather than taking another |  |
| `CauseFinderCarrierRefused` | `finder-carrier-refused` | `acquiring` | a person acts on the carrier standing at the named node: moves it off, relabels or clears it, or adds its type to the part's carriers; or a carrier the part may ride is set down there beside it | A PERSON WAIT, NOT A MATERIAL ONE. |
| `CauseFinderNoToteCarrier` | `finder-no-tote-carrier` | `acquiring` | NOTHING IN THE MATERIAL — the part's carrier rule names no tote carrier. Somebody adds a tote carrier to the part's carriers, or the pull is cancelled | A CONFIGURATION WAIT, beside finder-source-missing. |
| `CauseFinderAccessibilityUnreadable` | `finder-accessibility-unreadable` | `acquiring` | the reachability read succeeds — the finder declining to answer, not an empty plant |  |
| `CauseDropoffOccupied` | `dropoff-occupied` | `acquiring` | the bin at the destination leaves |  |
| `CauseDropoffInflight` | `dropoff-inflight` | `acquiring` | the order already inbound to that destination places its bin |  |
| `CauseNGRPFull` | `ngrp-full` | `acquiring` | a child of the destination group frees, or a slot frees in the overflow it names |  |
| `CauseCapacityCheckFailed` | `capacity-check-failed` | `acquiring` | the capacity read succeeds — undetermined, not full |  |
| `CauseIntakeResolve` | `intake-resolve` | `acquiring` | the step's node group resolves on a later scan |  |

<!-- /generated -->
