# Sequential — the classification, and why there is nothing to refactor

`sequential` is the A/B swap mode: a cell has two paired positions, the line draws from one
while the other is swapped, and a cutover flips which side is active. This document records a
census that has been run twice and reached the same answer both times, so the third person to
ask does not have to run it again.

**The short version: sequential is not a swap-mode cluster. Its references are almost all
step-list-shape questions, which the swap-mode law explicitly permits, and there is no diff to
be had.**

## The law this is measured against

`protocol/swap_mode.go` carries the rule:

> A gate reads the steps or a declared property, never the mode name, and node kind is not a
> swap mode.

with the test for telling one from the other:

> A branch may be deleted only when a property read replaces it. A branch that cannot be
> replaced is ESSENTIAL and stays — code that is choosing or costing the step list itself.

The loader question failed that test badly: "is this a forklift-managed window?" was answered
by comparing the mode at forty-odd sites, which is how the concept ended up with no name. It
now has one (`IsLoaderNode`) and `protocol/loader_derivation_drift_test.go` stops a third
derivation appearing. Sequential was examined for the same shape and does not have it.

## The census

27 non-test references to `protocol.SwapModeSequential`. Three are the declaration and its two
memberships in `protocol/swap_mode.go`, leaving **24 decision sites**: 20 essential, 4
arguable — and all four arguable in the essential direction.

### Essential — the site asks about the shape of the swap (20)

| site | what it means |
|---|---|
| `engine/swap_dispatch.go` — `BuildSwapDispatch`'s sequential arm (2) | a sequential swap is a removal leg that places nothing. This IS the step list. |
| `engine/material_orders.go` — `BuildSwapChangeoverSteps` | the per-mode builder switch. |
| `engine/material_orders.go` — `BuildEvacuateChangeoverSteps` | the same switch, evacuate variant. |
| `engine/changeover_planner.go` — `directTripChangeoverMode` | which modes skip the staging hop. A property of the choreography, and the function already names it. |
| `engine/changeover_planner.go` — `requiredChangeoverFields` | the per-mode required-field registry. |
| `engine/changeover_planner.go` — active-pull resolution and log tags (4) | resolving `inactive`/`active` from the active-pull snapshot is genuinely mode-specific; `swap_sequential` / `evacuate_sequential` are labels. |
| `engine/wiring_status_changed.go` — the sequential backfill | order B follows order A. No other mode has this leg. |
| `domain/claim_validation.go` — sequential's required-field arm | the arm that did not exist, and let a partnerless claim save clean and fail later as an empty dispatch naming a builder instead of a field. |
| `shingo-core/cmd/simcalc/main.go` — `fleetMovesPerSwap` | costs each step-list shape in floor crossings. The shape is the question. |
| `shingo-core/plantspec/plantspec.go` — `Claim.IsMultiStepSwap` | whether the mode needs inbound/outbound staging nodes. |
| `shingo-core/www/handlers_test_orders_direct.go` (3) | maps a requested cycle mode to leg roles in the test-order harness. |
| `shingo-core/www/handlers_test_orders_kafka.go` (3) | the same, Kafka path. |

### Arguable — asks about the CELL, and happens to key on the mode (4)

| site | the argument |
|---|---|
| `engine/changeover.go` — `sequentialParkedSideReuses` | "is this the parked side of an A/B pair, and does its bin carry over" is a question about the pair, not the step list. It is only true of sequential because sequential is the only pairing mode with a parked side. |
| `engine/operator_changeover_release.go` — `linePullsFrom` | the comment there makes the case itself: it is true of sequential *and only* of sequential, because that is the mode whose whole premise is that the other position takes over first. The scope is not a carve-out for a mode; it is the rule being stated about the choreography it describes. |
| `domain/claim_validation.go` — "the two positions of a pair must differ" | shared with press-index by an `\|\|`, which is the honest shape: two modes pair, and the rule is about pairing. If anything ever earns a `Pairs()` predicate, this is the site that names it. |
| `engine/sim_operator.go` — the A/B cutover driver | sim-only, and it is imitating an operator. |

## Zero authored claims is not evidence

Neither plant has a sequential claim configured today. **That is a configuration gap, not
evidence the mode is dead** — the owner has ruled sequential will be used in plants, and the
configuration has not been built yet. Do not read the row count as a deprecation signal, and
do not delete a sequential arm because nothing exercises it.

## The one thing worth carrying forward

`PairedCoreNode` read **alone** means two different things: "who is my A/B partner" in the
sequential sites, and "the back press position" in the press-index ones. One field, two
meanings, no name for either.

If sequential is about to be configured at a plant, that overload is the thing most likely to
be misread by whoever wires it. `domain/claim_validation.go` already refuses the worst case —
a pair whose two positions are the same node, which would have the parked order and the active
order evacuate and refill the same slot — but the field still carries both meanings silently
everywhere else.

## Provenance

Classified during the swap-mode cluster work (2026-09-09), re-verified against the tree
2026-09-10. The re-verification corrected three arithmetic errors in the original write-up: it
reported 31 references rather than 27, 28 decision sites rather than 24, and 17 essential sites
rather than 20 — the tables were complete and correct, the totals above them were not. It also
described `plantspec.Claim.IsMultiStepSwap` as an accepted-mode allowlist, which it is not.
Every line number the original cited still resolved to the site it named.
