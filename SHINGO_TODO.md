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
