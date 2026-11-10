# Ground rules

Verified against: fe06474a

The rules every change to shingo is held to, each with the guard that holds it. A rule here without a guard is a
hypothesis; a test listed as **predicted red** is committed in `testdb.VerifyRed`'s form (green while the named
violations stand, red the day they are gone, so it is replaced by the plain assertion in the commit that fixes
them). Go comments that cite a rule cite this file.

## Where things go

**`docs/` = anything naming both Core and Edge or a wire contract; `<module>/docs/` = operator/dev material for that
binary alone (setup, config, UI, deploy, backup); design records, briefs, round syntheses and incident logs live
outside the repo. Docs cite symbols and tests, never line numbers. A doc states the rule and names its guard; it
does not carry the measurement that justified it. A stream is done when `## Unreleased` carries its lines in operator
language, `scripts/docs-touched.sh` is clean against its diff, and `make docs-verify` has restamped the docs it
touched.**

- `docs/` is reference, not record: present tense, no plan tense, no round or brief names, no tombstones for deleted
  mechanisms, no pointers into anyone's workspace. A design record that must be mentioned is "(design record,
  outside the repo)".
- One home per fact; the other docs link to it.
- No plant data in a committed file or a commit message: no addresses, hostnames or customer part numbers (synthetic
  `SYN` codes and TEST-NET addresses instead). Plant names and shingo's own node names are fine. Guard: repolint's
  `plant-addresses` and `synthetic-part-numbers` rules.

## Docs

- **What a doc cites must still be there.** Every test it names exists and every `go test -run` alternative matches
  one; every code-shaped identifier resolves; no `file:line`; every cited path and relative link is tracked. Guard:
  `scripts/check-doc-citations.sh`, in every gate (`testcite -docs`, `deadident -docs`, `repolint -docs`,
  `docstamp -check`). The scope is defined once, in `shared/scripts/internal/docindex`; `CHANGELOG.md` and
  `docs/testing/population-ledger.md` are exempt (they are history by design).
- **A table the code already states is generated, not typed.** It sits between `<!-- generated: <source> -->` and
  `<!-- /generated -->`; `make docs-gen` rewrites every block from its source of truth (`shared/scripts/docgen`).
  Guard: `TestGeneratedBlocksAreCurrent` fails on a stale block and on a generator that feeds none.
- **Every doc says what it was last verified against:** `Verified against: <sha>` under its title. The gate only
  checks that the line exists and the sha is an ancestor of HEAD. At the end of a stream, after `make docs-gen`,
  `make docs-verify` restamps every doc the readers pass to HEAD, and the restamp is committed with the stream.
  `scripts/docs-touched.sh BASE` lists the docs that cite a path or a symbol the diff since BASE changed; each of
  them is read against the change, or changed with it.
- A change to a schema, a loop, a route, a wire field or a status names the docs it touches, or says none.

## Comments and scars

These are the rules the codebase's comments cite.

- **Cite a symbol, not a line, for anything you expect to survive.** Line numbers drift silently. Guards:
  repolint `comment-references` (a `file:line` past the end of its file), `deadident` (a code-shaped name nothing
  declares), `testcite` (a test name that does not exist).
- **A comment that claims something is pinned names its pin:** a test (after a `PIN:` marker) or a symbol.
  Without one it is a hypothesis: pin it or delete it. Guard: `testcite` reads every `PIN:` marker and every test a
  comment names.
- **When code and a comment disagree, the code is fact.** Correct the comment in the same edit that changes the
  behaviour. Retire by rewriting, and say so: a gravestone ("`TestX` was deleted", "renamed from …") records history
  on purpose, and the readers exempt it by the words touching the name.
- **Expiry by symbol, never by date:** `EXPIRY: when <symbol> is gone`. Guard: repolint `expiry` fails the change
  that deletes the symbol, so the comment retires in the same edit.
- **Pair every notification path with a sweep that checks the condition directly.** A hook fires once and can be
  missed; the sweep asks the world, so the notification is a latency optimisation and the sweep is the guarantee.
  The sweeps are listed in [sweeps-and-monitors.md](sweeps-and-monitors.md).
- **A check must know whether it had the input to check.** A killed run, a skipped test and a pattern that matched
  nothing must not read as a pass. Guard (for the repo-wide text rules): `TestEveryRuleHasACaseThatFires`.
- **A new table gets exactly one home:** declared in the baseline DDL or, for a table only a migration creates, in
  that migration alone and named in `migrationOnlyTables`. Guard: `TestBaselineDDL_DeclaresEveryTable`.

## The gate

`bash scripts/gate.sh` is what CI enforces, run locally, in three tiers:

| Tier | Command | When |
|---|---|---|
| dev | `bash scripts/gate.sh` | every change |
| push | `bash scripts/gate.sh full` | before a push |
| cert | `bash scripts/gate.sh full --unscoped` | certifying a tip; `make release` runs it |

CI's guards job runs `bash scripts/gate.sh scripts`, the same `scripts/check-*.sh` set. `scripts/gate.sh`'s header
says what each tier runs and why; run it from Windows (Git Bash), not WSL.

## Releases

One version for Core and Edge: `v0.MINOR.PATCH`. **MINOR** means a migration was added, `protocol/`'s source
changed or a config key was added or removed: migrate first, and ship both binaries together. **PATCH** means install
and restart.

A stream writes its lines under `## Unreleased` in `CHANGELOG.md`: a `Deploy:` line naming every migration it adds
(per module, `Core v176`, `Edge v45–v46`) or saying `no migration`, then one bullet per change in operator language;
`scripts/check-changelog.sh` fails the gate without them. The merge to `main` cuts the release:
`.github/workflows/auto-release.yml` computes the number from the changes since the last tag, runs the gates on that
commit, commits `release: vX.Y.Z` with its tag and publishes the binaries, with that section as the notes. A plant
installs a tag when the owner says: `sudo bash install-core.sh --tag vX.Y.Z` on the Core server first, then
`sudo bash install-edge.sh --tag vX.Y.Z` on each Edge, and both Logs pages (`/diagnostics`) then show
`vX.Y.Z · <commit>`.

## The order layer

The rules every door, finder and reader of an order is held to, each with the test that holds it.

1. **An order is issued holding exactly what its door declares,** softly: the cart it is for (a pending bin row;
   the cart's `order_bins` row and `named_bin_id` written with the INSERT) and the position it is bound to when that
   position is one carrier's at a time (a pending slot row), position before cart, in the issue's transaction or
   nothing. What the door does not know is the acquirer's. A hard claim is written only at a hand. The issue, door by
   door: [order-builder-dispatch.md](order-builder-dispatch.md).
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
