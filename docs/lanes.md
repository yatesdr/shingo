# Lanes — mouths, digs, admission and liveness

Verified against: 51489c78

> As of 2026-11 no deployed plant configures LANE nodes; the mechanism is exercised by the sim plants.

**Source of truth:** `shingo-core/dispatch/` (`admission.go`, `lane_gate*.go`,
`lane_entry.go`, `lane_gate_release.go`, `lane_floor.go`, `chapter_floor.go`,
`dig_*.go`, `queue_cause.go`, `queue_releasers.go`, and `lane_lock.go`:
`LaneLock`, `TryLockFor`, `DigOwner`), `shingo-core/store/reservations/`
(`mouth.go`: `DigAdmissible`; `dig_exclusion.go`), and
`shingo-core/store/internal/helpers/lane_reachability.go`. This document is the
human-readable rendering; if they diverge, the code wins.

Related: [reservations.md](reservations.md), [storage-protections.md](storage-protections.md),
[queue-releasers.md](queue-releasers.md), [order-lifecycle.md](order-lifecycle.md),
[terminology.md](terminology.md).

---

## What a lane is, and why it needs any of this

A lane is a linear run of storage slots. Only the mouth is reachable — a bin at
depth 3 cannot be picked until the bins at depths 1 and 2 are moved. That single
physical fact generates every mechanism on this page:

- Two orders wanting the same lane at the same time can physically collide, so
  something must **arbitrate** the lane (the mouth reservation).
- Getting at a buried bin means moving other bins first, so there must be a way
  to **excavate** (the dig).
- An excavation is several orders that only make sense together, so there must be
  a way to **group and supersede** them (the chapter).
- Any of those can wait on something that never arrives, so every wait must
  **declare what ends it** (the releaser inventory and the floors).

## Reachability — one definition

> A slot is REACHABLE iff no occupied slot sits strictly shallower in the same
> lane.

Everything that asks it routes through `LaneBlockerPredicate`:

| Use | Call |
|---|---|
| SQL where the `ORDER BY` is part of the semantics | `ReachableSQL` / `BuriedSQL` |
| Go call sites | `nodes.BlockersInFrontOf` |

If you need to ask this question, use one of those. Another spelling is a bug
whether or not it happens to agree today.

## Storage Logic

### Contiguous Packing

Bins in a lane are packed from the back. Occupied slots form a continuous block from the deepest position forward. No empty gaps exist between bins. New bins are placed in the deepest available empty slot.

```
Empty lane:    [ ][ ][ ][ ][ ]    Store first bin ->  [ ][ ][ ][ ][A]
                                  Store second bin -> [ ][ ][ ][B][A]
                                  Store third bin ->  [ ][ ][C][B][A]
```

Bin A is the oldest (first in, deepest position). FIFO retrieval takes A first.

### Store Order Resolution

When a store order targets a node group, the group's `store_algorithm` chooses the lane or flat child
(`binresolver.GroupResolver.ResolveStore`; the codes are in [node-naming-standards.md](node-naming-standards.md)):

- **LKND** (the default) prefers a candidate already holding the same payload (consolidation), then the deeper
  candidate, then — only where the group enables resolve-around — a lane whose mouth is compatible now, then the
  emptiest.
- **DPTH** packs back to front regardless of payload, lanes before direct children.

Either way a lane or slot is skipped when its payload or bin type restrictions (`node_payloads`, `node_bin_types`)
refuse the incoming bin, and inside the chosen lane the slot is the deepest empty one.

### Retrieve — Direct Access

When a station requests a bin of a specific payload, the group's `retrieve_algorithm` picks it:

- **FIFO** (the default) takes the globally oldest bin by `loaded_at`; if that bin is buried, it reshuffles to reach
  it.
- **COST** takes the oldest *accessible* bin, and reshuffles only when none is accessible.
- **FAVL** takes the first available unclaimed bin at an accessible depth and never reshuffles.

A bin at the front of its lane (no blocking bins) is retrieved directly and delivered to the requesting station.

### Retrieve — Reshuffle

If the chosen bin is blocked:

1. **Unbury**: park each blocking bin in another slot in the group, shallowest first
2. **Retrieve**: pick up the target bin and deliver it

There is no third step. **Blockers are never restocked** — they lie where the
unbury parked them, and a parked blocker is ordinary findable inventory where it
sits. Parking searches every child slot of the group (`findShuffleSlots`) and is
deepest-first, which is what keeps the lane packed without a restock.

```
Before:   [C][B][A]    Target: A (oldest)

Unbury:   Park C, then B, into the group's free slots
Pick:     Retrieve A -> deliver to station

After:    [ ][ ][ ]    B and C stay where they were parked
```

### Reshuffle Failure

