# Lanes — mouths, digs, admission and liveness

Verified against: b9210bce

**Source of truth:** `shingo-core/dispatch/` (`admission.go`, `lane_gate*.go`,
`lane_entry.go`, `lane_gate_release.go`, `lane_floor.go`, `chapter_floor.go`,
`dig_*.go`, `queue_cause.go`, `queue_releasers.go`),
`shingo-core/dispatch/binresolver/lane_lock.go` (`TryLockFor`, `DigOwner`,
`DigAdmissible`), `shingo-core/store/reservations/`
(`mouth.go`, `dig_exclusion.go`), and
`shingo-core/store/internal/helpers/lane_reachability.go`. This document is the
human-readable rendering; if they diverge, the code wins.

Related: `[[reservations]]`, `[[storage-protections]]`,
`[[queue-releasers]]`, `[[order-lifecycle]]`, `[[terminology]]`.

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

That sentence used to be spelled seven different ways across the tree — a Go
loop, a `COUNT`, four correlated sub-queries, and a sort key — and the spellings
did not agree. Everything now routes through `LaneBlockerPredicate`:

| Use | Call |
|---|---|
| SQL where the `ORDER BY` is part of the semantics | `ReachableSQL` / `BuriedSQL` |
| Go call sites | `nodes.BlockersInFrontOf` |

If you need to ask this question, use one of those. An eighth spelling is a bug
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

When a store order targets a supermarket:

1. Find all lanes with available space
2. Prefer lanes already holding the same payload (consolidation)
3. Among those, prefer the lane with the deepest target slot (most packed)
4. If no lane holds that payload, select any lane with space
5. Place the bin in the deepest accessible empty slot
6. Skip lanes that restrict bin types incompatible with the incoming bin

### Retrieve — Direct Access

When a station requests a bin of a specific payload:

1. Locate the oldest bin of that payload across all lanes (by `loaded_at` timestamp)
2. If the bin is at the front of its lane (no blocking bins), retrieve directly
3. Deliver to the requesting station

### Retrieve — Reshuffle

If the oldest bin is blocked:

1. **Unbury**: park each blocking bin in another slot in the group, shallowest first
2. **Retrieve**: pick up the target bin and deliver it

There is no third step. **Blockers are never restocked** — they lie where the
unbury parked them, and a parked blocker is ordinary findable inventory where it
sits. Parking is deepest-first, which is what keeps the lane packed without a
restock.

```
Before:   [C][B][A]    Target: A (oldest)

Unbury:   Park C, then B, into the group's free slots
Pick:     Retrieve A -> deliver to station

After:    [ ][ ][ ]    B and C stay where they were parked
```

The old restore-blockers subsystem did move them back. It was retired (migration
`v70` dropped `pending_restocks`) because it left permanent air bubbles whenever
it was off — empty slots buried behind bins, reachable by nothing.

### Reshuffle Failure

If a leg fails mid-reshuffle the compound stalls with a leg still open. The
stalled-chapter watchdog (`Dispatcher.SweepStalledChapters`, 60s) is what notices
and acts — it classifies the stall and either re-plans the chapter or records the
residue. Recovery is not a manual lane-unlock: the hold is a reservation row keyed
to the dig, and it clears on the same rule as any other dig.

### Flagged Bins in Lanes

Flagged bins occupy slots and block like any other bin, and stores can occur in
front of them. They are excluded from sourcing and from inventory counts by
status (`status NOT IN ('flagged','maintenance','retired')`).

A dig that has to move one parks it like any other blocker. There is no special
depth-1 placement for maintenance access — that behaviour belonged to the restock
step and went with it.

### Sizing a group for headroom

**Size it for headroom, not just for capacity.** A group needs at least one full
lane's worth of slots free, because that is where a dig puts its blockers. A group
filled to the brim cannot conduct an excavation at all — the blockers have nowhere
to stand — and every dig admitted against it is a dig that will park.

