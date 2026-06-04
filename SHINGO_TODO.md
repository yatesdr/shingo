# SHINGO_TODO

Tracked follow-ups and known residual risks that aren't (yet) bugs to fix
today, but that code comments point at so the rationale isn't lost. Each
entry is referenced from the code by its **bold title** (e.g. `see
SHINGO_TODO.md "Two-robot swap supply-leg bin exclusion"`).

---

## Two-robot swap supply-leg bin exclusion

**Status:** open follow-up (belt-and-suspenders). Filed 2026-06-03 from the
ALN_003 swap-starvation work.

**Context.** A two-robot swap (supply ↔ evac) used to let the supply leg
re-claim the very bin the evac leg had just returned to the supermarket —
the pointless round-trip that jammed ALN_003 on 2026-06-03. The **dispatch
hold** (`complex_dispatch.go:swapRemovalLegHeld`, task 1 of that fix) closes
the *normal* path: the removal leg can't evacuate the line bin until the
supply sibling has *already* claimed a replacement, so the evacuated bin is
never a candidate when the supply leg sources.

**Residual risk this entry covers.** If the supply leg's bin claim is lost
*after* the hold releases — an orphaned-claim sweep (`ReleaseOrphanedClaims`
/ `ReleaseExpiredStagedBins`) reaping it, or a non-atomic terminal
transition — the scanner re-sources the supply leg, and by then the
evacuated sibling bin *is* sitting in the supermarket and could be
re-grabbed. Same class as the `claimComplexBins` BinID-persistence races
already pinned in `bin_lifecycle_test.go`.

**Proposed fix.** In the supply leg's source resolution (the
`claimComplexBins` / `FindSourceBin*` candidate query), exclude any bin
claimed by — or being evacuated by — the order's two-robot sibling. The
sibling is reachable via `orders.sibling_order_uuid` (bidirectional since
task 0). One predicate; the cost is that it lives in the sensitive
bin-picker, so it wants its own focused regression pass rather than riding
in on the safety commit.

**Why deferred.** The hold already kills the actual incident and the
normal-flow reuse; this only guards a rare claim-loss race. Separate so the
bin-picker change gets its own review/tests.

Pointers: `shingo-core/dispatch/complex_dispatch.go` (hold + future
exclusion), `shingo-core/store/orders/orders.go:SiblingUUID`,
`aln003-swap-starvation-incident-2026-06-03.md` (fix plan, #4).

---

## Starvation alert time-to-empty escalation

**Status:** open follow-up. Filed 2026-06-03 from the ALN_003 starvation red
card (task 2), which ships **floor-based** first.

**Context.** The operator starvation red card v1 fires when an active
style's lineside UOP drops below a fixed floor (`capacity/4` for transitional
loaders; the reorder point for auto loaders). Simple and predictable, but a
fixed floor doesn't account for *how fast* the line is consuming.

**Proposed.** Escalate the danger tier on **time-to-empty**: red when there
is < N minutes of parts left at the current consume rate, not just < X
parts. Far more actionable for an operator ("~4 min left"). The consume
rate already exists — PLC counter deltas feed `threshold_calculator`; reuse
that. Keep the v1 floor as the fallback when no rate is available (cold
start). The card's danger computation is deliberately a single function
(see task 2) so this slots in without touching the render path.

---

## Swap sibling cancel cascade on manual cancel

**Status:** open follow-up. Filed 2026-06-03.

**Context.** Cancelling one leg of a two-robot swap should cancel the other.
This already happens for the two paths that matter most: changeover
`AbortNodeOrders` (both legs share the line node) and the stuck-order TTL
(`AbandonStuckOrders` cascades to the sibling, task 3). But a **manual
single-leg cancel** (`HandleOrderCancel`) does *not* cascade — cancel just
the supply leg and the held removal leg (task 1) waits for the 1h TTL
instead of clearing immediately.

**Proposed.** In `HandleOrderCancel` / `LifecycleService.CancelOrder`, when
the order has a `sibling_order_uuid`, cancel the sibling too (idempotent;
mirror the engine `abandonOrder` cascade). Also consider: when the supply
sibling is already terminal, `swapRemovalLegHeld` could release the hold (or
cancel) rather than hold until the TTL.

Pointers: `shingo-core/dispatch/dispatcher.go:HandleOrderCancel`,
`shingo-core/dispatch/lifecycle.go:CancelOrder`,
`shingo-core/dispatch/complex_dispatch.go:swapRemovalLegHeld`.
