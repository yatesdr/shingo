# Reservations and the claim discipline

This document describes how shingo-core claims the physical resources a robot
order needs — a source bin and a destination slot — and *when* those claims
become real. It is the reference for engineers touching dispatch, the fulfillment
scanner, or anything that writes `claimed_by`.

## The one rule: soft until complete

An order does not get to hard-claim a bin or a slot until it is actually about to
send a robot. While the order is still waiting — for material, for a free slot,
for the fleet — it holds its resources **softly**, as reservations. The hard claim
(a row in `bins.claimed_by` or `nodes.claimed_by`) is written in one step, for
both resources together, immediately before the call that creates the fleet order.

This is "soft until complete," and both transport families obey it:

- **Simple orders** (a single retrieve / retrieve-empty / move / store): the
  fulfillment scanner soft-reserves the destination slot, then soft-acquires the
  source bin, and confirms both at dispatch.
- **Coordinated orders** (multi-step, edge-authored plans): the allocator
  soft-reserves every distinct source bin and destination slot the plan needs,
  reconciling what it already holds each tick, and confirms the complete set at
  dispatch.

The point of the rule is to never strand a hard claim on an order that isn't
going anywhere. Before this discipline, a simple order hard-claimed its bin the
moment it found one, *then* tried to reserve its slot — so an order that waited
on its slot left a bin locked up for as long as it waited. Other orders couldn't
use that bin, and if the waiting order later failed, the bin was stranded until
reconciliation noticed. Soft-until-complete makes the waiting order polite: it
holds revocable reservations, it is visible to operators as a queued reason, and
the instant it actually dispatches, its claims become hard in one atomic step.

## The two halves: reserve, then confirm

A claim has two halves, and they happen at different times.

**Reserve (soft).** A *pending* row in the `reservations` table. It says "this
order intends to use this bin / slot." It writes nothing to `claimed_by`. Other
orders can see the reservation is held (the finders and the slot excluders skip
reserved resources), so two orders don't both plan to drop into the same slot —
but the reservation is revocable, and reaping it never strands a physical claim.

**Confirm (hard).** Flips the reservation `pending → confirmed` *and* writes the
`claimed_by` column, in one database transaction. After confirm, the resource is
genuinely owned: `bins.claimed_by` / `nodes.claimed_by` is what every other part
of the system reads as "taken." Confirm runs at dispatch, never earlier.

Both halves are owner-idempotent. An order re-confirming a resource it already
hard-claimed (a retry after a crash between the two writes, say) heals in place
rather than failing. This is what makes the reserve/confirm split safe across
scanner ticks and core restarts: the worst case is a half-state that the next
tick fixes, never a permanently wedged resource.

### Slots before bins

When an order needs both a destination slot and a source bin, it reserves and
confirms the **slot first, then the bin.** Ordering one resource class entirely
before the other is what prevents a slot↔bin cross-type deadlock cycle — two
orders each holding the slot the other needs while waiting on the bin the other
holds. The same ordering applies in both families.

## Where it happens in the code

**Simple path** (`shingo-core/fulfillment/scanner.go`, `dispatch/store_slot.go`):

1. The scanner finds a source bin (the shared `SourceFinder`) and moves the order
   to `sourcing`.
2. It resolves the destination node and soft-reserves the slot
   (`ReserveStorageDropoff` — a no-op for a line/consume destination, which has no
   slot to reserve).
3. It soft-acquires the bin (`ReserveForDispatch`, a pending reservation) and
   stamps the order's `BinID`.
4. At dispatch, `ConfirmForDispatch` hard-claims the slot (if a storage dropoff)
   and the bin, then `DispatchDirect` creates the fleet order.

If any step before confirm fails — the slot is contended, the bin loses a race,
the destination can't be resolved — the order parks in `sourcing` (never
`queued`), **keeps its soft reservations**, and the scanner retries it next tick.
Because `BinID` was stamped at soft-acquire, a re-entering order reuses its own
bin instead of shopping for a new one.

**Coordinated path** (`shingo-core/dispatch/allocator.go`, `complex_dispatch.go`):

