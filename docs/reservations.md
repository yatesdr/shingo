# Bin & Slot Reservations

Verified against: b9210bce

**Source of truth:** `shingo-core/store/reservations/` (`reservations.go` for the
bin/slot lifecycle, `mouth.go` for lane holds and occupancy, `blocking.go` and
`dig_exclusion.go` for the shared predicates),
`shingo-core/dispatch/allocator.go`, and `shingo-core/fulfillment/scanner.go`.
This document is the human-readable rendering; if they diverge, the code wins.

Related: [order-builder-dispatch.md](order-builder-dispatch.md) (how an order is
issued and what it holds from birth), [storage-protections.md](storage-protections.md),
[bin-transit-state.md](bin-transit-state.md), [lanes.md](lanes.md) (the lane mouth
and occupancy).

---

## The one rule: soft until complete

Sourcing a bin and claiming it are separate moments: planning decides on a bin
(a read), dispatch writes `bins.claimed_by` (a write). A reservation makes the
decision a durable, inspectable, revocable hold, so the gap between the two
never leaves a bin decided-on but unowned, or claimed with nothing recording
who should release it.

An order does not get to hard-claim a bin or a slot until it is actually about
to send a robot. While it is still waiting — for material, for a free slot, for
the fleet — it holds its resources **softly**, as reservations. The hard claim
(`bins.claimed_by` / `nodes.claimed_by`) is written in one step, for both
resources together, immediately before the call that creates the fleet order.

Every transport family obeys this. **Simple orders** (a single retrieve /
retrieve-empty / move / store) have the fulfillment scanner soft-reserve the
destination slot, then soft-acquire the source bin, and confirm both at
dispatch. **Coordinated orders** (multi-step, edge-authored plans) have the
allocator soft-reserve every distinct source bin and destination slot the plan
needs, reconciling what it already holds each tick, and confirm the complete
set at dispatch.

The point is to never strand a hard claim on an order that isn't going
anywhere: a waiting order holds revocable reservations, it is visible to
operators as a queued reason, and the instant it dispatches its claims become
hard in one atomic step.

## Reserve, then claim, then confirm

A **reservation** is a soft, revocable row that records "order O intends to
use resource R," *before* any hard column is written. The lifecycle is:

```
Acquire  →  Confirm  →  Release
(soft)      (hard)       (gone)
```

- **Acquire** — write a `pending` reservation row. This is the race point: a
  partial unique index makes Acquire exactly-one-winner (two orders can't both
  Acquire the same bin). The hard column (`claimed_by`) is **not** touched.
  Other orders can see the hold — the finders and slot excluders skip reserved
  resources — but the hold is revocable, and reaping it never strands a
  physical claim.
- **Confirm** — flip the row `pending → confirmed` **and** write the hard
  column, in one transaction. The soft hold and the hard claim move together,
  atomically, so neither ever exists without the other. Confirm runs at
  dispatch, never earlier.
- **Release** — a hard `DELETE`. A reservation never reaches a terminal state;
  every row on disk is `pending` or `confirmed`, which is why the partial
  indexes' `state IN ('pending','confirmed')` predicate matches every row.

There is one backward edge: a **fleet refusal demotes**. `DemoteConfirmedByOrder`
flips the order's confirmed bin and slot rows back to `pending` — "armor off,
paper kept." The bin stays spoken for, the re-dispatch's confirm has the pending
row the seatbelt requires, and nothing else can take the resource without
outranking the order that still holds it. The row is demoted, never deleted, and
never leaves the `pending`/`confirmed` domain. Mouth rows have no pending phase
and occupancy is a measurement, so neither is ever demoted.

Both halves are **owner-idempotent**. An order re-confirming a resource it
already hard-claimed (a retry after a crash between the two writes, say) heals
in place rather than failing. That is what makes the split safe across scanner
ticks and Core restarts: the worst case is a half-state the next tick fixes,
never a permanently wedged resource.

### What a reservation is not

- **Not a lock held in process memory.** It's a Postgres row. It survives Core
  restarts and is visible to every goroutine.
- **Not a capacity count.** It's per-resource (`bin_id` or `node_id`), not an
  aggregate tally. The capacity gate ([storage-protections.md](storage-protections.md)
  tier 1) handles the aggregate question; reservations handle the per-resource
  ownership question.