If a leg fails mid-reshuffle the compound stalls with a leg still open. The
stalled-chapter watchdog (`Dispatcher.SweepStalledChapters`, 60s) is what notices
and acts — it classifies the stall and either re-plans the chapter or records the
residue. Recovery is not a manual lane-unlock: the hold is a reservation row keyed
to the dig, and it clears on the same rule as any other dig.

### Flagged Bins in Lanes

Flagged bins occupy slots and block like any other bin, and stores can occur in
front of them. They are never sourced: sourcing reads the status allow-list
(`SourceableStatusSQL`: `available` and `staged` only), so `flagged`,
`maintenance` and `retired` are refused. A dig that has to move one parks it like
any other blocker.

### Sizing a group for headroom

**Size it for headroom, not just for capacity.** A group needs at least one full
lane's worth of slots free, because that is where a dig puts its blockers. A group
filled to the brim cannot conduct an excavation at all — the blockers have nowhere
to stand — and every dig admitted against it is a dig that will park.

The deepest single dig needs one free slot per blocker in front of its target (up
to the deepest lane's depth less one). That sizes one dig; it does not give the
group headroom, and the two are different numbers. A group sized only to it can
still be packed tight enough that digs contend with each other for somewhere to
put blockers.

The related rule for the bins themselves: a partially filled lane should be packed
from the back, empties contiguous at the mouth. A hole behind a bin is a slot no
robot can reach and no dig can create room in.

## The mouth — who may work this lane

A **mouth reservation** is the right to work a lane. One row per (lane, order),
`resource_kind='mouth'`, `node_id` = the lane, carrying a `mode` — the only kind
that uses the `mode` column:

| Mode | Meaning |
|---|---|
| `inbound` | the owner drops into the lane (its in-lane blocks are dropoffs) |
| `outbound` | the owner picks from the lane (its in-lane blocks are pickups) |
| `dig` | exclusive — the owner does both directions at once |

**The admission rule** (`admitMouth`): a `dig` excludes every other owner and is
excluded by every other owner. Any other pair is admitted only on an **exact
same-mode share**. So two inbound orders can work a lane together; an inbound and
an outbound cannot; a dig shares with nobody.

Two exemptions cut into that rule, both narrow:

- **The dig's beneficiary.** A hold taken on behalf of another order
  (`AcquireLanesFor`) ignores the beneficiary's own rows — a dig raised to rescue a
  gate-staged dweller is not refused by the dweller's own hold.
- **The mark.** An ordinary (non-dig) holder standing at one of its node group's
  wait points does not refuse an incoming dig. The row stays and keeps its other
  job ("still coming"); it stops turning away the robot that needs to work the
  corridor. Membership is group-scoped (`StagedOutside`, `dig_exclusion.go`): the
  wait points are a shared staging area in front of all the group's lanes, so
  staged at any of them is standing outside every lane in that group. The arm keys
  on where the robot is standing, never on whether the dig row is an excavation or
  a source lock.

An order holds **one mode per lane** (one row). Holding the lane in a different
mode is a refusal — except the sanctioned upgrade: an order digging the lane for
itself while holding it in a weaker mode has its row upgraded in place, because a
second row would put one owner on one lane twice. A foreign conflict is the
stronger refusal and wins regardless of which row is read first, which is why the
verdict is decided after the scan rather than inside it: deciding inside would make
the answer depend on the id ordering of unrelated orders.

The rows own the hold. It is durable state in Postgres, so it survives a Core
restart, and it is visible to every goroutine — it is not a lock in process
memory.

### `mode='dig'` does not mean "an excavation is running"

Every demand's source hold is a `dig`: a demand owns the lane it resolved onto
until the bin leaves by its mover. So a `dig` row says the lane is held
exclusively, not that an excavation is under way.

This is a **reporting** trap, not a mechanism one. A wait that says an excavation
is running when a plain retrieve is sourcing sends the next engineer looking for
a dig nobody planned. The readers that name an excavation (the lane-hold cause
classifier, and admission's refusal) read the kind off `reserved_by`, which every
writer stamps (`reservations.IsExcavation`).

### Concurrency: an advisory lock

Bins and slots get exactly-one-winner from a partial unique index. The mouth has
no such index. Its correctness comes from a **transaction-scoped advisory lock on
the lane id** (`pg_advisory_xact_lock`), taken before the lane's rows are read and
held for the acquire transaction. The durable state is the rows; the lock is only
the serializer and never outlives the transaction.

A multi-lane acquire takes its lanes in **ascending lane-id order**
(`sortedUniqueLanes`). That ordering is the deadlock-free lock order — the
mouth's equivalent of [reservations.md](reservations.md)'s slots-before-bins rule,
not a variant of it.

### Occupancy is a separate fact

An `occupancy` row says a robot is physically inside the lane right now, which is
not the same as holding the claim on the work. It is an idempotent insert keyed
on `(order_id, node_id)`: it de-dupes one order's repeat takes and says nothing
about a different order on the same node. **This row is the corridor mutex — it
arbitrates, it does not advise.** `dispatch.admit` reads `OccupantsOf` and
refuses a leg while a different order is recorded inside (`CauseLaneOccupied`),
and `TakeLaneOccupancy` returns the write's error rather than logging it — a take
that failed silently would declare an occupied corridor empty to the next entrant.

The SQL itself is permissive (no unique index on occupancy rows): at most one
inside rests on the caller's read plus the write succeeding.

## Admission vs. ordering — the distinction to get right

These look alike in a `queue_cause` histogram and are not alike at all.

| | Question | Answered by |
|---|---|---|
| **Admission** | *May this move happen at all?* | `admission.go` |
| **Ordering** | *It could — but should somebody else go first?* | `classifyLaneEntry` (`lane_entry.go`) |

The line: **admission says the lane cannot take this move; ordering says it
could, but somebody else should go first.**

- `lane-target-buried` is **admission** — the bin physically cannot be reached.
- `lane-deeper-pending` is **ordering** — the move is perfectly safe, and doing
  it now would wall a deeper target.

The `QueueCause` constants are grouped by that split in `queue_cause.go` for
exactly this reason. If you are reading a histogram, this is the distinction that
tells you whether you have a physical problem or a scheduling one.

Admission answers lane safety only: is the lane claimable by this order, is a
robot already inside it, is the slot this order wants actually reachable. It
deliberately does **not** own the mouth acquire — `admitMouth` stays in
`store/reservations`, because its correctness is a property of running inside the
acquire transaction under the advisory lock, not a property of the function.

### The entry tiers

`classifyLaneEntry` walks the other orders on the lane:

| Tier | Condition | Result |
|---|---|---|
| 1 | same-origin partner | co-dispatch, never gate |
| 2 | a deeper cross-origin store is still pending | `CauseLaneDeeperPending` |
| 3 | an active cross-origin group holds the lane | `CauseLaneGroupActive` |

An order whose origin cannot be classified is treated as its own origin — so it
gets depth-gated rather than wrongly co-dispatched. Conservative on purpose.

## Digs

A **dig** is an excavation: move the blockers out of the way so a buried bin can
be reached.

- **The dig row is the admission claim; the occupancy row is the corridor
  mutex.** There is no separate lock table — the `mode='dig'` mouth row is the
  exclusive admission claim (one dig per lane, one mode per lane). But what
  serialises the lane physically — at most one bin inside — is the occupancy
  row, taken at the append. The mouth row says who may enter; the occupancy
  row is the mutex.
- **A dig dwells in the lane it is digging**, and **yields to the dig already
  there**. Two digs do not fight over one lane.
- **Every lane takes a dig row**, a depth-1 lane included.
- **A capacity group refuses what it cannot feed — at plan time, not at
  admission.** There is no admission ledger counting digs per group. When a group
  has nowhere to put blockers, planning refuses the reshuffle as transient
  congestion (`ErrNoShuffleSlot`) and retries later.
- **Blockers are never restocked.** They lie where the unbury parked them and
  become ordinary findable inventory. See [Retrieve — Reshuffle](#retrieve--reshuffle) above.

### When the dig's lane claim drops

**Not** when the compound terminates. It drops when the **last blocker leaves the
lane**, and the corridor is then handed to the order collecting the uncovered bin
(tagged `ByDigHandoff`) rather than released into open contention.

The two obvious alternatives are both wrong:

- **Release at completion** re-buries the target. The slots the dig has just
  emptied are the cheapest parking candidates in the group, so the traffic the
  excavation was run to get ahead of fills them straight back in.
- **Hold until the target is collected** makes a finished order that never
  terminates — a third non-terminal state, invisible to every stall checker,
  holding a corridor on behalf of a demand it has no way to ask about.

Two nuances in the handoff: the last blocker leaving can trigger a
*self-handoff* — the lane handed to the dig's own order, no change of hands
(`handOffDugLane` gate 2) — and a holder that already collected releases
outright (gate 3). The holder counts as part of the population it would
otherwise be blocked by (`dig_lock_release.go`), so a lone dig never deadlocks
on its own shadow.

### The standoff tripwire

Dig admission is supposed to make a mutual hold unreachable. `SweepMutualDigHolds`
looks for digs waiting on each other in a closed loop that cannot self-clear, and
**reports** rather than acts — a human rules the incident. It is silent at zero,
which is its normal state, so any output means a set of loaded robots is holding
itself still, and each occurrence is a defect in the usable-capacity claim rather
than a routine event.

## The gate — where an order waits

A lane is **gated** if and only if it has a point for its robots to dwell at — its
**mark**. `laneWaitPoints` resolves it: the lane's own `lane_gate_point`
(`PropLaneGatePoint`), a legacy key that wins when set, else the group's
`group_wait_points` (`PropGroupWaitPoints`). The store refuses a lane whose group
has no waiting spot (`ErrLaneUnmarked`, `shingo-core/store/nodes/marks.go`), and
`gateTargetForLane` reports one it finds as a breach. One fact, set by the person
who knows the aisle, and the thing they set *is* the thing that makes it true.

The mark buys two things:

- **The wait** — where a robot stands while Core decides whether it may enter.
- **The oracle** — the dropoff slot is chosen at release (`releaseStoreCandidate`,
  through `plan.Planner.ChooseDrop`: across the whole group for a group-stamped
  drop, in its own lane otherwise), so it fits the lane as it is when the robot
  arrives. When a gated dropoff is refused or stale, release re-resolves it — to
  the lane's mark first, then to a sibling lane in the same group. A slot bound at
  dispatch instead goes stale on the drive: the robot arrives at a lane that has
  changed, and the outcomes are `dropoff-occupied` and the air bubble.

Collision safety does not live on the mark. The physical questions (is a foreign
dig holding this lane, is a robot inside it, is the target reachable) are asked on
every lane-entry path unconditionally, and occupancy rows are written
unconditionally. Enablement is per lane and incremental: placing a mark gates a
lane, clearing it is the rollback, and robots already dwelling complete under the
old rules. There is no global switch, and gate release has no timer.

## Chapters

A **chapter** is a generation of a compound. When a plan is superseded, the old
generation is a **closed chapter** — one seam creates, and `orders.open_for_children`
makes sealedness explicit rather than inferred.

A dig's parent is the demand that caused it, an order that existed before the
excavation was thought of; there is no synthetic parent order minted to own an
excavation (a "folder"). `TestFence_NoFolderCanEverBeMinted` holds that, checked
against live source with no exceptions clause, because a rule with a carve-out is
a rule whose carve-out grows.

### The stalled-chapter watchdog

A demand wearing `reshuffling` has two ways to stall, and each has its floor:
`AdvanceStuckReshuffleParents` sweeps the half where every child is terminal, and
`SweepStalledChapters` (60s, on `laneLivenessFloorInterval`) the half where a
**leg is still open**. Unlike the lane floor, it **resolves rather than points** —
dissolve-and-re-plan is the default disposition wherever re-planning is safe. It
asks one question of a chapter that has stopped — can it be safely re-planned now?
— with three answers: dissolved and re-queued, waiting on a committed vehicle, or
unresolvable residue. Only the last owes a human anything.

## Liveness — every wait declares what ends it

The doctrine: **every machine-owned wait has (a) a named event releaser, (b) a
periodic floor that re-evaluates it, and (c) a record when the floor is what
freed it.**

It is a table rather than prose because prose cannot be asserted: a comment can
say "a dropped event costs only latency until the next firing" while nothing
checks that a next firing can exist. So the connection is data, in
`queue_releasers.go`, enforced by three totality tests:

| Test | Asserts |
|---|---|
| `TestEveryQueueCauseHasAReleaser` | `causeReleasers` is total over the `QueueCause` constants |
| `TestEveryWaitPopulationHasBothPaths` | every population carries both an event set and a floor |
| `TestDeclaredReleaserEventsAreSubscribed` | the named events are really subscribed to that population's re-driver |

**A cause whose row cannot be written truthfully carries a `finding` instead of a
plausible sentence.** Those findings are the honest record of what is broken or
deliberately absent, and they are worth reading before trusting a histogram. The
generated tables are [queue-releasers.md](queue-releasers.md)'s.

The floor records the cause an order was carrying when the floor — rather than an
event — freed it. So the histogram of floor releases grouped by cause is a
**ranked worklist of missing emitters**, which is the artifact an emitter hunt
runs on. See [sweeps-and-monitors.md](sweeps-and-monitors.md) for the passes and
their order.

---

## Where the mechanism is exercised

The lane storage of the sim plants is the `[corridor]` section of
`plants/master.yaml`, and the properties this page states (the mouth gate, the dig
walk, no air bubbles, two digs in one group) are asserted on it by
`shingo-core/cmd/seeddev` `TestLanesFamily_*`. On a running stack, `soakstat`
(`shingo-core/cmd/soakstat`) reads the lane measures and exits non-zero on a broken
invariant, and `scripts/soak-watch.sh` samples it into a journal. See
[dev-env/sim.md](dev-env/sim.md).
