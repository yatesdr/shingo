# Lanes — mouths, digs, admission and liveness

**Source of truth:** `shingo-core/dispatch/` (`admission.go`, `lane_gate*.go`,
`lane_entry.go`, `lane_floor.go`, `chapter_floor.go`, `dig_*.go`,
`queue_cause.go`, `queue_releasers.go`), `shingo-core/store/reservations/`
(`mouth.go`, `dig_exclusion.go`), and
`shingo-core/store/internal/helpers/lane_reachability.go`. This document is the
human-readable rendering; if they diverge, the code wins.

Related: `[[reservations]]`, `[[material-flow]]`, `[[storage-protections]]`,
`[[queued-order-fulfillment]]`, `[[terminology]]`.

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

An order holds **one mode per lane**. Holding the lane in a different mode is a
refusal — but a foreign conflict is the stronger refusal and wins regardless of
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
about a different order on the same node. Arbitration is the caller's read.

Making the write itself the arbiter would mean a partial unique index on
`(node_id) WHERE resource_kind='occupancy'`. That is a live option, not a settled
no — do not read the idempotent-insert shape as a decision against it.

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

- **The dig row is the lock.** There is no separate lock table — the `mode='dig'`
  mouth row *is* the exclusive hold.
- **A dig dwells in the lane it is digging**, and **yields to the dig already
  there**. Two digs do not fight over one lane.
- **Depth-1 lanes are exempt** from the dig rules — the exemption is about the
  lane a dig excavates, not about the demand.
- **A capacity group admits only the digs it can feed.** A group with nowhere to
  put blockers cannot excavate, so admitting a dig against it just parks it.
- **Blockers are never restocked.** They lie where the unbury parked them and
  become ordinary findable inventory. See `[[material-flow]]`.

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

### The standoff tripwire

Dig admission is supposed to make a mutual hold unreachable. `SweepMutualDigHolds`
looks for digs waiting on each other in a closed loop that cannot self-clear, and
**reports** rather than acts — a human rules the incident. It is silent at zero,
which is its normal state, so any output means a set of loaded robots is holding
itself still, and each occurrence is a defect in the usable-capacity claim rather
than a routine event.

## The gate — where an order waits

A lane is **gated** if and only if it has a point for its robots to dwell at
(`PropLaneGatePoint` on the LANE). One fact, set by the person who knows the
aisle, and the thing they set *is* the thing that makes it true.

The mark chooses **only the waiting room**: park before dispatch, or drive out
and dwell at a point. Collision safety does not live on it — the physical
questions (is a foreign dig holding this lane, is a robot inside it, is the
target reachable) are asked on every lane-entry path unconditionally, and
occupancy rows are written unconditionally.

> **No marks exist at either plant today.** Every lane therefore parks its orders
> pre-dispatch. A lane goes gated the day a human places its mark; rollback is
> clearing it, and robots already dwelling complete under the old rules.

Enablement is per-lane and incremental by design. There is no global switch, and
gate release has no timer.

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

Measurements quoted above come from the lane-stress rig, not from a plant:
`plants/lane-stress.yaml` and `plants/lane-stress-packed.yaml`, driven with
`soakstat` and `scripts/soak-watch.sh`. See `[[dev-env/sim]]`.

One rig caveat worth carrying: its clock ran at two speeds until 2026-08-16, and
every measurement taken before that fix was against the wrong timeline.