- **Not time-boxed.** There is **no TTL** and **no age-based give-up** (see
  *Reaping* below). A waiting (`queued`) order legitimately holds its
  reservations for as long as it takes to find a source; demand is
  operator-driven and does not evaporate on a timer.

### Slots before bins

When an order needs both a destination slot and a source bin, it reserves and
confirms the **slot first, then the bin.** Ordering one resource class entirely
before the other is what prevents a slot↔bin cross-type deadlock cycle — two
orders each holding the slot the other needs while waiting on the bin the other
holds. The same ordering applies in every family.

## The four resource kinds

A reservation covers one of four resource kinds (`resource_kind` column):

| Kind | Identity | What it holds | Since |
|------|----------|---------------|-------|
| `bin`  | `bins.id`  | a source bin the order will pick up | v42 |
| `slot` | `nodes.id` | a destination storage/staging slot the order will deliver to | v44 |
| `mouth` | `nodes.id` (the lane) | the right to work a lane, in a declared direction | v69 |
| `occupancy` | `nodes.id` (the lane) | a robot is physically inside the lane right now | v76 |

**Bins and slots** are the soft-until-complete substrate this document is
about. Slots are the near-mirror of bins: a slot reservation is a soft,
revocable hold on a destination node, and `ConfirmSlotClaim` writes the slot
claim and flips the reservation in one transaction, exactly as a bin's confirm
does. The package keys both on a kind-agnostic `Ref` (`BinRef` / `SlotRef`) so
the Acquire/Confirm/Release primitives are shared, not forked per kind.

**Mouth and occupancy are a different mechanism that happens to share the table.**
They are not a third and fourth flavour of the reserve→confirm→release lifecycle
above: they have their own admission rule (`admitMouth`), their own concurrency
control (an advisory lock on the lane, not a unique index), and no confirm step.
What they share with bins and slots is the row, the `state` domain, and the
reaper. Their model — modes, the admission rule and its exemptions, the lock
order, occupancy as the corridor mutex — is [lanes.md](lanes.md).

## Where it happens in the code

**Simple path** (`shingo-core/fulfillment/scanner.go`, `dispatch/store_slot.go`):

1. The scanner finds a source bin (the shared `SourceFinder`). No status is
   written for this: acquiring is the act between `pending` and `dispatched`,
   not a stored status.
2. It resolves the destination node and holds it soft when it is one
   carrier's at a time — a storage slot (`storageDropoffOf`) or a loader's
   window (its home row) — through `ReserveStorageDropoff` (`holdPosition`).
   A line or consume destination holds nothing. Owner-idempotent: a window the
   order holds from its issue (`Need.Window`) passes straight through.