The shuffle-row minimum ([Shuffle Row](terminology.md#shuffle-row)) sizes the
*deepest single dig*. It does not give the group headroom, and the two are
different numbers. A group sized only to the shuffle-row minimum can still be
packed tight enough that digs contend with each other for somewhere to put
blockers.

The related rule for the bins themselves: a partially filled lane should be packed
from the back, empties contiguous at the mouth. A hole behind a bin is a slot no
robot can reach and no dig can create room in.

## The mouth — who may work this lane

A **mouth reservation** is the right to work a lane. One row per (lane, order),
`resource_kind='mouth'`, `node_id` = the lane, carrying a `mode`:

| Mode | Meaning |
|---|---|
| `inbound` | the owner drops into the lane |
| `outbound` | the owner picks from the lane |
| `dig` | exclusive — the owner does both directions at once |

**The admission rule** (`admitMouth`): a `dig` excludes every other owner and is
excluded by every other owner. Any other pair is admitted only on an **exact
same-mode share**. So two inbound orders can work a lane together; an inbound and
an outbound cannot; a dig shares with nobody.

Two exemptions pass the gate: a holder re-acquiring a lane it already holds
(the beneficiary), and — for a dig-mode acquire only — a holder staged at the
lane group's wait points (the mark's waiting room). See [reservations.md](reservations.md)
for the full statement of both.

An order holds **one mode per lane**. Holding the lane in a different mode is a
refusal — except the sanctioned upgrade: an order digging the lane for itself
while holding it in a weaker mode has its row upgraded in place. But a foreign
conflict is the stronger refusal and wins regardless of
which row is read first, which is why the verdict is decided after the scan
rather than inside the loop. Deciding inside made the answer depend on the id
ordering of unrelated orders.

The rows own the hold. It is durable state in Postgres, so it survives a Core
restart, and it is visible to every goroutine — it is not a lock in process
memory.

### `mode='dig'` no longer means "an excavation is running"

It did, until every demand's source hold was generalized from `outbound` to
`dig`: a demand owns the lane it resolved onto until the bin leaves by its mover.
Exclusivity did not change. What changed is that a `dig` row stopped implying an
excavation, and the readers were not told.

This is a **reporting** trap, not a mechanism one. A wait that says an excavation
is running when a plain retrieve is sourcing sends the next engineer looking for
a dig nobody planned — the same family as an alarm with the wrong name on it. The
readers that name an excavation (the lane-hold cause classifier, and admission's
refusal) read the kind off `reserved_by`, which every writer already stamps.

### Concurrency: an advisory lock

Bins and slots get exactly-one-winner from a partial unique index. The mouth has
no such index. Its correctness comes from a **transaction-scoped advisory lock on
the lane id** (`pg_advisory_xact_lock`), taken before the lane's rows are read and
held for the acquire transaction. The durable state is the rows; the lock is only
the serializer and never outlives the transaction.

A multi-lane acquire takes its lanes in **ascending lane-id order**
(`sortedUniqueLanes`). That ordering is the deadlock-free lock order — the
mouth's equivalent of `[[reservations]]`'s slots-before-bins rule, not a variant
of it.

### Occupancy is a separate fact

An `occupancy` row says a robot is physically inside the lane right now, which is
not the same as holding the claim on the work. It is an idempotent insert keyed
on `(order_id, node_id)`: it de-dupes one order's repeat takes and says nothing
about a different order on the same node. **This row is the corridor mutex — it
arbitrates, it does not advise.** `dispatch.admit` reads `OccupantsOf` and
refuses a leg while a different order is recorded inside, and
`TakeLaneOccupancy` returns the write's error rather than logging it.

Making the write itself the arbiter *in SQL* — a partial unique index on
`(node_id) WHERE resource_kind='occupancy'` — is a live option, not a settled
no; at-most-one-inside currently rests on the caller's read plus the write
succeeding. Do not read the idempotent-insert shape as a decision against it.

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
exactly this reason. If you are reading a histogram at a plant, this is the
distinction that tells you whether you have a physical problem or a scheduling
one.

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
- **Every lane takes a dig row.** The old depth-1 exemption is gone — a dig
  against a depth-1 lane holds the lane the same as any other.
- **A capacity group refuses what it cannot feed — at plan time, not at
  admission.** There is no admission ledger counting digs per group (§R.79
  deleted it). When a group has nowhere to put blockers, planning refuses the
  reshuffle as transient congestion (`ErrNoShuffleSlot`) and retries later.
- **Blockers are never restocked.** They lie where the unbury parked them and
  become ordinary findable inventory. See [Retrieve — Reshuffle](#retrieve--reshuffle) above.

### When the dig's lane claim drops

**Not** when the compound terminates. It drops when the **last blocker leaves the
lane**, and the corridor is then handed to the order collecting the uncovered bin
(tagged `ByDigHandoff`) rather than released into open contention.

Both obvious alternatives were tried, and the measurements are why they are not
the rule:

- **Release at completion** re-buried the target. The slots the dig had just
  emptied were the cheapest parking candidates in the group, so the traffic the
  excavation was run to get ahead of filled them straight back in.
- **Hold until the target is collected** produced a finished order that never
  terminates — a third non-terminal state, invisible to every stall checker,
  holding a corridor on behalf of a demand it has no way to ask about. On the
  lane-stress rig (2026-08-13) five of them held five lanes, and no live order
  wanted any of the five slots they were holding for.

Two §R.101 nuances in the handoff: the last blocker leaving can trigger a
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

A lane is **gated** if and only if its group has a point for its robots to dwell
at. Wait points live on the node **group** (`group_wait_points`); the per-lane
`lane_gate_point` (`PropLaneGatePoint`) is a legacy key that still wins when
set. No plant spec carries one — both real plants are group-configured. One
fact, set by the person who knows the aisle, and the thing they set *is* the
thing that makes it true.

The mark chooses the waiting room **and the oracle**: when a gated dropoff is
refused or stale, release re-resolves it — to the lane's mark first, then to a
sibling lane in the same group. Collision safety does not live on it — the
physical questions (is a foreign dig holding this lane, is a robot inside it, is
the target reachable) are asked on every lane-entry path unconditionally, and
occupancy rows are written unconditionally.

> **No lane-level marks exist at either plant today** (group-level wait points
> are in use). Every lane therefore parks its orders pre-dispatch. A group goes
> gated the day a human places a mark at one of its wait points; rollback is
> clearing it, and robots already dwelling complete under the old rules.

Enablement is per **group** and incremental by design. There is no global
switch, and gate release has no timer.

## Chapters

A **chapter** is a generation of a compound. When a plan is superseded, the old
generation is a **closed chapter** — one seam creates, and `orders.open_for_children`
(v84) makes sealedness explicit rather than inferred.

The synthetic parent that used to wear `reshuffling` on a demand's behalf — the
"folder" — is **deleted**. A dig's parent is the demand that caused it, an order
that already existed before the excavation was thought of, so there was nothing
left for a folder to be. A five-symbol ban fence stands where it was, checked
against live source only, with **no exceptions clause**: the previous version of
that rule had one, was endorsed 5/5 by two review rounds, and was still wrong,
because a rule with a carve-out is a rule whose carve-out grows.

### The stalled-chapter watchdog

Making the demand itself wear `reshuffling` created a gap:
`AdvanceStuckReshuffleParents` sweeps the half where every child is terminal, and
the half where a **leg is still open** had no floor at all. A machine-owned wait
population with no periodic floor is a liveness hole, and this one was found by a
reviewer reading a run rather than by anything in the tree.

`SweepStalledChapters` (60s) closes it, and unlike the lane floor it **resolves
rather than points** — dissolve-and-re-plan is the default disposition wherever
re-planning is safe. It asks one question of a chapter that has stopped — can it
be safely re-planned now? — with three answers: dissolved and re-queued, waiting
on a committed vehicle, or unresolvable residue. Only the last owes a human
anything.

## Liveness — every wait declares what ends it

The doctrine: **every machine-owned wait has (a) a named event releaser, (b) a
periodic floor that re-evaluates it, and (c) a record when the floor is what
freed it.**

It is a table rather than prose because prose cannot be asserted. The defect this
closes was not a missing idea — the evaluator's own doc said "a dropped event
costs only latency until the next firing." It was that nothing checked whether a
next firing could exist. Every individual comment was defensible; nothing
connected them.

So the connection is data, in `queue_releasers.go`, enforced by three totality
tests:

| Test | Asserts |
|---|---|
| `TestEveryQueueCauseHasAReleaser` | `causeReleasers` is total over the `QueueCause` constants |
| `TestEveryWaitPopulationHasBothPaths` | every population carries both an event set and a floor |
| `TestDeclaredReleaserEventsAreSubscribed` | the named events are really subscribed to that population's re-driver |

**A cause whose row cannot be written truthfully carries a `finding` instead of a
plausible sentence.** Those findings are the honest record of what is broken or
deliberately absent, and they are worth reading before trusting a histogram.

The floor records the cause an order was carrying when the floor — rather than an
event — freed it. So the histogram of floor releases grouped by cause is a
**ranked worklist of missing emitters**, which is the artifact an emitter hunt
runs on. See `[[sweeps-and-monitors]]` for the three passes and their order.

---

## Where the rig evidence lives

Measurements quoted above come from the lane-stress rig, not from a plant: its
frozen seed and its packed re-cut, driven with `soakstat` and
`scripts/soak-watch.sh` (both in git history). Its storage is now the `[corridor]`
section of `plants/master.yaml`, slot for slot, and the properties those runs
certified (the mouth gate, the dig walk, no air bubbles, two digs in one group) are
asserted on it by `shingo-core/cmd/seeddev` `TestLanesFamily_*`. See `[[dev-env/sim]]`.

One rig caveat worth carrying: its clock ran at two speeds until 2026-08-16, and
every measurement taken before that fix was against the wrong timeline.
