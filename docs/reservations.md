# Bin & Slot Reservations

**Source of truth:** `shingo-core/store/reservations/reservations.go`,
`shingo-core/dispatch/allocator.go`, and `shingo-core/fulfillment/scanner.go`.
This document is the human-readable rendering; if they diverge, the code wins.

Related: `[[order-builder-dispatch]]`, `[[storage-protections]]`,
`[[bin-transit-state]]`, `[[reservation-decision-glossary]]`.

---

## Problem — the decide→do gap

Before reservations, sourcing a bin and claiming it were two separate steps
with a hole between them:

1. **Planning** resolved a source bin (a read — "bin X is eligible").
2. **Dispatch** then ran a CAS to set `bins.claimed_by = order.id` (a write).

Between those two steps the bin was resolved-but-unowned, so:

- **A crash or error** between resolve and claim left nothing recorded — the
  order had "decided" on a bin it never secured, and a concurrent order could
  grab it.
- **A crash after claim but before the order completed sourcing** left a bin
  *claimed but with no reservation backing it* — a "wedge". The claim was
  set, the bin was unavailable to everyone, but nothing knew *why* or *who
  should release it*.
- **Two orders** could both read the same bin as eligible and both attempt the
  CAS; only the DB's atomicity decided the winner, with no higher-level
  coordination over the whole multi-bin plan.

The capacity gate and `claimed_by` (see `[[bin-transit-state]]`) protected the
*destination*, but nothing protected the *source decision* as a durable,
inspectable, revocable hold.

## The one rule: soft until complete

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
anywhere. Before this discipline a simple order hard-claimed its bin the moment
it found one, *then* tried to reserve its slot — so an order that waited on its
slot left a bin locked up for as long as it waited. Other orders couldn't use
that bin, and if the waiting order later failed, the bin was stranded until
reconciliation noticed. Soft-until-complete makes the waiting order polite: it
holds revocable reservations, it is visible to operators as a queued reason,
and the instant it dispatches its claims become hard in one atomic step.

## Solution — reserve, then claim, then confirm

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
  column, in one transaction. This is what closes the wedge: the soft hold and
  the hard claim move together, atomically. Confirm runs at dispatch, never
  earlier.
- **Release** — a hard `DELETE`. A reservation never reaches a terminal state;
  every row on disk is `pending` or `confirmed`, which is why the partial
  indexes' `state IN ('pending','confirmed')` predicate matches every row.

Both halves are **owner-idempotent**. An order re-confirming a resource it
already hard-claimed (a retry after a crash between the two writes, say) heals
in place rather than failing. That is what makes the split safe across scanner
ticks and Core restarts: the worst case is a half-state the next tick fixes,
never a permanently wedged resource.

### What a reservation is not

- **Not a lock held in process memory.** It's a Postgres row. It survives Core
  restarts and is visible to every goroutine.
- **Not a capacity count.** It's per-resource (`bin_id` or `node_id`), not an
  aggregate tally. The capacity gate (`[[storage-protections]]` tier 1) handles
  the aggregate question; reservations handle the per-resource ownership
  question.
- **Not time-boxed.** There is **no TTL** and **no age-based give-up** (see
  *Reaping* below). A sourcing order legitimately holds its reservations for as
  long as it takes to find a source; demand is operator-driven and does not
  evaporate on a timer.

### Slots before bins

When an order needs both a destination slot and a source bin, it reserves and
confirms the **slot first, then the bin.** Ordering one resource class entirely
before the other is what prevents a slot↔bin cross-type deadlock cycle — two
orders each holding the slot the other needs while waiting on the bin the other
holds. The same ordering applies in every family.

## The two resource kinds — bins and slots

A reservation covers one of two resource kinds (`resource_kind` column,
migration v44):

| Kind | Identity | What it holds |
|------|----------|---------------|
| `bin`  | `bins.id`  | a source bin the order will pick up |
| `slot` | `nodes.id` | a destination storage/staging slot the order will deliver to |

Slots are the near-mirror of bins: a slot reservation is a soft, revocable hold
on a destination node, and `ConfirmSlotClaim` makes the slot claim
reservation-guarded — closing the split-brain where an incomplete order held its
**bins** as reservations but its **slots** as hard claims across ticks. The
package keys everything on a kind-agnostic `Ref` (`BinRef` / `SlotRef`) so the
Acquire/Confirm/Release primitives are shared, not forked per kind.

> `mouth` is a third kind the schema accepts but has no code path yet; it's a
> forward-looking placeholder, not an active mechanism.

## Where it happens in the code

**Simple path** (`shingo-core/fulfillment/scanner.go`, `dispatch/store_slot.go`):

1. The scanner finds a source bin (the shared `SourceFinder`) and moves the
   order to `sourcing`.