3. It soft-acquires the bin (`ReserveForDispatchAndPoint`: the pending
   reservation and the order's cart row in `order_bins`, in one statement).
4. At dispatch, the hand (`DispatchHand`) hard-claims the slot (a storage
   dropoff only: a window's row stays pending to the order's end) and the
   bin, in one transaction, then the fleet order is created.

If any step before confirm fails — the slot is contended, the bin loses a race,
the destination can't be resolved — the order parks `pending → queued` under
the cause that arm wrote (an order already `queued` stays there), **keeps its
soft reservations**, and the scanner retries it next tick. Because its cart row
was written with the soft-acquire, a re-entering order reuses its own bin
instead of shopping for a new one (see *Why the finder can't see a soft-held
bin* below).

**Coordinated path** (`shingo-core/dispatch/allocator.go`, `complex_dispatch.go`).
Complex (multi-step) orders don't claim their bins at dispatch time by
shopping — they:

1. **Reserve at plan time** — when the plan is built, `reserveComplexPlan` /
   `reserveComplexSlots` Acquire a pending row for each distinct source and
   destination the plan needs.
2. **Confirm at dispatch** — when the order actually ships to the fleet,
   `confirmComplexPlan` flips each row pending→confirmed and writes the claims.
   The scanner drives this via `DispatchPreparedComplex`.

Between reserve and confirm the order sits in `pending` or `queued`, holding its sources as
revocable reservations. This is what lets an **incomplete** order retry: on each
scanner tick the Allocator **reconciles** rather than re-shopping from scratch —

```
need = the plan's distinct sources
held = ListByOrder(orderID)                 # already secured
keep = held ∩ need  → skip Acquire          # don't fight our own holds
release = held \ need → Release             # a re-resolution moved a source
acquire = need \ held → Acquire each        # a conflict here = genuinely another order
complete iff acquire drains
```

The reconcile must load-held-first and skip-Acquire for bins the order already
holds, because the unique index is on `bin_id` alone — re-Acquiring a bin this
order already holds would read as a conflict and falsely report the order's own
bins as "missing" every tick.

The orders the scanner may ask are the acquiring set, `{pending, queued}`
(`AcquiringStatusSQLList`; [order-builder-dispatch.md](order-builder-dispatch.md)).

**Issued holding.** An order for one carrier, and an order bound to a window, take
their pending row in the issue's own transaction (`Need.Cart`, `Need.Window`);
who asked decides whether a stranger's pending hold is taken. The rules, door by
door, are [order-builder-dispatch.md](order-builder-dispatch.md) §The one issue.
A dig's leg is the one exception, below.

## The store API

`shingo-core/store/reservations/reservations.go`:

| Function | Effect |
|----------|--------|
| `Acquire(db, orderID, laneOwner, binID, reservedBy)` | pending bin reservation; exactly-one-winner via the partial unique index. Also runs the dig arm: a bin standing in a lane a FOREIGN dig holds returns `ErrLaneDugByAnother` instead of inserting — asked against the insert's own snapshot. `laneOwner` is the order that would hold a lane row on orderID's behalf (its compound parent, or orderID itself); callers with no compound context pass orderID twice |
| `AcquireAndPoint(db, orderID, laneOwner, binID, reservedBy)` | `Acquire` that also writes the order's cart row (`order_bins`, the implied plan's lift) in the same statement, only when the reservation was inserted |
| `AcquireSlot(db, orderID, nodeID, reservedBy)` | pending slot reservation |
| `Confirm(db, orderID, binID)` / `ConfirmSlot(...)` | `pending → confirmed`, paired with the hard-column write in the caller's tx |
| `Release` / `ReleaseSlot` / `ReleaseByOrder` / `ReleaseByBin` / `ReleaseByNode` | hard `DELETE` the row(s) |
| `ListByOrder(db, orderID)` | read an order's pending+confirmed holds (used by the reconcile — see above) |
| `ReapOrphaned(db)` | reclaim rows whose owning order is terminal or gone |
| `DemoteConfirmedByOrder(db, orderID)` | fleet refusal: flips every confirmed bin/slot row of the order back to pending — paper kept, armor off (see the lifecycle above) |

The package takes the `Execer`/`Queryer` it's handed (`*sql.DB`, `*sql.Tx`, or
a store interface) rather than threading a concrete DB — so Acquire/Confirm can
run *inside* a caller's transaction, which is what makes reserve+confirm atomic.

This table is the bin/slot substrate only. The package also carries, in
`mouth.go`, `blocking.go` and `dig_exclusion.go`: the mouth and occupancy
families (the `AcquireLanes`/`HandOffLaneToPicker`/`OccupantsOf` set), the
shared SQL predicate renderers (`BinSpokenForSQL`, `HeldByOwnerSQL`,
`OnTheBooksSQL`, `DigExclusionSQL` — one spelling each, kept honest by drift
tests), and the asker/exemption types (`DigAsker`, `StagedOutside`). `Acquire`
takes a `RowExecer` rather than the `Execer` below — it writes and reads back
in one statement, because the dig arm must be evaluated against the insert's
own snapshot.

## The claim seatbelt

`ClaimForDispatch` (in `service/bin_manifest.go`) is the **sanctioned path** to
set `bins.claimed_by` — see the one recorded exception below. Its CAS requires
the order's own pending reservation on the bin (`reservations.HeldByOwnerSQL`):
you cannot claim a bin you have not reserved. The inverse — clearing
`claimed_by` — is coupled to releasing the reservation in the same transaction
(`ReleaseClaimForBin`, and `TerminalizeOrder` for the whole of an order's
holds), so the hard column and the soft row can never drift apart.

