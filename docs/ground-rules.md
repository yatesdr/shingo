# Ground rules for the order layer

The rules every door, finder and reader of an order is held to, each with the test that holds it. They were
written down after the press tranche's attack rounds (2026-10-24) found the same drift twelve times: a sentence
about the tree that nobody read back against the tree. A rule here without a test is a hypothesis; a test listed
as **predicted red** is committed in `testdb.VerifyRed`'s form (green while the named violations stand, red the day
they are gone, so it is replaced by the plain assertion in the commit that fixes it).

`AGENTS.md` is not a tracked file; it points here.

## The rules

1. **An order is issued holding exactly what its door declares,** softly: the cart it is for (a pending bin row;
   `bin_id` and `named_bin_id` stamped in the INSERT) and the position it is bound to when that position is one
   carrier's at a time (a pending slot row), position before cart, in the issue's transaction or nothing. What the
   door does not know is the acquirer's. A hard claim is written only at a hand.
   Tests: `TestCensus_Issuers`, `TestIssueCensus_AtCommit` (dispatch and engine, one table:
   `testdb.IssueDoors`), `TestRule1_NoHardClaimWhileSourcing_Sweep`.

2. **A hold the issue cannot take refuses the issue:** nothing is written, no log, no park of a row that does not
   exist. A pull's loop goes to its next candidate; a person is told.
   Test: the census's take column (`TestIssueCensus_AtCommit`, a stranger's pending hold on each door's cart).

3. **A take is a person's act, decided by who asked** (`dispatch.Need.By`). A person takes a stranger's pending hold
   out loud, on the cart or on the position the order declares: the holder un-pointed and told (a cart's holder that
   is itself an order for the cart ended by name; a position's holder told `slot-taken`, as a dig's ranked take tells
   it). Core's own errand takes nothing; nobody takes a confirmed hold, and a person refused by one is told whose it is.
   Tests: the census's take column; `TestTakeSoftHolds_*`; `TestNamedMove_TakesAPendingHoldOnItsDropOutLoud`,
   `TestNamedMove_IsRefusedByAConfirmedHoldOnItsDropNamingItsHolder`.

4. **A want is a `Height` value on the demand,** stated at intake and stored. No order type, intent or step flag
   carries a height or a carrier state. A step says what it lifts.
   The want rides the wire as `protocol.OrderRequest.Want` and Core's row as `orders.want`; a find reads it into
   `plan.Need.Height`.
   Test: `TestRule4_NoOrderTypeOrIntentCarriesAHeight` (the two deleted names absent from every module, the plain
   family three types), and forbidigo's tombstone on the two names (`.golangci.yml`).

5. **A pickup takes a carrier that answers what its step names:** a lift, whatever stands at its node; an empty, a
   part-less carrier the set-down height admits (and, where the step names the part it is for, one that part may
   ride); stock of a part, a carrier loaded with it, confirmed and counted above 0. The same rule for produce and
   consume.
   Tests: `TestRule5_EveryStepKindAgainstEveryCarrierState` (the bind, `binresolver.StockOnly` for stock);
   the Edge's steps say what they lift: `TestPressIndexChangeoverR2_OnDeckPickupsSayWhatTheyLift`,
   `TestKeepStagedSwap_LiftsTheSpareOnceAfterTheWait`.

6. **Every availability reader composes the one fragment or its Go twin,** held by one agreement table over one
   fixture set; a reader that omits an arm names it in the table, with the reason.
   Test: `TestRule6_OneAvailabilityTable`, all assertions (a group's slot children ask the part's carrier rule,
   `binresolver.Store.LoadBinTypeRule`, read at the scan's first flat child holding bins).

7. **A count is a claim.** "Counted to nothing" has one spelling each side (`domain.Bin.Stocked`,
   `helpers.StockSQL`; the Edge reads the bins feed's `Stocked`), and so has "a tote type"
   (`protocol.IsToteType`, SQL `IS NOT NULL`); "empty" is written only by a person's act or by the release of the
   leg that lifted the line's cart.
   Tests: `TestRule7_CountedToNothingHasOneSpelling`, `TestRule7_ToteTypeHasOneSpelling`.

8. **"A carrier is coming to X" is `protocol.OrderPlacesBinAt` over live rows,** pinned by the shared vectors
   (`shared/comingvectors`); every reader composes it or is run over them.
   The loader pulls' free windows compose it (`orders.CarrierComingTo`); the window choice is run over the vectors and
   reads the delivery node, by ruling: an ask asks where nothing of its own intent is asked (2026-08-05).
   Tests: `TestComingVectors_CoreAgreesOverStoredRows` (Core), `TestComingVectors_TheEdgesReadersAgree` (the Edge's
   three readers), `TestComingVectors_TheWindowReadersAgree` (the pulls' free windows and the window choice).

9. **The census's unit is a caller.** Every function that reaches the issue is named, with what its door's order
   holds at commit. A comment that claims "every X" or "the one Y" names the test that proves it.
   Test: `TestCensus_Issuers` (the walk up from the one INSERT; a new caller anywhere on it fails).

10. **A brief may STOP a mechanism, never fence a builder out of the shared door.** "If the rule must go beside the
    shared read, STOP" means stop and redesign, not build beside it. (The orchestrator's rule, for the orchestrator.)

## Go idiom

- Extract only where a second caller exists.
- No bool that selects behaviour; a value, not a flag.
- No interface with one implementation.
- No export without a reader outside the package.