2. It resolves the destination node and soft-reserves the slot
   (`ReserveStorageDropoff` — a no-op for a line/consume destination, which has
   no slot to reserve).
3. It soft-acquires the bin (`ReserveForDispatch`, a pending reservation) and
   stamps the order's `BinID`.
4. At dispatch, `ConfirmForDispatch` hard-claims the slot (if a storage
   dropoff) and the bin, then `DispatchDirect` creates the fleet order.

If any step before confirm fails — the slot is contended, the bin loses a race,
the destination can't be resolved — the order parks in `sourcing` (never
`queued`), **keeps its soft reservations**, and the scanner retries it next
tick. Because `BinID` was stamped at soft-acquire, a re-entering order reuses
its own bin instead of shopping for a new one.

**Coordinated path** (`shingo-core/dispatch/allocator.go`, `complex_dispatch.go`).
Complex (multi-step) orders don't claim their bins at dispatch time by
shopping — they:

1. **Reserve at plan time** — when the plan is built, `reserveComplexPlan` /
   `reserveComplexSlots` Acquire a pending row for each distinct source and
   destination the plan needs.
2. **Confirm at dispatch** — when the order actually ships to the fleet,
   `confirmComplexPlan` flips each row pending→confirmed and writes the claims.
   The scanner drives this via `DispatchPreparedComplex`.

Between reserve and confirm the order sits in `sourcing`, holding its sources as
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

See `[[order-builder-dispatch]]` for how `MoveToSourcing` is positioned at the
start of the reserve attempt and how the `AcquiringStatusSQLList = {queued,
sourcing}` set widens which orders the scanner retries.

**Synchronous manual orders** (`engine.CreateDirectOrder`,
`www.submitSpotRetrieveSpecific`): the operator picks a specific bin, so these
soft-acquire it and confirm at dispatch in the same call. The soft window is
microscopic, but the discipline is uniform — no hard claim before the dispatch
step on any path.

## The store API

`shingo-core/store/reservations/reservations.go`:

| Function | Effect |
|----------|--------|
| `Acquire(db, orderID, binID, reservedBy)` | pending bin reservation; exactly-one-winner via the partial unique index |
| `AcquireSlot(db, orderID, nodeID, reservedBy)` | pending slot reservation |
| `Confirm(db, orderID, binID)` / `ConfirmSlot(...)` | `pending → confirmed`, paired with the hard-column write in the caller's tx |
| `Release` / `ReleaseSlot` / `ReleaseByOrder` / `ReleaseByBin` / `ReleaseByNode` | hard `DELETE` the row(s) |
| `ListByOrder(db, orderID)` | read an order's pending+confirmed holds (used by the reconcile — see above) |
| `ReapOrphaned(db)` | reclaim rows whose owning order is terminal or gone |

The package takes the `Execer`/`Queryer` it's handed (`*sql.DB`, `*sql.Tx`, or
a store interface) rather than threading a concrete DB — so Acquire/Confirm can
run *inside* a caller's transaction, which is what makes reserve+confirm atomic.

## The claim seatbelt

`ClaimForDispatch` (in `service/bin_manifest.go`) is the **sanctioned path** to
set `bins.claimed_by` — see the one recorded exception below. Its CAS keeps an
`EXISTS(pending reservation)` clause: you cannot claim a bin you have not
reserved. The inverse — clearing `claimed_by` — is coupled to releasing the
reservation in the same transaction (`ReleaseClaimByOrder` /
`ReleaseClaimByBin`), so the hard column and the soft row can never drift apart.

A `forbidigo` rule in `.golangci.yml` rejects any direct `db.ClaimBin` call
outside `ClaimForDispatch` (and test fixtures). An identical rule guards slots:
direct `nodes.ClaimSlot` is forbidden in favour of `db.ConfirmSlotClaim`.

This is what the code comments call the **atomicity wedge** fix (glossary:
`D45`): the original `ApplyComplexPlan` claimed a bin and confirmed its
reservation in separate statements, so a crash between them left the wedge.
Reserve+confirm share one transaction; the CAS seatbelt never weakens.

### The one recorded exception: compound reshuffle children

A compound reshuffle order is a parent that spawns child legs to dig a buried
bin out of a lane. Those children are created and sequenced by the compound
machinery in `CreateCompoundChildren` (`store/orders.go`), not by the scanner,
and the bin each child moves is assigned by the reshuffle plan — not found. At
creation, each child takes a **raw bin claim** (a direct
`UPDATE bins SET claimed_by`, with no reservation row) so the sequenced legs
don't race each other for the same bin.

This is the one place a hard claim exists on an order that has not reached its
own dispatch step. It is keyed on the data: a compound child is the only order
that carries a `parent_order_id`.