A `forbidigo` rule in `.golangci.yml` rejects any direct `db.ClaimBin` call
outside `ClaimForDispatch` (and test fixtures), and a tombstone rule forbids
re-adding an unclaim that skips the reservation release. Slots need no lint
rule: `nodes.ClaimSlotTx` is the only slot-claim primitive, its CAS carries the
same seatbelt (`HeldByOwnerSQL` for the slot), and its one caller,
`ConfirmSlotClaimWithPartnerTx` (under `ConfirmSlotClaim` and its `Tx` forms),
flips the slot reservation in the same transaction.

Reserve and confirm share one transaction; the CAS seatbelt never weakens. A
claim and its confirm written as separate statements is the **atomicity wedge**
the code comments name: a crash between them leaves a hard claim with no
confirmed row behind it.

### The one recorded exception: a dig's legs

A compound reshuffle order is a parent that spawns legs to dig a buried bin out
of a lane. Its legs are issued on one transaction (dispatch `issueLegsTx`:
`store.OpenCompoundTx`, then each leg's row through the one issue, then
`store.HoldLegTx`), not found by the scanner, and the bin each leg moves is
assigned by the reshuffle plan. A leg is issued holding **no bin reservation**:
the bin it moves is its cart (its `order_bins` row, written with its INSERT),
and its parent's lane holds it — the dig's mouth row keeps every order outside
the compound out of the lane (`reservations.DigExclusionSQL`). One bin may be two
legs' (an unbury followed by a retrieve of the same bin) and the bin index
admits one active row, which is why. That cart with no pending reservation
behind it is the exception to "an order for one carrier holds it by a pending
row from its issue".

At the leg's issue the dig settles the bin's foreign soft holds:
`stealSoftHolds` ranks the two demands. An outranked *pending* promise is taken
(the loser's reservation and its cart row deleted in one statement, so it
re-finds; an order for that one cart keeps its row and is displaced out loud,
never quietly re-aimed); an outranking promise, or a hard claim outside the
compound, refuses the dig. Any refusal rolls the whole compound back. The leg's
parking is pencilled in the same transaction.

The leg's claim and ledger row are its hand's, at its dispatch
(`ClaimCompoundLegBinTx`): a **direct bin claim** (an `UPDATE bins SET
claimed_by` that bypasses `ClaimForDispatch`), sibling-scoped — it refuses a
bin held by an order outside this compound — with a **confirmed reservation
row** behind it in the same transaction (`supersedeBinLedger`: the bin's rows
deleted, one inserted `state='confirmed', reserved_by='compound-child'`), so a
hard claim never exists without a ledger row, even here. The last leg to claim
the bin holds its one active row.

A home clear's legs are not a dig: each holds its cart by a pending row from its
issue, as every order for one carrier does, and is no part of this exception.

### The invariant, and the fence that enforces it

**No order that is still acquiring (`pending` or `queued`) holds a hard bin or
slot claim.** (The sweep also covers `sourcing`, which only an old history row
can still say.) A dig's legs are no exception: a leg waiting its turn holds
nothing hard.