The allocator's `reserveComplexPlan` / `reserveComplexSlots` reconcile the order's
held reservations against its distinct needs each tick (keeping what still
matches, acquiring what's missing, releasing what no longer matches), and
`confirmComplexPlan` commits the complete reserved set to hard claims at dispatch.
The scanner drives this via `DispatchPreparedComplex`.

**Synchronous manual orders** (`engine.CreateDirectOrder`,
`www.submitSpotRetrieveSpecific`): the operator picks a specific bin, so these
soft-acquire it and confirm at dispatch in the same call. The soft window is
microscopic, but the discipline is uniform — no hard claim before the dispatch
step on any path.

## Why the finder can't see a soft-held bin (and why that's fine)

The bin finders exclude any bin with a pending reservation — `NOT EXISTS (SELECT 1
FROM reservations ... state='pending')` in the SQL, and the `HasPendingReservation`
flag in `BinUnavailableReason`. This exclusion is *owner-blind*: it skips the bin
even if the reservation belongs to the very order doing the finding.

That is deliberate and correct for everyone else — two orders must not both
soft-hold the same bin. For the order's *own* held bin it would be a trap: if a
soft-holding order re-ran the finder, it would not see its own bin and would shop
a second one (double-source). The defense is the keep arm: an order that already
holds a bin (its `BinID` is set from soft-acquire) short-circuits straight to the
held-bin dispatch path and never consults the finder. Its own bin is reused; the
owner-blind exclusion only ever affects *other* orders' holds.

## The one recorded exception: compound reshuffle children

A compound reshuffle order is a parent that spawns child legs to dig a buried bin
out of a lane. Those children are created and sequenced by the compound machinery
in `CreateCompoundChildren` (`store/orders.go`), not by the scanner, and the bin
each child moves is assigned by the reshuffle plan — not found. At creation, each
child takes a **raw bin claim** (a direct `UPDATE bins SET claimed_by`, with no
reservation row) so the sequenced legs don't race each other for the same bin.

This is the one place a hard claim exists on an order that has not reached its own
dispatch step, and it is recorded as the exemption to the invariant below. It is
keyed on the data: a compound child is the only order that carries a
`parent_order_id`.

## The invariant, and the fence that enforces it

**No non-compound order that is still acquiring (`pending`, `queued`, or
`sourcing`) holds a hard bin or slot claim.** A compound child
(`parent_order_id IS NOT NULL`) is the exempted case above.

The fence is a SQL sweep run as a docker integration test
(`dispatch/rule1_invariant_test.go`), not the linter. There is a `forbidigo` rule
that stops the raw bin-finder fallbacks from being called outside the shared
finder, which is useful — but it is *selector-match only*. It cannot see a bare
`UPDATE bins SET claimed_by` written in a transaction, and it cannot see a new
caller that bypasses the reserve/confirm pair. The invariant sweep can. It
populates a real database with an order parked in `sourcing` and asserts the
sweep finds zero hard claims on it, and separately that a compound child's raw
claim is correctly exempted.

If you add a new path that writes `claimed_by`, the sweep is what will catch a
rule violation. Run the docker tests.

## The reservation table, briefly

`reservations` holds one row per (order, resource) pair a soft-hold covers — a
bin (`resource_kind='bin'`, `bin_id`) or a slot (`resource_kind='slot',
node_id`). Two partial unique indexes (`uq_reservations_bin_active`,
`uq_reservations_slot_active`) make Acquire exactly-one-winner per resource: only
one order at a time can hold a pending or confirmed reservation on a given bin or
slot. State is `pending` or `confirmed` only — release is a hard `DELETE`, so
every row on disk is one of those two.

Reaping is **owner-liveness, not age.** An order legitimately sourcing material
may hold its reservations for minutes, hours, or days; demand is operator-driven
and never evaporates on a timer. A reservation is reclaimed only when its owning
order is terminal or gone. (An older age-based reaper was retired when
reserve-at-plan-time made the soft window long; do not reintroduce one.)

The lifecycle is `Acquire` (pending) → `Confirm` (pending→confirmed, paired with
the `claimed_by` write) → `Release` (delete). The per-resource unique index is
why every caller that retries across ticks must load its own held reservations
first and reuse them — re-acquiring your own hold conflicts on your own row.
