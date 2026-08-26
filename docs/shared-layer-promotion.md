# When code may be promoted to a shared layer

Owner decision D10 (2026-08-19), from the structural-refactor review. This is
the strict version, and it was chosen over a looser one deliberately.

## The criterion

Promote only when **all four** hold.

1. **Disagreement is a defect that reaches the floor.**
   This is the load-bearing clause and it is the one that does the work.
   Identical code on both sides is not evidence: two implementations that could
   safely drift apart tomorrow and nobody would notice are a *coincidence*, not
   a shared concept. Ask what breaks on the plant floor if Core answers one way
   and Edge answers the other. If the answer is "nothing" or "you would see it
   in review", stop — the duplication is not the problem you think it is.

2. **Both sides genuinely compute it today, from separate code.**
   Not "one side has it and the other might want it later." A shared layer
   populated preemptively acquires callers that never needed it and becomes
   impossible to reverse.

3. **Its dependencies are at most stdlib plus `protocol`.**
   Anything reaching into a store, a config, or a module-local type is not
   shared logic — it is one module's logic with a wide signature.

4. **It changes in lockstep.**
   A change to it must be a change both sides want at the same instant. Where
   one side needs to move first, or move alone, it is not shared: it is two
   things that currently agree.

## Ship the agreement test in the same commit

A promotion without a drift guard has not removed the disagreement, it has only
moved it. Whatever makes the two sides agree — an agreement test, a table of
checked-in vectors, a one-definition-site scan — lands in the promoting commit,
not after it.

`protocol/wire_vocabulary_drift_test.go` is the worked example for a
single-definition-site guard, and `shingo-core/store/reservations/dig_exclusion_drift_test.go`
is the pattern it was modelled on.

## Where things live

- **Infrastructure** → a `protocol/` sub-package. This is where the repo already
  keeps it: `auth/`, `types/`, `debuglog/`, `backoff/`, `eventbus/`, `outbox/`,
  `clock/`. Wire vocabulary belongs here too.
- **Presentation, cross-surface answers, and cross-module test fixtures** →
  `shared/`.
- **Neither creates a new `go.work` module.** Five is the number.

## The canonical statements

Two packages in `shared/` already state the criterion better than a rule can,
and are worth reading before promoting anything:

- `shared/windoworder/windoworder.go` — "It lives in `shared/` because both
  sides need the same answer and they compute it in separate modules... If the
  two order the windows differently, a carrier goes to a window nobody
  expected." That is clause 1 with a plant consequence attached.
- `shared/loadervectors/loadervectors.go` — "THE VECTORS ARE THE PERMANENT
  GATE, not a migration aid," and its argument for why a live both-implementations
  comparison test dies in the commit it is meant to protect.

For what "correctly shared" looks like on the adapter side, see
`shingo-core/engine/eventbus.go` (thin type aliases over `protocol/eventbus`,
so the local names and types survive) and both `messaging/outbox.go` files
(module-local adapters over one `protocol/outbox` drainer).

## The worked example, and one non-example

The wire-vocabulary consolidation (D9, 2026-08-19) is the worked example: the
eight loader values, the two inventory-delta scope kinds and the outbox retry
cap all crossed the wire, both sides spelled them independently, and a rename on
one side alone would have changed a loader's behavior at a plant or stopped
inventory-delta deduplication silently. Clause 1 is met with a floor consequence
named. The drift guard shipped with it.

`outage_log` was the other promotion candidate from that review and it is moot:
both copies live in `countgroup/`, which D1 retires outright.

## What `shared/` is not

`shared/` began as UI assets and several places still describe it that way. It
has held cross-surface answers (`windoworder`) and cross-module fixtures
(`loadervectors`) for some time. It is still **not** the home for
infrastructure — that is `protocol/` — and it is still not a place to put
something because two files look alike.