The fence is a SQL sweep run as a docker integration test
(`engine/rule1_invariant_docker_test.go`, `TestRule1_NoHardClaimWhileSourcing_Sweep`),
**not** the linter. The `forbidigo` rule above is useful but is *selector-match
only*: it cannot see a bare `UPDATE bins SET claimed_by` written inside a
transaction, and it cannot see a new caller that bypasses the reserve/confirm
pair. The invariant sweep can. It runs a real scanner pass that parks an order
in `queued` holding its bin softly (its hand refused at the slot and rolled
back), then asserts no acquiring order holds a hard claim, with a dig's legs
waiting their turn among them; separately it pins that a compound child is
written holding nothing (its claim is its hand's).

If you add a new path that writes `claimed_by`, the sweep is what will catch a
rule violation. Run the docker tests.

## Why the finder can't see a soft-held bin (and why that's fine)

The plain finders exclude any bin with a pending reservation —
`helpers.BinNotReservedSQL` (`NOT reservations.BinSpokenForSQL`) inside
`BinSourceableSQL`, and the `HasPendingReservation` flag in
`BinUnavailableReason`. This exclusion is *owner-blind*: it skips the bin even
if the reservation belongs to the very order doing the finding.

That is deliberate and correct for everyone else — two orders must not both
soft-hold the same bin. For the order's *own* held bin it would be a trap: if a
soft-holding order re-ran the finder it would not see its own bin and would shop
a second one (double-source). The defense is the keep arm: an order whose cart
row names its bin (`Order.OnlyBin`, written at soft-acquire) short-circuits
straight to the held-bin dispatch path (`dispatchHeldBin`) and never consults
the finder. Its own bin is reused; the owner-blind exclusion only ever affects
*other* orders' holds. The group resolver's scans are owner-aware instead
(`BinSourceableSQLFor` for a lane, `reservedByStranger` for a slot child): a
complex demand re-resolves its group pickup each pass, and must see the bin its
own dig reserved.

One exception: if the soft hold was reaped while the cart row survived, the
confirm seatbelt can never open and the retry has no releaser — so the held-bin
arm asks whether the order still holds the bin (`stillHoldsBin`) and, if not,
forgets the cart (`ForgetHeldCart` deletes its row) and lets the next tick
re-find. Safe precisely because with no hold there is nothing to double-source
over. A named order keeps the row for the cart it names — re-finding could only
fetch that cart — and takes its hold again instead (`reholdNamedCart`).

## Reaping — owner-liveness, not age

`ReapOrphaned` reclaims reservation rows whose **owning order is terminal or
gone** — never on age. An order in `queued` may legitimately hold its
reservations for minutes, hours, or days while waiting for a source; that is not
"stuck," it is working as designed. A hold becomes reclaimable only when its
order reaches a terminal status or no longer exists.

Consequently pre-dispatch orders (`pending`, `queued`) are **exempt** from the
stuck-order sweep — `AbandonStuckOrders` is restricted to the runtime states
(`dispatched`, `staged`; `protocol.IsStuckSweepCandidate`). There is no timer
that abandons a waiting order, and no age-based reaper: reserve-at-plan-time
makes the soft window long by design. Give-up is operator-driven.

The `expires_at` column (made nullable in v44) is written by no Acquire and read
by no reaper.

## The Allocator

`shingo-core/dispatch/allocator.go` — `type Allocator` — owns the plan-time
reserve/confirm/reconcile logic for **both** bins and slots. `Dispatcher`
constructs it and delegates the resource legs to it; the Dispatcher retains the
gates, re-resolution, lifecycle, fleet calls, and compound orchestration, so the
reserve/confirm model has one home.

## Schema

In `shingo-core/store/migrations.go`:

| Version | Change |
|---------|--------|
| v42 | `reservations` table |
| v43 | partial unique index `uq_reservations_bin_active` on `(bin_id)` — the exactly-one-winner gate |
| v44 | `resource_kind` + `node_id` target; rescoped bin index; per-kind slot index; `CHECK` on `state` and on exactly-one-of (`bin_id` xor `node_id`) |
| v69 | `mode` column + the `(resource_kind, node_id)` read index — the lane-mouth substrate |
| v76 | `resource_kind='occupancy'` admitted — the in-lane hold, separate from the claim |

v44 is what makes the slot substrate possible and pins the state column so a
typo can't silently escape the partial index.

Two partial unique indexes (`uq_reservations_bin_active`,
`uq_reservations_slot_active`) make Acquire exactly-one-winner per resource:
only one order at a time can hold a pending or confirmed reservation on a given
bin or slot. **There is no equivalent index for `mouth` or `occupancy`** — the
mouth is serialized by an advisory lock instead ([lanes.md](lanes.md)), and
occupancy's insert is idempotent per owner rather than exclusive.

The current constraints admit four kinds and three modes:

```sql
CHECK (resource_kind IN ('bin','slot','mouth','occupancy'))
CHECK (mode IS NULL OR mode IN ('inbound','outbound','dig'))
CHECK ((resource_kind = 'bin'   AND bin_id IS NOT NULL AND node_id IS NULL)
    OR (resource_kind IN ('slot','mouth','occupancy')
                                AND node_id IS NOT NULL AND bin_id IS NULL))
```