### The invariant, and the fence that enforces it

**No non-compound order that is still acquiring (`pending`, `queued`, or
`sourcing`) holds a hard bin or slot claim.** A compound child
(`parent_order_id IS NOT NULL`) is the exempted case above.

The fence is a SQL sweep run as a docker integration test
(`dispatch/rule1_invariant_test.go`), **not** the linter. The `forbidigo` rule
above is useful but is *selector-match only*: it cannot see a bare
`UPDATE bins SET claimed_by` written inside a transaction, and it cannot see a
new caller that bypasses the reserve/confirm pair. The invariant sweep can. It
populates a real database with an order parked in `sourcing`, asserts the sweep
finds zero hard claims on it, and separately asserts that a compound child's raw
claim is correctly exempted.

If you add a new path that writes `claimed_by`, the sweep is what will catch a
rule violation. Run the docker tests.

## Why the finder can't see a soft-held bin (and why that's fine)

The bin finders exclude any bin with a pending reservation — `NOT EXISTS (SELECT
1 FROM reservations ... state='pending')` in the SQL, and the
`HasPendingReservation` flag in `BinUnavailableReason`. This exclusion is
*owner-blind*: it skips the bin even if the reservation belongs to the very
order doing the finding.

That is deliberate and correct for everyone else — two orders must not both
soft-hold the same bin. For the order's *own* held bin it would be a trap: if a
soft-holding order re-ran the finder it would not see its own bin and would shop
a second one (double-source). The defense is the keep arm: an order that already
holds a bin (its `BinID` is set from soft-acquire) short-circuits straight to the
held-bin dispatch path and never consults the finder. Its own bin is reused; the
owner-blind exclusion only ever affects *other* orders' holds.

## Reaping — owner-liveness, not age

`ReapOrphaned` reclaims reservation rows whose **owning order is terminal or
gone** — never on age. An order in `sourcing` may legitimately hold its
reservations for minutes, hours, or days while waiting for a source; that is not
"stuck," it is working as designed. A hold becomes reclaimable only when its
order reaches a terminal status (`confirmed`/`failed`/`cancelled`) or no longer
exists.

Consequently pre-dispatch orders (`queued`, `sourcing`) are **exempt** from the
stuck-order sweep — `AbandonStuckOrders` is restricted to runtime states
(`dispatched`). There is no timer that abandons a sourcing order. (An older
age-based reaper was retired when reserve-at-plan-time made the soft window
long; do not reintroduce one.)

The `expires_at` column is still stamped at Acquire (NOT NULL) but is no longer
read by any reaper — vestigial pending a schema drop.

## The Allocator

`shingo-core/dispatch/allocator.go` — `type Allocator` — owns the plan-time
reserve/confirm/reconcile logic for **both** bins and slots. `Dispatcher`
constructs it and delegates the resource legs to it; the Dispatcher retains the
gates, re-resolution, lifecycle, fleet calls, and compound orchestration. The
extract is mechanical (no behaviour change) — it exists so the reserve/confirm
model has one home rather than being inlined into the complex-dispatch path.

## Schema

Migrations `v42`–`v44` in `shingo-core/store/migrations.go`:

| Version | Change |
|---------|--------|
| v42 | `reservations` table (originally dormant) |
| v43 | partial unique index `uq_reservations_bin_active` on `(bin_id)` — the exactly-one-winner gate |
| v44 | `resource_kind` (bin\|slot\|mouth) + `node_id` target; rescoped bin index; per-kind slot index; `CHECK` on `state` and on exactly-one-of (`bin_id` xor `node_id`) |

v44 is what makes the slot substrate possible and pins the state column so a
typo can't silently escape the partial index.

Two partial unique indexes (`uq_reservations_bin_active`,
`uq_reservations_slot_active`) make Acquire exactly-one-winner per resource:
only one order at a time can hold a pending or confirmed reservation on a given
bin or slot.

## What is explicitly out of scope

- **No time-based abandon / horizon / timer.** Give-up is operator-driven
  (demand never evaporates; a cancelled order is re-issued by Edge's in-flight
  guard).
- **No parent-walk seatbelt, no mouth behavior/index, no reshuffle building.**
  These are gated on separate decisions and are not part of the reservation
  substrate.

---

## Design history

The reservation substrate was built in stages out of the earlier plan/apply
dispatch work. The staged implementation briefs and review rounds live outside
the repo, at the GitHub workspace root under
`reservation-lifecycle-design-2026-06-30/` and
`slot-reservations-design-2026-07-03/`. They are build-process scratch — the
reasoning behind specific decisions — and are intentionally not checked in.
This document is the canonical in-repo description; if the two ever disagree,
**this document (and the code) win** and the briefs are stale.
