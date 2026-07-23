# Changelog

## 2026-07-22 — Nodes page cleanup, map token promotion, template-var check

### Added / Changed

- **Nodes page** style-guide compliance: the node-tile colours move onto the
  shared palette (has-payload green, staged teal instead of a cyan that collided
  with in-transit, maintenance amber, disabled danger, claimed warning border),
  which lets the dark-mode tile overrides drop; the undefined `var(--card-bg)` is
  renamed to real tokens; the two Create Loader buttons become primary buttons;
  the Ctrl/Shift-click test-node actions become an explicit "Dev tools"
  disclosure; and the dead "Check Occupancy" block (which errored on live plants)
  is removed.
- **Map palette** moved into shared tokens: the `--map-*` floor-plan colours and
  the map JS's two local hex palettes (dock colours, node-class colours) now come
  from `shared/tokens.css`. No visual change.

### Tests

- New `TestNoUndefinedCSSVarsInTemplates` fails CI when a template references a
  `var(--foo)` that no stylesheet defines — the gap that let `var(--card-bg)`
  render as unset. Everything builds, vets, lints clean, and the full drift +
  JavaScript test set passes.

## 2026-07-22 — Inventory page rebuilt (v2), exception-first

A rebuild of the inventory page around what needs attention, plus the backend
to power it. The old bins table moved to /bins; consumption, produced, and
cycle-time moved to the per-payload drill and to Missions.

### Added / Changed

- **New endpoint `GET /api/inventory/monitor-totals`** returns one row per
  payload that is monitored or stocked: DB on-hand (bins + lineside split), the
  threshold monitor's cached total (so the page can flag cache-vs-DB drift), the
  configured threshold(s), and the catalog description. Backed by a new
  `ThresholdMonitor.Snapshot()` (reads the monitor's in-memory totals and
  bindings under its lock) and a `ReplenishmentHealth` rollup that joins the
  monitor snapshot, the stocked-payload list, per-payload UoP, and descriptions.
- **Lineside buckets now carry `updated_at`**, so the page can colour stale rows
  (amber over 7 days, coral over 30) — a bucket untouched for a month is a ghost
  that inflates the in-loop total and never fires a threshold.
- **The inventory page** now leads with a Replenishment Health table (one row
  per payload, worst first), a five-tile KPI strip, and a conditional alerts
  banner; each health row expands to an explicit-save threshold editor (with a
  suggest-then-apply calculator) and the payload's holding bins. Search filters
  every section. The native pop-up dialogs, silently-dropped saves, and
  fixed-colspan empty rows are gone.

### Notes / follow-ups

- The consumption drill shows window totals and days-of-cover rather than a
  daily trend chart: consumption is recorded per part number, which does not map
  one-to-one to a payload code, and there is no per-payload daily series
  endpoint yet. The "show on map" button is a stub until the map's material
  layer lands.
- The Postgres-backed and race test suites were not run locally (they need WSL
  or Docker); the build, vet, linter, and the monitor-snapshot unit test pass.

## 2026-07-22 — Map: zoom/pan and truthful route rendering

Interactive framing and honest route animation on the dashboard map
(`dashboard-map.js`). No schema or API change. The map's on-screen behavior
needs a live scene to confirm — there is no automated JavaScript test for it.

### Added / Changed

- **Manual view.** Wheel-zoom at the cursor and left-drag to pan. Either one
  takes over the view, so the frame stops auto-chasing active orders until you
  press Recenter. Zoom keeps the point under the cursor fixed. A new Recenter
  button (crosshair icon) eases the view back to the active area and resumes
  auto-follow; the small overview map is now click-to-jump. A drag that actually
  moved is not treated as a click, so panning never accidentally selects a robot.
  The map page now loads the shared component styles and the icon sprite.
- **Honest route comets.** The map data carries no "picked up yet" flag, and no
  order status separates the trip to the pickup from the trip to the drop-off
  (both are "in transit"), so the code infers the current leg from how close the
  robot is to its source and locks it in once the robot reaches the source. The
  comet now points along the leg the robot is actually driving — toward the
  source until pickup, then toward the delivery — instead of always pointing at
  the delivery. Empty runs draw hollow dots, loaded runs draw solid dots. Comets
  only animate while the robot is moving; a robot stopped mid-route shows a faint
  static line, a blocked or faulted one a red static line, and orders not yet
  dispatched (or already staged) show at most a dashed intent line. The reduced-
  motion setting is honored throughout.

### Tests

- Template parsing, the emoji and inline-handler checks, and the JavaScript unit
  tests all pass. The map has no automated UI test; the leg inference and the
  three route states were reviewed by reading the code and need a running scene
  to confirm visually.

## 2026-07-22 — UI refresh groundwork: cleanups, design tokens, and the icon set

Groundwork for the inventory, map, and nodes refresh. No behavior change — new
design tokens, documentation, one new asset, and a set of mechanical
style-consistency fixes. All existing checks pass.

### Added / Changed

- **Admin-page cleanups (Core).** `loaders.js` now uses the styled confirmation
  dialog instead of the browser's native `confirm()`, and its one stray green
  color is replaced with the theme's accent. `diagnostics.js` fire-alarm and
  robot-status colors move from hard-coded hex values to theme colors and the
  shared robot-status badge classes. The node "check occupancy" discrepancy rows
  drop their light-only inline backgrounds for theme colors that stay legible in
  dark mode. A new "locked" badge reuses the existing amber "claimed" styling, so
  no new color is introduced.
- **"UoP" casing.** Visible text reading "UOP" is now "UoP" across the bins,
  payloads, nodes, material, processes, and replenishment pages. Code names,
  data attributes, and JSON keys are left alone.
- **Design tokens (`shared/tokens.css`).** A five-step type scale and two font
  weights; a 4px-based spacing scale; a single-hue teal ramp and a
  teal-to-coral diverging ramp for heat maps and signed charts (both light and
  dark); and motion tokens (two durations, one easing) with a reduced-motion rule
  that zeroes the durations so animation is disabled for users who ask for it.
- **Icons.** A ~22-icon subset of the Lucide icon set (ISC license) is vendored
  as one SVG sprite (`shared/icons.svg`), embedded and inlined once into the Core
  layout. Icons are single-color and take the surrounding text color.
- **Style guide.** Reordered the chart palette so two adjacent series no longer
  collapse for red-weak viewers (same colors, new order); added a "visual
  principles" section (keep structure quiet and let live state carry the color;
  animate only real movement; focus one thing and dim its neighbors); documented
  the new tokens and the icon and no-emoji rules; added a "UoP" glossary entry.

### Tests

- New `shared.IsEmoji`/`FirstEmoji` helper with a unit test, and a
  `TestNoEmojiInTemplatesAndPageJS` check in both `www` packages that fails on
  any emoji in a template or page script (the tree is currently clean). Build,
  vet, unit tests, and the linter (0 issues) pass across all four modules, along
  with the existing consistency checks and the JavaScript unit tests.

## 2026-06-12 — Bin-loader multi-window refactor, checkpoint C4a: multi-window delivery (flag-gated)

Activates the multi-window runtime — the payoff of the refactor — behind a config
flag, default OFF. A shared loader configured with N windows now shares one budget
of N and spreads empties one-per-window; **one demand of N → exactly N, never 2N,
never two at one window.** This is the budget=N never-2N gate deferred from C3.

### Added / Changed

- `config.LoadersMultiWindow` (`loaders_multi_window`, default false) — gates
  shared-window spreading. Off by default until the operator board (A2) renders
  per-window state and the demand re-key (B9) land; a multi-window loader would
  otherwise stage bins at windows the HMI can't show.
- `domain.Loader.ReservationTarget(payload, multiWindow)` — with the flag on, a
  shared loader resolves to (its windows, `SlotCount`); off, to (anchor, 1). A
  single-window loader is unchanged either way, so flipping the flag only affects
  loaders actually configured multi-window.
- The reservation seam distributes empties **round-robin to free windows** (the
  windows with no empty in flight), one physical bin per window; `fire` now takes
  the target window list. The never-2N budget stays per-loader (keyed on
  `loader.ID()`), so spreading does not fragment it. Decision record gains the
  `targets=[…]` field.

### Tests

- `TestMultiWindow_DemandOfN_ExactlyNAcrossWindows` (flag on: 3-window loader, one
  demand of 3 → exactly 3, one per window; full loader fires 0),
  `TestRace_MultiWindow_NeverExceedsWindowCount` (concurrent, under `-race`: never
  exceeds the window count, ≤1 per window), `TestReservationTarget` (per-layout
  semantics). Flag-off behavior is unchanged (existing suites green). Full edge
  `-race` suite green; build/vet/gofmt/golangci-lint clean.

### Remaining for C4 (the feature's HMI + acceptance half)

The runtime is ready and flag-gated. Still to land before turning the flag on in
production: **the view-path cutover + A2 operator board** (`BuildView` reads the
Loader projection; one loader card + per-window strip; converge the renderers),
**B9** (`loader_id` demand re-key + typed binding key; retire the anchor), and the
**dockerized sim acceptance gate** (`SimSLN002_Replay` + `SimMultiWindow_DemandOfN_ExactlyN`
under `-race`) — which needs the simulator running.

## 2026-06-12 — Bin-loader multi-window refactor, checkpoint C3: the Loader owns the reservation

Makes the loader empty-in path resolve and reserve through the first-class
`*domain.Loader`, retiring the `manualSwapNode {node, claim}` shim as the unit of
resolution. A structural refactor under green tests — the never-2N invariant was
already enforced in C1; this is the "rename, not a race-fix" step.

### Changed

- **The reservation seam is Loader-first.** `reserveLoaderEmpties(loader *domain.Loader, …)`
  keys its mutex on `loader.ID()` and gets the delivery set + budget from
  `loader.ReservationTarget(payload)`. That method encodes the per-layout
  semantics in one place: shared funnels to the anchor (budget 1) until C4 widens
  it to the windows (budget = `SlotCount`); a dedicated payload maps to its one
  independent position (budget 1). C4 activates multi-window by changing only
  `ReservationTarget`.
- **The resolvers return `*domain.Loader` via the LoaderStore.** `findLoaderForDemand`
  and `HandleLoopBelowThreshold` resolve through the flag-selected store (the two
  flag-branches collapse to one call) and fail **closed** on a real store error
  (a DB flicker drops the signal instead of rerouting it). `tryCreateL1` and
  `refillLoaderForPayload` take `*domain.Loader`; the latter reads
  `loader.MinStockFor(payload)` instead of a flag branch + `loaderMinStockFromCore`.
- **`domain.Loader` carries the empty-in config** it needs (`InboundSource`,
  per-payload `MinStock`) via functional options (`WithInboundSource` /
  `WithMinStock`), so the legacy claim is fully out of this path. The fire closure
  resolves the process node from the delivery node at fire time.
- `manualSwapNode` survives only behind `findManualSwapNodes` (the push/board
  enumeration the **unloader** shares) — its retirement there is deferred. Removed
  the now-dead `loaderDeliveryNodes`.

### Tests

- Reworked the seam + try_create_l1 + findLoaderForDemand tests to the
  `*domain.Loader` API; the budget-property test asserts budget 1 (the C3 shape) —
  the budget=N multi-window property moves to C4 with `ReservationTarget`. A lazy
  `loaders()` accessor lets struct-built test engines resolve without a nil store.
  Full edge `-race` suite green; build/vet/gofmt/golangci-lint clean.

### Docs

- `docs/bin-loader-unloader-architecture.md` — seam is Loader-first
  (`ReservationTarget`); LoaderStore now consumed by the empty-in path; the shim
  retirement is scoped. (The adjudication named `edge-named-methods.md` /
  `engine_db_methods_residual.md`; the former is a Phase-4 traceability table and
  the latter does not exist, so the canonical bin-loader doc is the home.)

## 2026-06-12 — Bin-loader multi-window refactor, checkpoint C2: consumer-defined LoaderStore

Collapses the two loader-config duals (legacy `style_node_claims`, Core-owned
aggregate) behind one consumer-defined interface, so callers stop branching on
the `loaders_from_core` flag on every lookup. **Dormant** — the store is
constructed and refreshed on each sync but the hot-path resolvers still return
`*manualSwapNode`; C3 swaps them and retires the shim.

### Added

- **`engine.LoaderStore`** interface (`LoaderForPayload` / `LoaderAt` /
  `Loaders`) + `ErrLoaderNotFound` sentinel. Defined at the consumer (engine).
- **`aggregateLoaderStore`** — projects the Core cache into validated
  `*domain.Loader`s and holds them as an **immutable snapshot** swapped
  atomically (`atomic.Pointer`) on each node-list sync (`SetCoreLoaders` →
  `Refresh`). Resolution reads memory, never the DB — eliminates torn
  multi-statement cache reads and the demand-reroute-on-DB-flicker bug, and
  removes the five-times-repeated full-table scan.
- **`legacyLoaderStore`** — projects each `manual_swap` claim into a
  single-window loader via `WalkClaims`.
- `domain.Loader.Contains` / `ServesPayload` accessors.

### Error contract (fail closed)

A clean miss returns `ErrLoaderNotFound` (caller may fall back); a real failure
(DB read, malformed config) returns a wrapped error (caller fails closed, never
reroutes demand). Callers branch with `errors.Is`.

### Tests

- `TestLoaderStore_FlagDual` (legacy ≡ aggregate for one loader),
  `TestResolve_CleanMiss_ReturnsSentinel`, `TestResolve_DBError_FailsClosed`
  (legacy errors ≠ miss; aggregate keeps last-known-good across a failed
  refresh), `TestRace_CacheReplaceDuringClusterResolve` (snapshot swap under
  `-race`, no torn loader). Full edge `-race` suite green.

### Docs

- `docs/bin-loader-unloader-architecture.md` — LoaderStore section (interface,
  the two impls, snapshot, fail-closed error contract). (The adjudication named
  `shingo-edge/docs/architecture.md`, which does not exist; the bin-loader
  architecture doc is the canonical home.)

## 2026-06-12 — Bin-loader multi-window refactor, checkpoint C1: the reservation seam

Makes loader empty-in creation count→fire **atomic per loader**, enforcing the
never-2N invariant on today's node-keyed world — the foundation the multi-window
feature builds on. Two commits (part 1 additive/behavior-preserving, part 2 the
behavior-changing seam, race-gated), per the bisectability split.

### Changed — part 1 (prerequisites)

- `loaderMemberNodes` branches on the loader **Layout** instead of
  `len(Positions)>0`, fixing the live bug where a shared_window loader with window
  homes emitted `allowed:[""]` members. Shared loaders project a single anchor
  member with the full shared set; the resolver still funnels them to the anchor
  (delivery/count stay there until C4+).
- `ListActiveByDeliveryNodeSet` — one `IN`-query counting in-flight orders across
  a delivery-node set in a single snapshot.

### Changed — part 2 (the seam)

- **`reserveLoaderEmpties`** — the single chokepoint for loader L1 creation. A
  per-`LoaderID` `sync.Map` mutex serialises count→fire so a demand signal and an
  operator REQUEST can't both pass the in-flight count and both fire. One set
  query yields the per-payload dedup **and** the loader-capacity cap (budget = the
  delivery-set cardinality, retiring the magic `manualSwapWindowSlots = 1` on the
  loader path; the unloader still uses the constant). **No transaction** —
  atomicity is the mutex plus count monotonicity, and `CreateRetrieveOrder` is not
  tx-pure (Core enqueue + synchronous emit mid-write). Fails closed on a count
  error. Structured `loader_reserve` decision record per reservation.
- All four empty-firing writers route through it: `tryCreateL1` (threshold +
  side-cycle), `RequestEmptyBin` (operator, manual_swap), and
  `maybeStageLoaderEmpty`/`MaybePushLoader` (via `tryCreateL1`).
- **Pinned re-entrancy rule:** no order-event subscriber may synchronously
  re-enter the seam for the same loader (the mutex is non-reentrant) — documented
  in `bin-loader-unloader-architecture.md` and guarded by a deadlock test.

### Tests

- `TestRace_LoaderBudget_ConcurrentSignalsAndOperator` (the gate, run under
  `-race`), `TestReserveLoaderEmpties_PropNeverExceedsBudget` (randomized,
  multi-window budget), `TestReserveLoaderEmpties_EmitDuringReservation_NoDeadlock`,
  plus `TestLoaderMemberNodes_BranchesOnLayout` and `TestListActiveByDeliveryNodeSet`.
  Full edge `-race` suite green.

### Docs

- `docs/bin-loader-unloader-architecture.md` (reservation seam + re-entrancy rule),
  `docs/uop-threshold-replenishment.md` (loader-total budget; dedup contract now the
  seam), `shingo-edge/store/store.go` transaction-contract comment (seam owns no tx).

## 2026-06-12 — Bin-loader multi-window refactor, checkpoint C0: first-class Loader type

Foundation for the multi-window bin-loader work (a `shared_window` loader has N
window nodes presenting one shared demand; load either window → satisfied). See
`bin-loader-multiwindow-reviews-2026-06-12/FINAL-ADJUDICATION.md`. C0 is purely
additive — **no runtime behavior change** — and lands ahead of the C1 reservation
seam that enforces the never-2N invariant.

### Added

- **`domain.Loader`** (`shingo-edge/domain/loader.go`): the Edge runtime's
  first-class bin-loader aggregate. Unexported fields, two constructors
  (`NewSharedWindowLoader`, `NewDedicatedPositionsLoader`) that make invalid
  states unconstructible — a shared layout with per-position payloads is
  forbidden by the type signature; zero members, empty node ids, empty payloads,
  and slot-count mismatch are rejected at construction. `SlotCount` (the
  shared-window budget) is derived, never passed.
- **Typed identifiers** `LoaderID`, `NodeID`, `PayloadCode` (newtypes over
  string), adopted on the new Loader surfaces only — the compile-time guard
  against the A1 bug class (a count keyed by the wrong node string).
- **Explicit position `kind`** (`window` | `dedicated`) on the wire
  (`protocol.LoaderPosition.Kind`) and the Edge cache
  (`core_loader_positions.kind`), replacing the empty-`payload_code`-means-window
  convention. Derived from the parent loader's `layout` at the single Core
  projection point (`BuildLoaderInfos`), so layout stays the one source of truth.

### Tests

- `TestNewLoader_RejectsInvalidStates` (the C0 gate) plus valid-construction,
  derived-slot-count, and accessor-immutability tests.
- `TestBuildLoaderInfos` extended to pin the derived position `kind`.

### Docs

- `docs/terminology.md` — bin loader / unloader / window / dedicated position /
  anchor / budget.
- `docs/data-model.md` — bin-loader aggregate, position kind, typed identifiers.
- `docs/wire-protocol.md` — documented `NodeListResponse.loaders` (`LoaderInfo` /
  `LoaderPosition` incl. `kind` / `LoaderPayloadInfo`), previously undocumented.

## 2026-05-17 — Test Infrastructure Cleanup, Phase 6: Fleet-Simulator Doc Sync

### Documentation

- Fixed the **TC-60 "10-step swap"** discrepancy in
  `docs/fleet-simulator/complex-orders.md` — the test code is a 9-step
  sequence (single-robot swap with 1 pickup at storage + 1 pickup at
  line + 6 motion/wait/drop steps + 1 final dropoff). All `10-step`
  mentions updated to `9-step`.
- Updated `docs/fleet-simulator/architecture.md` and per-domain doc
  "Test files" sections to reference the post-Phase-3 split file names
  (`engine_simulator_test.go`, `engine_claim_test.go`,
  `engine_linechangeover_test.go`, `engine_quality_test.go`,
  `engine_terminal_test.go`, `engine_reconciliation_test.go`,
  extended `engine_concurrent_test.go`) instead of the deleted
  `engine_test.go`. All 18 stale test-file refs across the 3 large
  domain docs corrected.

### Deferred

- The full learning-mode prose pruning (plan target: 30–40% line
  reduction across the 9 docs) was deferred. A scan of the four
  largest docs (`changeover.md`, `complex-orders.md`,
  `core-dispatch.md`, `bin-reservation.md`) found that they are
  already organized as scenario/expected/result/root-cause specs with
  minimal learning-mode prose; an invasive line-reduction pass would
  remove operationally useful context (test scenarios, production
  risk callouts, before/after diff blocks) without clear payoff.
  Plan-prescribed updates landed: the TC-60 fix and the test-file
  reference cleanup. Deep prose pruning captured as a Phase 7
  candidate to be revisited only if a specific reader complaint
  surfaces.

## 2026-05-17 — Test Infrastructure Cleanup, Phase 5: MustNoErr Migration

### Refactoring

- Added `testutil.MustNoErr(t, err, msg)` and `testutil.Context(t, timeout)`
  helpers to `protocol/testutil/`.
- Migrated ~1,224 `if err := ...; err != nil { t.Fatalf(...) }` and
  `err := ...; if err != nil { t.Fatalf(...) }` sites across 139 test
  files to `testutil.MustNoErr`. Net diff: **-2,342 LOC**. Only sites
  where the `err` variable is unused after the fatal block were migrated;
  sites that reference `err` later (e.g. `errors.As`, sentinel checks),
  multi-statement err blocks, `t.Errorf` (non-fatal), and `t.Fatalf`
  templates with additional format args were left alone.
- Collapsed eight `TestParseDebugFlag_*` functions in
  `protocol/debuglog/flag_test.go` into a single table-driven
  `TestParseDebugFlag` covering all 8 cases via `t.Run`.

### Developer tooling

- Added `make test-explain` target in `shingo-core/Makefile` to run the
  `//go:build explain` bins-store EXPLAIN-plan regression harness.
  Local-only — not in CI. Use before merging changes to the bins-store
  query layer (`FindSourceBinFIFO`, `ResolveRetrieve`,
  `FindEmptyCompatibleBin`) to catch index regressions at production
  scale.

### Out of scope

- The partial `changeover_diff_test.go` table-driven consolidation
  (plan Step 5.3b) was skipped — the 5–9 candidate functions sit
  alongside 20+ structurally-distinct neighbors and the partial
  conversion adds split-form complexity without enough payoff.
  Captured as a Phase 7 candidate.

## 2026-05-17 — Test Infrastructure Cleanup, Phase 4: t.Parallel + -race CI

### Refactoring

- Added `t.Parallel()` to ~1,600 top-level test functions across
  `protocol/`, `shingo-core/`, and `shingo-edge/`. Skipped:
  `shingo-edge/www/` (package-level shared `testDB`),
  `shingo-core/countgroup/loop_test.go` (timing-mechanic tests),
  `TestConcurrent_ClaimRaceDeterministic` (intentionally serial
  TOCTOU exerciser). Functions using `t.Setenv` / `os.Setenv` left
  serial because Go forbids combining them with `t.Parallel()`.
- Per-test DB isolation via `testdb.Open(t)` was already in place;
  the mass addition just unlocks the parallel scheduling.

### Continuous integration

- Added `-race` detector to `core.yml`, `edge.yml`, and
  `protocol.yml` as `continue-on-error: true` steps. Warning-only
  for the first 30 days (until **2026-06-16**) so the team can
  fix any latent production races without merge blocks. Promote
  to gating once the baseline is known clean — tracked as a
  Phase 7 candidate.

## 2026-05-17 — Test Infrastructure Cleanup, Phase 3: Engine Split + Coverage Rename

### Refactoring

- Split `shingo-core/engine/engine_test.go` (1734 LOC, 19 funcs) into
  7 behavior-clustered files: simulator, claim, line-changeover,
  quality, terminal, reconciliation, plus extending concurrent.
  Original file deleted. `setupThreeBinLine` helper hoisted to
  `engine_testhelpers_test.go`.
- Renamed 19 `*_coverage_test.go` files to drop the `_coverage` tier
  signal. The `//go:build docker` tag (where present) carries the
  integration signal; the filename suffix was misleading.
- Minor rename adjustment: `shingo-edge/orders/manager_coverage_test.go`
  renamed to `manager_db_test.go` (not `manager_test.go` as planned)
  because that name was already taken by an unrelated fakes-based suite.

## 2026-05-17 — Test Infrastructure Cleanup, Phase 2: Eventually Adoption

### Refactoring

- Hoisted `Eventually` / `EventuallyWithInterval` / `AssertEventually`
  to `protocol/testutil/`. Deleted the two prior copies in
  `shingo-core/internal/testdb/` and `shingo-edge/testharness/`.
- Replaced 8 `time.Sleep`-based polling loops with `testutil.Eventually`.
- Deleted 2 local `eventually` helpers in `scanner_test.go` and
  `sse_test.go`; both duplicated the canonical helper.
- Refactored `mockEmitter.waitFor` (SSE tests) to fatal-on-timeout
  via `testutil.EventuallyWithInterval`; eight callers no longer
  carry the `if !waitFor(...) { t.Fatal(...) }` boilerplate.
- Replaced `waitForTransitions` polling internals with
  `testutil.EventuallyWithInterval`.
- Added KEEP annotations on 14 intentional `time.Sleep` calls
  (negative assertions, timestamp separation, timing-mechanic tests,
  post-stop quiet windows). The 10 server-side SSE event-pacing sleeps
  carry `// KEEP: localhost server-side event pacing` headers so the
  intent is unambiguous to future readers.

Net effect: `testutil.Eventually` went from 0 callers to a baseline
of real callers across protocol, shingo-core, and shingo-edge. SSE
test client side is no longer wall-clock dependent.

## 2026-05-17 — Test Infrastructure Cleanup, Phase 1: TC Scheme Removal

### Refactoring

- Stripped `// TC-*` test comments across 13 files (~65 comments).
- Renamed 25 `TestTC##_*` functions to `Test<Subject>_<Behavior>` form.
- Rewrote 3 TC-77 known-issue references in `wiring_completion.go` and
  `changeover_flow_test.go` to describe the failure mode by name
  (`phantom-inventory pin`) rather than TC token.
- Updated `docs/fleet-simulator/` references to new test names.

### Documentation

- Deleted `shingo-core/docs/test-catalog.md`. Of 262 test functions only
  70 carried a TC ID, with 30 numbering gaps and ~30 missing comment
  headers. Catalog admitted in-doc it was outpaced by test growth.
  Learning-mode scaffolding, not load-bearing infrastructure.

### Out of scope

- Fleet-simulator design docs retain internal TC numbering. Phase 6
  will streamline these docs without dropping the spec convention.

## 2026-05-06 — Dispatch Test Coverage & Retrieve-Algorithm Collapse

### Refactoring

- **`dispatch/binresolver/`**: collapsed the FIFO/COST/FAVL retrieve
  variants into a single algorithm parameterized by sort key, and
  split the resolver into per-strategy files matching the Stage 5
  layout. Test gaps in `classifyEmptyGroup`, lane-lock acquire/release,
  and store-strategy routing closed out at the same time.

## 2026-05-05 — UOP Runtime Cache, Side-Cycle Auto-Confirm Guards, Debug Log UI

### UOP Runtime Cache Binding

The runtime UOP cache (used by the operator HMI to render bin contents
before the bin record is updated) now binds to **release-click** and
**OrderDelivered**, not to operator confirm. Confirm-time binding meant
the HMI could show stale UOP for the entire two-robot delivery window.

- `engine/release.go` snapshots UOP at the click and again on delivery
- Edge regression test covers the contract end-to-end
- Failed-order tests realigned to the faulted grace-period semantics
  introduced 2026-05-04

### Side-Cycle Auto-Confirm Guards

Side-cycle orders (manual\_swap L1/U1 inbound/outbound legs) were being
auto-confirmed twice — once by the swap dispatcher and again by the
reconciliation sweep on the next tick.

- `skip_auto_confirm` flag on side-cycle orders prevents reconciliation
  from re-confirming
- `RequestEmptyBin` / `RequestFullBin` no longer auto-confirm on
  manual\_swap nodes
- Authoritative LOADED badge derived from the bin record, with an
  empty-bin escape hatch when the operator needs to recover from a
  partial load

### Two-Robot Changeover

- Operator station gains a RELEASE button for two\_robot swap during
  changeover (previously only single-robot had one)
- `SituationAdd` delivers directly to the lineside node, skipping the
  staging hop that was creating phantom in-flight orders during
  changeover
- Two\_robot swap correctly falls back when `OutboundStaging` is empty
  instead of dispatching a no-op

### UI

- Bin-loader payload cards: render **NO DEMAND** (neutral) instead of
  a false **QUEUED** badge when no order is sourcing from the loader
- Debug log beautification: color-grouped by source/level, JSON expand
  on click, row grouping for repeated messages, brighter palette and
  larger text for plant-floor monitors
- Amber background on the changeover RELEASE header button so it
  reads as the active step
- Changeover RELEASE row column overflow fixed in grouped views

## 2026-05-04 — Failed Order Recovery, Edge as UOP Authority

### Failed Order Recovery (Faulted Grace Period)

`failed` is no longer terminal. Orders that fail mid-flight enter a
non-terminal **`faulted`** status with a configurable grace period
(default 5 minutes) during which:

- The robot can recover and resume — the order transitions back to
  `in_transit` without operator intervention
- The bin claim and lineside slot reservation are preserved
- The HMI shows a faulted toast with a retry affordance

After the grace period, the order transitions to terminal `failed` and
the claim is released as before. Auto-return is still disabled pending
redesign (see 2026-04-14).

### UOP Authority Flip

Bins are now the source of truth for `remaining_uop`, with Edge as the
sole writer; Core's reconciler that re-derived UOP from style claims is
deleted. This eliminates a class of races where Core and Edge would
write conflicting UOP values during a two-robot release.

- `remaining_uop_cached` backfill migration for DBs that skipped the
  earlier rebuild
- Race-safe upserts on `process_node_runtime_states`,
  `style_node_claims`, and `node_lineside_bucket` so concurrent edge
  writes can't lose updates
- Two-robot release: zero runtime UOP at the click and stash a durable
  sibling pointer so the partner order can resolve cleanly even after
  process restart

### Edge Admin

- New **Lineside Buckets** page lets engineering override slot UOP
  capacity from the admin UI without a YAML edit + restart
- Operator HMI keypad and release prompt scaled for 7" displays;
  failure toast now carries the actual error string instead of a
  generic "order failed"

### Logging & CI

- Engine release errors and core dispatch errors routed through the
  ring-buffer logger so they show up in the Debug Log UI
- CI workflow YAML rewritten — earlier patch had introduced duplicate
  `run:` keys that some YAML parsers accepted silently
- `mkdir build/` before `go test -coverprofile` so the coverage file
  has a directory to land in
- Strip UTF-8 BOMs from Go source files (Go 1.25 rejects them);
  drop `.out` extension from coverprofile paths

## 2026-05-03 — Coverage Reporting, Fuzz Targets, Changeover Floor Stability

### Tests

- **CI coverage reporting**: `go test -coverprofile` plumbed through
  for `protocol/`, `shingo-core/`, and `shingo-edge/` with merged
  HTML output uploaded as a workflow artifact
- **Store coverage migration**: facade-level tests on the outer
  `store/` package moved into the per-aggregate sub-packages
  (`bins/`, `nodes/`, `orders/`, `payloads/`) where the logic actually
  lives. Coverage numbers are now meaningful per package
- **Protocol fuzz targets**: `Envelope` decode, `OrderRequest`
  validation, and waybill round-trip added under `go test -fuzz`
- **Event-driven test polling**: replaced 23 `time.Sleep` calls in
  fulfillment, dispatch, and engine tests with channel-based waits

### Changeover

- **Floor stability bundle**: mode-aware step builders (single-robot
  vs two-robot vs sequential vs press-index produce) consolidated;
  fan-out for paired produce nodes routed through the same builder
  so phase-2 reuses phase-1's logic instead of diverging

## 2026-05-02 — UOP Bin-as-Truth Refactor

Foundational refactor that the 2026-05-04 authority flip builds on.
Bins gain `remaining_uop_cached` so HMI rendering and dispatch
decisions read from a single column, and the legacy "compute UOP from
claim + manifest" path is removed from hot paths. Engine still
recomputes on bin load and on operator confirm; everything else trusts
the cached value.

## 2026-05-01 — Bin Transit State, Forklift-Scale Bin Loader, Migration Hardening

### Bin Transit State (Phases 1-5)

`bin_transit_state` enum and column on `bins` give Core explicit
visibility into where a bin is in its delivery cycle:

```
parked → claimed → en_route → at_destination → released
```

- Replaces ad-hoc inference from `orders.status` + `bins.node_id`
- Plumbed through dispatch, completion, fulfillment scanner, and
  audit log
- UOP audit at each transition flags drift before it reaches the
  operator
- Phases 1-2 add the column and writers; phases 3-4 migrate
  read-path consumers; phase 5 removes the legacy inference paths

### Operator Station

- **Forklift-scale bin-loader board**: redesigned for tablet-on-fork
  use — large tap targets, demand grouped by payload, manifest
  preview before the operator commits
- **Manual request affordance**: operator can dispatch a load order
  from the loader UI without going through the material page

### Migrations

- **Per-version transactions**: each migration runs in its own `BEGIN
  ... COMMIT` so a partial failure leaves the DB at the previous
  version, not halfway through
- **Self-heal on startup**: detect and repair migrations that left
  broken state from before the per-version-tx wrapping (orphan FKs,
  half-renamed columns)

### Dispatch

- Complex orders now bypass the dropoff-capacity gate. The gate was
  designed for simple deliveries; complex orders manage their own
  capacity through the multi-step plan and were getting blocked
  spuriously when the destination was momentarily full mid-cycle

## 2026-04-30 — Press-Index, Typed Constants, Two-Robot Release Polish

### Press-Index 3-Position Layout

Optional second paired core node for press cells with a back-side
material position. Configured via `second_paired_core_node` on the
press's claim — when set, produce-empty dispatches use the back node
as the secondary destination.

- Auto-provision back-node claim row when paired field is set
- Pairing UI clarifies primary vs secondary on the processes page
- Produce-node Request Empty button respects pairing

### Two-Robot Release Polish

- **Multi-payload bins**: release auto-picks the correct bin from the
  manifest when the lane has bins of different payloads
- **Press-index two-robot**: release works before the runtime
  `claim_id` is stamped (was failing on the first cycle of the day)
- **In-transit acceptance**: dispatch now accepts release on
  `in_transit` orders so two-robot fan-out can release Robot B while
  Robot A is still moving
- **Multi-step produce-empty**: uses the same swap-dispatch builder
  as resupply; produce-empty button stays visible across all produce
  modes; `/request` no longer fires from produce nodes
- **Manual\_swap demand cards**: clickable while idle (were greyed
  out unless an order was already active)

### Typed Constants Migration

The `string` literals for order status, order type, claim role, and
bin status across `engine/`, `dispatch/`, and `www/` migrated to typed
constants from `shingocore/types`. Catches typos at compile time and
makes call-site dispatch on these values exhaustive-checkable.

- `Plan/Apply` pattern formalized for swap dispatch — `Plan` returns
  the resolved step set without side effects, `Apply` commits
- Shared `SwapDispatch` collapses single-robot, two-robot, sequential,
  and press-index produce paths to one entry point
- Tests aligned with the typed signatures (no behavior change)

### Other

- **Complex orders**: `ProcessNode` threaded through so multi-bin
  manifest sync writes against the correct edge process
- **Static cache**: `/static/*` cache busted on every edge restart
  via ETag — operators no longer need a hard refresh after a deploy
- **Stale operator\_station\_id**: cleared when a process is reassigned;
  stops the phantom-claimed greying on the processes page
- **Release UX**: explicit labels, always-enabled PULL PARTS button,
  primary group surfaced first

## 2026-04-29 — Side-Cycle Relocation, Replenishment, Two-Robot Press-Index

### Side-Cycle Trigger Relocation

The L1 trigger (inbound side-cycle on manual\_swap) moved from
`OrderDelivered` to release-click; U1 and U2 wired symmetrically. This
makes side-cycle a release-time decision instead of a delivery-time
one, which lets the operator abort the cycle by not clicking release.

### Replenishment

- **OrderDelivered UOP snapshot**: bin's `uop_remaining` carried in
  the delivery envelope so replenishment uses the actual arrived UOP
  instead of looking it up after the fact
- `arrivedBinUOP` field dropped — the snapshot replaces it

### New Mode

- **`two_robot_press_index`**: produce swap mode for press cells —
  Robot A pre-stages new material at the back while Robot B is still
  finishing the previous cycle on the front

### Other

- `NODE_GROUP` and its direct physical children now treated uniformly
  as storage slots by the dispatcher (closes a gap where a group node
  was rejected as a destination)
- `called_by` defaulted on release handlers; empty-body confirms
  tolerated instead of 400'd

## 2026-04-28 — V2 Side-Cycle Hardening, Floor-Jam Guards, JS/HTML Cleanup

### V2 Side-Cycle Direction (Phases 2-3)

- **`payload_bin_types` advisory enforcement**: dispatch consults the
  payload→bin-type mapping to filter source bins, but operators can
  still override via manual claim. Was strict-enforce; advisory matches
  how the plant actually uses it
- **Side-cycle hardening**: L2 outbound move always auto-confirms
  (was sometimes left at `delivered` waiting for an operator click
  that wasn't coming); LOAD reliably confirms inbound L1; recovery
  path for cancelled-leg in two-robot swap so the surviving leg
  doesn't deadlock

### Floor-Jam Guards

- **Manual\_swap stale-bundle guard**: refuses to dispatch a swap
  bundle when one of the underlying orders has gone terminal between
  bundle build and dispatch
- **Half-built complex order guard**: orders with missing manifest
  or unresolved source no longer reach the dispatcher; rejected at
  build time with a specific error
- **Retired bins exclusion**: operational queries (source-finder,
  empty-finder, manifest verify) skip bins where `retired_at IS NOT
  NULL` — was leading to phantom bin matches on retired QR codes
- **Direct-order source claim**: claims source bin so completion can
  update `bins.node_id` (was leaving direct orders' bins at the old
  location)

### Operator HMI

- **Bin-loader confirm-on-load**: confirm fires on the `bin.load`
  receipt, not on a separate operator click
- **UOP-on-delivery**: UOP populates from the delivery envelope as
  soon as the robot reports delivered
- **Release prompt rework**: branching disposition (deplete/return/
  scrap), no flicker on state transitions, qty autofills from the
  manifest
- **Keypad z-index** raised so it actually appears on top of the
  release prompt (was being hidden under)

### JS/HTML Cleanup Phase 1

- `h/el/apiGet` helpers added to the shared utilities module; raw
  `fetch()` calls migrated to use them. Silent `.catch()` blocks now
  surface to the toast logger
- `operator.js` split into ES modules (one per workflow)
- `nodes.js` split into overview / detail / supermarket
- Bins/orders table loops converted from string concat to `h\`\``
  templates; nested ternaries wrapped to prevent double-escape
- `location.reload` removed from bulk-action and node-bin SSE paths;
  now diff the response and patch the DOM
- **Dead code deletion**: `ConfirmNodeManifest` stack,
  `forceTransitionOrder`, `DeleteLinesideBucket`, `operator-canvas/`
  — all unused after the v2 side-cycle landing

### Release Flow

- **Shared validator**: per-mode validation collapsed to one
  `validateRelease` taking the resolved swap mode
- **`ChangeoverWait` guard**: refuses release while the changeover is
  in its wait phase (was racing the changeover sequencer)
- **`called_by` required**: release endpoints now require the
  `called_by` field; two-robot UI bypass that wasn't sending it fixed
- **Fall-through logging**: unhandled release dispositions logged at
  WARN with the resolved mode so they don't disappear silently

## 2026-04-27 — Architecture Finishing Pass, Side-Cycle Bin Loader

### Architecture (`finish-architecture`)

- **Depguard ratchet drained**: package import rules tightened to
  match the post-Stages-1-9 architecture; remaining violations all
  fixed (or moved to a tracked exception list)
- **Edge wiring split**: `cmd/shingoedge/wiring.go` decomposed by
  concern matching the Stage 7 / 2C pattern in core
- **CI hygiene**: lint action upgraded; four state-machine test
  failures from Stage 10 reconciled; workflow files normalized

### Side-Cycle Bin Loader

Side-cycle replaces the kanban auto-request that was being deleted in
the same commit. The loader's UI now surfaces line demand for orders
sourcing **from** this loader node, so the forklift operator sees what
the line needs without the ops team scheduling a kanban request first.

- **Kanban auto-request deleted** entirely — side-cycle subsumes it
- **Source-finder destination exclusion**: a node never appears as a
  source for an order whose destination is itself
- **Kanban spam block**: when the destination already has a bin,
  no auto-request fires (was looping on transient empty windows)

### Release Flow Simplification

- **Two-robot release**: gate on Robot B staged, then fan out to both;
  removes the Robot-A-first ordering that was creating stuck cycles
- **AutoConfirm split**: extended from system-initiated paths to
  operator-initiated swap and removal
- **Two-robot teleport fix**: bin no longer "teleports" to destination
  on a late confirm — confirm is now bound to the actual delivery
  receipt, not the order status

### Other

- **`FindEmptyCompatible`**: dropped the `manifest_confirmed` check
  (too strict; rejected legitimate empties on confirm-races) and
  made the empty-bin check NULL-safe
- **`order_bins` junction**: only deleted on terminal status —
  was deleting mid-flight on cancel-then-recover paths
- **Edge `RequestEmptyBin` / `LoadBin`**: occupancy and demand guards
  removed for `manual_swap` nodes (those nodes legitimately accept
  multiple loads in a row)
- **Battery level unit**: SEER returns 0.0–1.0; UI was showing
  decimals. Multiplied by 100 at the mapper for 0–100 display

## 2026-04-26 — Stage 10: Order State Machine + Phase 6/7 Architectural Parity

### Stage 10 — Typed Lifecycle Facade

The order state machine — previously scattered across `engine/orders.go`,
`engine/wiring.go`, and `dispatch/` — moved behind a typed `Lifecycle`
facade. Every transition (`pending → sourcing → ... → confirmed`) now
runs through one entry point with explicit pre/post-conditions.

- Compile-time exhaustive switching on `Status` enum catches missed
  transitions
- `Lifecycle.Transition(orderID, from, to)` returns a structured error
  on invalid transitions instead of silent no-ops
- All eight `HandleOrder*` methods on `CoreHandler` and the
  fulfillment scanner refactored to call the facade
- `scanner_test.go` gains a stub `Lifecycle` so the fake-backed test
  battery still compiles tag-free

### Phase 6 + Phase 7 — Architectural Parity

Cross-module parity work landed alongside Stage 10:

- Edge module gains the same narrow-interface treatment Core got in
  Stages 5-9 (resolver, dispatcher, scenesync now defined as
  consumer-side interfaces in the calling package)
- Wiring layout aligned across core and edge — same per-concern file
  decomposition, same naming conventions
- Phase 7 begins extracting edge's order lifecycle into a sibling
  `edge/orders/lifecycle.go` matching core's structure

## 2026-04-25 — Two-Robot Release Consolidation

Consolidated release timing across the two-robot fan-out so Robot A
and Robot B receive their release in a single transaction. Adds a
**stuck-cycle guard** that fires after a configurable timeout if the
fan-out hangs (e.g., one robot reports delivered but the other never
does), and an **admin disposition** path so engineering can manually
unstick a cycle from the diagnostics page without restarting Core.

## 2026-04-24 — Dispatch Skip-Reason Visibility

`d.dbg`/`s.dbg` ring-buffer routing for the skip-reason logs added in
2026-04-23 (`ef002a2`) so they appear in the Debug Log UI instead of
only in stdout. Operators investigating "why didn't this dispatch"
can now answer the question from the web UI.

## 2026-04-23 — Two-Robot Release Manifest, Supply-Bin Guard

### Bug Fixes

- **Two-robot manifest stuck at `SMN_003`**: bin lookup fallback for
  manifest sync, plus a runtime-reset guard that prevents the manifest
  from being re-bound mid-cycle
- **Per-order safety net**: tracks supply-bin status per order so
  partial failures roll back the affected order without dragging
  siblings down. `manifest_sync_failed` rolls back atomically;
  `CalledBy` audit preserved on partial-failure paths
- **Late-bind bin manifest**: bind manifest at operator release click
  rather than at order creation. Lets operators reassign payload right
  up to release without re-creating the order
- **Skip-reason diagnostics**: `claimComplexBins` and the planning
  move-bin loop log specific reasons when they decline to dispatch

## 2026-04-22 — Lineside Inventory, Multi-Payload Manual\_Swap

### Features

- **Lineside inventory + reset-on-release**: edge tracks inventory
  per lineside slot and resets the bucket counter on release. Engineering
  can audit consumption per slot per shift instead of inferring from
  delivery counts
- **Reporting-point sync on process update**: when a process's PLC
  reporting point changes, the heartbeat tag/PLC binding updates without
  a service restart

### UI

- **Manual\_swap node highlighting**: every node with active demand
  highlights on the operator board (was only highlighting the most
  recent one)
- **HMI style chip + bigger payload labels**: style chip rendered on
  every operator tile; payload code label bumped to a readable size
  on 7" displays

### Bug Fixes

- **Multi-payload manual\_swap loading**: unblocked — the loader was
  rejecting loads where the payload didn't match the lineside default,
  even when the claim's `allowed_payload_codes` listed it

## 2026-04-21 — Architecture Overhaul Phases 1-5, Demand Sync

### Architecture Overhaul (Phases 1-5)

Cross-module structural pass laying the groundwork for Stage 10
(2026-04-26). Phases 1-5 cover module-boundary cleanup, naming
alignment between core/edge/protocol, depguard rule introduction,
and initial extraction of shared lifecycle types. The loader-demand
fix landed in the same commit — it was the motivating use case.

### Demand Registry

- **`core/demand_registry`**: reaps stale entries when an edge stops
  reporting; no more phantom demand from a powered-off station
- **Edge claim sync**: pushes claim upserts/deletes to Core on UI
  action so Core's view of "what does this edge want" is current
  without waiting for the next heartbeat

### Bug Fixes

- **Operator board active-status set**: covers the full set of active
  statuses (was missing `queued` and `faulted`); unstick path for move
  orders that landed in a transitional state at restart
- **Cleared bins at concrete lineside nodes**: dispatch now allows
  a cleared bin (zero UOP, no manifest) to satisfy a manual\_swap
  request at a concrete lineside node — was rejecting because of an
  empty-payload check that didn't fire for NGRP children
- **Kanban guardrail**: consume claims on non-LANE nodes are rejected
  at claim build time (kanban only makes sense for lane storage)

## 2026-04-19 — Two-Robot Single-Click Release, Toolchain Downgrade

### Two-Robot Coordinated Release

Single click from the operator releases both robots in a two-robot
swap. Previously each robot needed its own release click — confusing
when the cycle was visually one swap. The handler now identifies
sibling orders via the durable claim pointer and fans out the release.

### Toolchain

- **Go 1.26.2 → 1.25.0**: rolled back across `protocol/`,
  `shingo-core/`, and `shingo-edge/`. The 1.26.2 bump (briefly
  on `main`) was rejecting BOM-prefixed source files that Windows
  editors had silently introduced. Coupled with the BOM-strip script
  this leaves the toolchain stable at 1.25
- **Protocol BOM strip**: `auth/password.go` had a UTF-8 BOM that
  blocked the build under 1.26 — stripped explicitly

## 2026-04-18 — E-Maint Robot Telemetry Stub

In addition to the Stages 1-9 architecture refactor (below), the
following landed late on 2026-04-18:

- **`/api/telemetry/e-maint`** + **`/download`** endpoints expose
  per-robot maintenance telemetry (odometer, runtime, jack/lift,
  voltage, current, controller state)
- **`RbkReport`** + **`RobotBasicInfo`** extended with the new fields;
  `fleet.RobotStatus` gains 14 telemetry fields and the SEER mapper
  populates them
- **E-Maint tab** added to the Diagnostics page with a download
  button — populated from the in-memory robot cache
- Tab active-state cleanup for `data-tab` buttons (the previous
  code only handled `data-target` buttons)

## 2026-04-18 — Architecture Refactor: Stages 1-9 (shingo-core)

Nine-stage architectural cleanup of `shingo-core`, landed as one squashed
commit on `main` after being developed on `refactor/shingo-architecture`.
Public API surface and wire protocol are preserved; the changes are
internal organization, test structure, and documentation.

The subsections below walk through each stage in the order it landed.

### Stage 1 — `engine.DB()` → Named Engine Methods (www)

`www/` handlers no longer receive a `*store.DB` handle through
`engine.DB()` and pick arbitrary queries off it. The `EngineAccess`
interface gains 116 named, single-purpose query methods; handlers call
`h.engine.ListBins()` / `h.engine.GetNode()` / etc. directly.

- `engine/engine_db_methods.go` — 116 alphabetized one-line delegates
  to `*store.DB` keep the implementation thin
- `DB()` removed from `EngineAccess`; still exists as a concrete
  method on `*engine.Engine` for `router.go`'s `ensureDefaultAdmin`
  callsite
- `www/nodes_page_data.go` takes a narrow `nodesPageDataStore`
  interface so `getNodesPageData` remains testable with a fake
- Prep commit characterized the seven handler write-paths affected
  (nodegroup, orders, test_orders, nodes, demand, auth, bins-gaps)
  with HTTP-surface tests so Stage 1 could run green without touching
  test expectations

### Stage 2A — `domain/` Package

Pure data types lifted out of the store aggregates into a new
persistence-free `shingocore/domain/` package so higher layers
(dispatch, engine, service, www) can reference the shapes without
pulling in `database/sql`.

Types moved: `Bin` (+`Manifest`, `ManifestEntry`, `Bin.ParseManifest`),
`BinType`, `Node`, `NodeType`, `NodeProperty`, `Payload`,
`PayloadManifestItem`, `Order`, `OrderBin`. Each store sub-package
retains the local name via a one-line `type Bin = domain.Bin` alias so
every existing call site compiles untouched. Non-pure types
(`NodeTileState` view projection, `orders.History`, `orders.Filter`,
`orders.BinArrivalInstruction`, scan helpers) stay in place.

### Stage 2C — `wiring.go` Split by Functional Concern

`engine/wiring.go` (~1050 LOC spanning fleet status mapping, completion,
staging, auto-return, kanban, telemetry, and count-group dispatch) split
into per-concern sibling files. The master `wireEventHandlers` registry
and `sendToEdge` helper stay in `wiring.go` so the reactive contract
still reads top-to-bottom in one place.

- `wiring_vendor_status.go`, `wiring_completion.go`,
  `wiring_staging.go`, `wiring_auto_return.go`, `wiring_kanban.go`,
  `wiring_telemetry.go`, `wiring_count_group.go` each track a single
  concern
- No symbol visibility or signature changes; no call-site edits
- New filenames align with existing per-concern `_test.go` files

### Stage 2D — `store/` Split by Aggregate

Flat `store/` package decomposed into four aggregate-scoped sub-packages
(`bins/`, `nodes/`, `orders/`, `payloads/`). The outer `store/` keeps
the full `*store.DB` method surface via type aliases and one-line
delegate methods; sub-packages own free-function persistence APIs
taking `*sql.DB` and are zero-dep on each other.

Cross-aggregate composition methods (`SetBinManifestFromTemplate`,
`FindStorageDestination`, `GetEffectiveBinTypes`, `GetEffectivePayloads`,
`GetGroupLayout`, `FindSourceBinInLane`, `FindBuriedBin`,
`FindOldestBuriedBin`, `ApplyMultiBinArrival`, `CreateCompoundChildren`,
`FailOrderAtomic`, `CancelOrderAtomic`, `ListOrdersByBin`,
`UpdateOrderBinID`) stay at the outer `store/` level. Public API
unchanged; no caller-visible renames.

### Stage 3 — BinService Pilot

New `shingocore/service/bin_service.go` — the first service-layer
migration. Validation and mutation logic moves out of
`www/handlers_bins.go` into `BinService`. Audit logging and event
emission stay at the handler layer (same boundary `BinManifestService`
established).

Covers: `Create`, `CreateBatch` (one-bin-per-physical-node plus
multi-bin-at-synthetic invariants), `Move`, `LoadPayload`, `Lock`,
`ChangeStatus`, `Release`, `Unlock`, `RecordCount` (with discrepancy
signal), `AddNote`, `Update`. `handleBinCreate` delegates to
`CreateBatch`; `httpStatusForCreate` maps service error messages back
to pre-refactor status codes (400 node-not-found, 409 occupancy, 500
otherwise).

### Stage 4 — OrderService + NodeService

Follows the BinService pilot for the remaining mutating handlers.

- `service/order_service.go` — `Create`, `UpdateStatus`,
  `UpdateVendor`, `SetPriority` (composes fleet + DB, returns the
  resolved order), `ClaimBin`, `UnclaimBin`. `apiSetOrderPriority`,
  `submitSpotSendTo`, `submitSpotRetrieveSpecific` now delegate
  through `h.engine.OrderService()`
- `service/node_service.go` — `ApplyAssignments` consolidates the
  4-step "station mode + stations + bin-type mode + bin types" flow
  previously duplicated between `handleNodeCreate` and
  `handleNodeUpdate`. Sub-step errors are joined and returned; audit
  / event emission stay at the handler layer

### Stage 5 — `dispatch/binresolver/` Extraction

Slot-picking algorithms separated from the dispatch state-machine.
Files moved verbatim into `dispatch/binresolver/`:

- `resolver.go`, `group_resolver.go`, `lane_lock.go`, `helpers.go`
- `dispatch/binresolver_aliases.go` re-exports the public surface via
  type aliases (so `*dispatch.BuriedError` and
  `*binresolver.BuriedError` are the same type — `errors.As` at call
  sites keeps working without edits), var forwards (`ErrBuried`,
  `NewLaneLock`), and const forwards (`RetrieveFIFO/COST/FAVL`,
  `StoreLKND/DPTH`)
- Private helpers (`isBinAvailableForRetrieve`, `storageCandidate`,
  `bestStorageCandidate`, `classifyEmptyGroup`, `binTypeAllowed`,
  `getGroupAlgorithm`, `resolveRetrieve`, `resolveStore`) stay
  internal to the sub-package

Stage 5 gate: `binresolver/store.go` narrow Store interface (14
methods), fake-backed unit tests across every strategy — FIFO, COST,
FAVL, LKND, DPTH, and `classifyEmptyGroup`. 19 new tests, zero DB
fixtures required.

### Stage 6 — `material/` Package (CMS Transactions)

Pure boundary-walk and transaction-builder logic extracted from
`engine/cms_transactions.go` into a top-level `shingocore/material`
sub-package following the Stage 5 pattern. The engine wrapper keeps
`FindCMSBoundary`, `RecordMovementTransactions`,
`RecordCorrectionTransactions` so the two call sites
(`engine/wiring.go` and `engine/corrections.go`) don't move.

- Narrow 4-method `Store` interface + compile-time assertion that
  `*store.DB` satisfies it
- Package functions take a Store and return values/errors; no
  persistence, no event emission, no engine coupling
- Hand-rolled fakeStore drives unit tests without a database
- Engine wrapper owns `CreateCMSTransactions` + `EventCMSTransaction`
  emission, logs build errors, preserves the nil-only fallback
- `cms_transactions.go` drops from 246 → 74 LOC
- Coverage: `go test ./material/... -cover` = 89.6%

### Stage 7 — `engine.go` Split + `fulfillment/` Extraction

Two changes in one atomic commit — the last engine-package
reorganization before the Stage 8 protocol inbox work.

`engine.go` (682 LOC) split into the struct file plus seven siblings,
mirroring the Stage 2C `wiring` pattern:

- `engine.go` — struct + `New` + `dbg` + robot-cache getters
- `engine_lifecycle.go` — `Start`/`Stop`/`loadActiveOrders`
- `engine_accessors.go` — one-liner getters + `SetCountGroupRunner`
- `engine_messaging.go` — `SendToEdge`/`SendDataToEdge`/
  `RunFulfillmentScan`
- `engine_connection.go` — `checkConnectionStatus`/
  `connectionHealthLoop`
- `engine_reconfigure.go` — `ReconfigureDatabase`/`Fleet`/
  `CountGroups`/`Messaging`
- `engine_scene_sync.go` — `SyncScenePoints`/`SyncFleetNodes`/
  `UpdateNodeZones`/`SceneSync`
- `engine_background.go` — `robotRefreshLoop`/`stagedBinSweepLoop`

`orderResolver` (fleet.OrderIDResolver adapter) moved into
`adapters.go` alongside `dispatchEmitter` / `pollerEmitter` /
`countGroupEventEmitter`.

`FulfillmentScanner` extracted to `shingocore/fulfillment/`:

- `doc.go`, `store.go` (14-method consumer-side Store interface with
  compile-time assertion), `scanner.go` (`Scanner` renamed from
  `FulfillmentScanner`, `NewScanner`, `Trigger`, `RunOnce`,
  `StartPeriodicSweep`, `Stop`, `scan`, `tryFulfill` — logic
  preserved verbatim)
- Engine struct field `fulfillment *FulfillmentScanner` becomes
  `*fulfillment.Scanner`; `engine_lifecycle.go` calls
  `fulfillment.NewScanner` at Start
- Method names (`Trigger`, `RunOnce`) unchanged so wiring call-sites
  need no edits beyond the field type

Stage 7 gate: fake-backed 12-case `scanner_test.go` covering every
branch of `tryFulfill` that returns false before
`s.dispatcher.DispatchDirect` — cancelled-between-list-and-fetch
fresh-copy re-check, in-flight delivery node blocks dispatch,
destination still parked, empty payload short-circuit,
`retrieve_empty` zone preference derivation, `ClaimBin` failure (no
unclaim / no status change), `GetNode`/`GetNodeByDotName` post-claim
failure triggering `UnclaimOrderBins` (and for `GetNodeByDotName`
also `StatusQueued` re-queue), `Trigger` coalescing during an
in-progress scan, `StartPeriodicSweep`/`Stop` lifecycle.

### Stage 8 — InboxDedup Decorator

Three-line `shouldProcessInbound` guard that was copy-pasted across
all eight `HandleOrder*` methods in `CoreHandler` collapses into a
single `protocol.MessageHandler` decorator (`InboxDedup`) wired into
the composition root between the ingestor and `CoreHandler`.

- `messaging/inbox_dedup.go` — embeds `protocol.NoOpHandler` for
  forward compatibility, overrides all 16 interface methods
  explicitly to stay transparent, gates the 8 Edge→Core order methods
  via `RecordInboundMessage`, passes `HandleData` + the 7 Core→Edge
  replies through ungated (matching pre-decorator behavior where
  only order messages were guarded)
- `core_handler.go` loses `shouldProcessInbound` and 24 lines of
  per-method guard duplication
- `main.go` wraps `coreHandler` with `messaging.NewInboxDedup` before
  passing it to `protocol.NewIngestor`
- Existing dedup tests updated to exercise the new path;
  `inbox_dedup_test.go` adds unit coverage for `HandleData`
  passthrough and the empty-envelope-ID bypass
- No changes to `protocol/`, `store/`, or any other package — dedup
  behavior and wire format are unchanged

### Stage 9 — Narrow Interfaces, Scenesync Extraction, Docker-Gated Tests

#### Consumer-Side Narrow Interfaces

Two packages now hold their collaborators behind narrow interfaces
instead of concrete dispatcher types. Structural typing means
`*dispatch.Dispatcher` and `*dispatch.DefaultResolver` satisfy them
automatically — the engine wiring in `cmd/shingocore/main.go` is
unchanged.

- **`fulfillment.Dispatcher`** (1 method) and
  **`fulfillment.Resolver`** (1 method) — declared in
  `fulfillment/dispatcher.go`, held on `Scanner` fields. Lets
  `scanner_test.go` stub one-method fakes, closing the coverage gap
  flagged in the Stage 7 scope note.
- **`messaging.Dispatcher`** (8 methods covering all order-channel
  handlers) — declared in `messaging/dispatcher.go`, held by
  `CoreHandler`. Removes the `messaging → dispatch` import edge so
  dispatch can't leak transport assumptions back up to the handler.
- **Compile-time assertions**
  (`var _ Dispatcher = (*dispatch.Dispatcher)(nil)`) catch drift
  before any caller-site build failure.

#### Scenesync Package Extraction

New `shingocore/scenesync` package owns fleet→DB scene reconciliation
logic. Exposes a narrow 8-method `Store` interface
(`DeleteScenePointsByArea`, `UpsertScenePoint`, `GetNodeTypeByCode`,
`GetNodeByName`, `CreateNode`, `UpdateNode`, `ListNodes`,
`DeleteNode`) plus `LogFn`/`NodeChangeFn` callback types.

`engine/engine_scene_sync.go` is reduced to a thin shim — holds the
`sceneSyncing` atomic, wires `emitNodeChange` to `Events.Emit`, and
delegates `SyncScenePoints`/`SyncFleetNodes`/`UpdateNodeZones`/
`SceneSync` to the new package. External API is byte-for-byte
identical; `www/handlers_nodes.go` and `engine/engine_connection.go`
see no change.

Go named-type identity requires explicit `scenesync.LogFn(e.logFn)`
conversions at the four call sites (both types are
`func(format string, args ...any)` but are nominally distinct).

#### Protocol: RawHeader.Src

`protocol/envelope.go` `RawHeader` gains a `Src Address` field
alongside `Dst`. Wire format unchanged (json tag `src` matches
`Envelope.Src`). Lets routing code identify the sender from the
minimal decode without a full payload parse — necessary for inbound
dedup + rate-limit work that can't afford to decode every message.

#### Test Structure

- **`dispatch/integration_test.go` → `end_to_end_test.go`**. The
  tests drive the dispatcher through complete retrieve/move/store/
  cancel/redirect/synthetic/reshuffle lifecycles — that is end-to-end
  behavior, not two subsystems interacting, so "integration" was
  the wrong word.
- **`engine/engine_test.go` split three ways.** Shared scaffolding
  moved to `engine_testhelpers_test.go` (`testDB`, `setupTestData`,
  `createTestBinAtNode`, `testEnvelope`, `newTestEngine`). The six
  `TestRegression_*` tests moved to `engine_regression_test.go`.
  `engine_test.go` itself keeps only top-level behavior tests.

#### `//go:build docker` Gating

39 test files across `dispatch/`, `engine/`, `messaging/`, `service/`,
`store/`, `www/`, and `shingo-edge/store/` now carry
`//go:build docker` on the first line. `go test ./...` on a bare
machine compiles and runs only the unit + fake-backed tests; the
Postgres-backed tests are excluded from the build.
`go test -tags=docker ./...` pulls them back in.

- `shingo-core/Makefile`: `test` target unchanged; new `test-all`
  target runs `-tags=docker`
- `shingo-core/README.md` "Build Targets" section documents both plus
  the rationale (fake-backed contract coverage stays tag-free and
  runs on every push)
- Tag-free fake-backed coverage retained in `material/`,
  `fulfillment/`, `dispatch/binresolver/`

#### Documentation

- **architecture.md** — Package Layout rewritten to remove phantom
  `nodestate/` and `debuglog/` entries and add the real sub-packages
  that have landed since the last pass: `countgroup/`, `fulfillment/`,
  `material/`, `scenesync/`, `service/`, `internal/testdb/`,
  `fleet/seerrds/`, `fleet/simulator/`,
  `store/{bins,nodes,orders,payloads}/`. Message Ingest Pipeline
  diagram now shows the `InboxDedup` decorator sitting between the
  protocol Ingestor and `CoreHandler`.
- **test-catalog.md** — renamed dispatch integration section, added
  `engine_testhelpers_test.go` and `engine_regression_test.go`
  sections, added Docker-gating note to the preamble, updated TC
  numbering backlog references.
- **fleet-simulator/architecture.md** and
  **fleet-simulator/complex-orders.md** — updated references to the
  renamed `dispatch/end_to_end_test.go`.

### Validation

Builds and tests green across `protocol/`, `shingo-core/`, and
`shingo-edge/` with and without `-tags=docker` (18 commands, all
pass). Includes two validation-phase fixups: explicit
`scenesync.LogFn` conversions in `engine_scene_sync.go`, and
`//go:build docker` headers on `www/auth_test.go` and
`www/handlers_demand_test.go` (both reference `testHandlers` /
`postJSON` defined in gated files).

## 2026-04-17 — Toolchain Bump & Dead-Symbol Flagging

### Toolchain

- **Go 1.25.0 across all modules**: `protocol/`, `shingo-core/`, and
  `shingo-edge/` all moved to Go 1.25.0, clearing the version drift
  between the three modules. CI workflows updated to match.
- **x/crypto v0.48.0 alignment**: pinned uniformly across all modules;
  edge's `x/net`, `x/sys`, `x/text` indirects aligned with
  shingo-core's current pins.
- **Drift-check test**: new `protocol/version_test.go` parses all
  three `go.mod` files and fails if Go versions or `x/crypto`
  versions drift apart again (pulls `golang.org/x/mod` v0.27.0 as a
  test dependency).
- **go mod tidy sweep**: follow-up to regenerate `go.sum` entries
  matching the new version pins.

### Code Quality

- **Dead-symbol flagging**: six callsite-free symbols annotated with
  `TODO(dead-code)` markers for a later pruning pass —
  `core/dispatch.coreAddress()`, `edge/backup.hashBytes()`,
  `edge/engine.BuildDeliverSteps()`,
  `edge/messaging.EdgeHandler.SetNodeStateHandler()`,
  `edge/messaging.Heartbeater.RequestNodeState()`,
  `edge/orders.Manager.forceTransitionOrder()`. Kept (not removed)
  in case external wiring, reflection, or WIP branches rely on them.

## 2026-04-16 — Web UI Configuration: Traffic Page & Fire Alarm Toggle

### Features

- **Traffic page (core + edge)**: operators can configure
  heartbeat tag/PLC and zone-to-PLC bindings and advanced-zone
  count-groups from a Traffic tab on each service's Admin UI instead
  of editing YAML manually.
  - **Edge** — PLC names auto-populate from WarLink discovery; binds
    are written straight to `shingoedge.yaml`.
  - **Core** — count-group add / remove / toggle is now a UI action;
    changes persist to `shingocore.yaml` and hot-reload the
    count-group Runner without a service restart.
- **Fire alarm toggle on config page**: enables/disables the fire
  alarm feature and sets the auto-resume default from the web UI
  instead of requiring a YAML edit and service restart.

## 2026-04-15 — Count-Group Light Alerts & Fire Alarm Pass-Through

### Count-Group Advanced-Zone Light Alerts

Real-time safety lighting for advanced zones (crosswalks, forklift aisles). Core polls RDS `/robotsInCountGroup` per configured group and emits Kafka commands that Edge translates into PLC tag writes via WarLink. Designed as a safety-adjacent polling loop with asymmetric hysteresis — ON commits faster than OFF to bias toward caution.

- **Configurable per-group polling** with dedicated RDS client and sub-second poll interval (default 500ms)
- **N-of-M hysteresis thresholds**: `on_threshold` (2) and `off_threshold` (3) prevent flicker from transient sensor readings
- **Fail-safe timeout**: forces lights ON after sustained RDS communication failure (default 5s)
- **Stale-group warnings**: escalating log levels when a group never reports occupied (WARN at 5m, ERROR at 30m)
- **Audit trail**: all transitions and fail-safe activations logged to the audit table
- **Feature gate**: empty `groups` list = feature disabled; no polling goroutine started

### Fire Alarm Pass-Through

Feature-gated fire alarm control on the diagnostics admin page. Core relays activate/clear commands to RDS via `/isFire` and `/fireOperations` — RDS owns all robot logic (stop, evacuate, resume). Core is only the communicator; the upgrade path is automating the trigger via a plant-side input (PLC, building alarm system).

- **Two API endpoints** (protected): `GET /api/fire-alarm/status`, `POST /api/fire-alarm/trigger`
- **Optional interface pattern**: `fleet.FireAlarmController` with adapter delegation — same architecture as `RobotLister`, `VendorProxy`, etc.
- **SSE broadcast**: real-time `fire-alarm` events push state changes to all connected browsers
- **Auto-resume checkbox**: when checked, robots resume navigation automatically on alarm clear without manual RDS intervention
- **Confirm dialogs** on both activate and clear to prevent accidental triggers
- **Audit trail**: every activate/clear logged with actor, timestamp, and auto-resume setting
- **Config gate**: `fire_alarm.enabled: false` hides the UI tab and returns 404 on API calls

### Bug Fixes

- **ReadTag name collision**: resolved naming conflict in count-group test that shadowed the WarLink ReadTag method
- **Nil fleet test panic**: fixed test setup that panicked when fleet backend was nil during count-group wiring tests

### Code Quality

- **Trailing newline cleanup**: fixed 13 files across core and edge missing POSIX trailing newlines

## 2026-04-14 — Order Failure Hardening & Bin Protection

### Bug Fixes

- **Occupied node guard**: refuse to create or move bins onto already-occupied physical nodes, preventing bin stacking
- **Staging override removal**: lineside bins are now protected from poaching by staging logic
- **Edge failure notification**: edge is now notified on order failure; broken auto-return disabled pending redesign
- **Manual swap form fixes**: `manual_swap` claim form hides non-applicable fields, pre-seeds allowed payloads, and allowed-payloads picker populates correctly when switching from `simple` to `manual_swap` during edit

### Features

- **Sticky operator toast**: async order failure notifications persist as a toast on the operator HMI instead of disappearing silently

### Tests

- **Auto-return tests**: updated complex order cancel/fail tests for new auto-return behavior
- **TC23b skip**: `TestTC23b_CancelThenMoveBin` skipped while auto-return is disabled
- **Test catalog**: expanded documentation to cover all 262 test functions across 35 files

## 2026-04-13 — Wait Block, Operator UX, Route Visibility & Engine Access Refactor

### Features

- **RDS Wait block**: replaced pre-position dropoff with RDS native Wait block for wait-at-node sequences — eliminates dummy location visits
- **Load bin at node**: operators can load an empty bin already at the node without waiting for a delivery order
- **Step-by-step route display**: mission detail and test orders pages show the full block-by-block route for each order

### Refactoring

- **EngineAccess interface + EventBus migration**: new consumer-side
  `EngineAccess` interface in `www/engine_iface.go` (26 methods),
  `EventBus()` accessor added to `*Engine`, dual-field pattern on
  `Handlers` (`engine EngineAccess` + `eng *engine.Engine`). All 14
  `.Events.Emit()` calls migrated to `.EventBus().Emit()`. Dead
  `Config.Debug` field removed. Sets up the boundary that Stage 1
  (2026-04-18) later narrows further by replacing `engine.DB()`.
- **`handlers_testing.go` → `handlers_test_orders.go`**: pure rename
  for discoverability — the file hosts test-order UI routes, not Go
  test infrastructure. No logic changes.
- **Named constants for magic strings / numbers**: `VendorIDPrefix`
  (dispatch), `PurgeCycleInterval` + `MessageRetentionPeriod`
  (protocol/outbox), `DrainBatchSize` (edge/messaging),
  `MaxBatchRetrieveCount` (edge/www), `rdsProxyTimeout` (core/www).
  Dispatch also upgraded a swallowed error to a log + `%w` wrap.
- **Cross-layer import fix (seerrds)**: mappers now import status
  constants from `shingo/protocol` instead of
  `shingocore/dispatch`, eliminating an adapter-to-orchestration
  import violation.

### Bug Fixes

- **Auto-return safety**: skip auto-return for complex orders when bin position is uncertain after partial completion
- **Same-node move prevention**: refuse to dispatch a move order where source and destination resolve to the same node
- **Single-payload auto-select**: auto-select payload in load bin modal when only one option is available
- **Outbox SELECT missing `sent_at`**: both `ListPendingOutbox` and
  `ListDeadLetterOutbox` were missing `sent_at` in their SELECT and
  Scan, causing row-scan misalignment. Added back.
- **Mount corruption repair**: restored truncated `BuildKeepStagedCombinedSteps` and removed NUL bytes from `complex.go`

## 2026-04-12 — Cross-Module Deduplication & Code Organization

### Refactoring

- **Shared protocol packages**: extracted duplicated types and helpers into shared packages across core, edge, and protocol modules
- **Test assertion helpers**: replaced inline test assertions with `testdb` package helpers across core tests
- **Navigation headers**: added section comments and navigation headers to core and edge router files for IDE navigation

### Bug Fixes

- **Truncated file restoration**: fixed files corrupted during dedup commit

## 2026-04-11 — Structural Refactoring

### Refactoring

- **Characterization tests**: added characterization tests to lock existing behavior before refactoring, then applied 5 structural refactors across core and edge
- **Edge helper extraction**: shared helpers extracted from changeover and demand code to reduce duplication
- **Plan discussion items**: implemented items 2, 5, 7 from architecture review (`2567plandiscussion.md`)

## 2026-04-10 — Bin Loader/Unloader Multi-Order Queue

### Features

- **Multi-order queue with kanban demand**: bin loader and unloader nodes now support queued multi-order workflows with automatic kanban-style demand generation. Orders queue at the node and fulfill in sequence.

### Bug Fixes

- **Plant testing fixes**: bin arrival on delivery, cancel guard, and transition idempotency fixes discovered during plant testing
- **Test infrastructure**: fixed `sql.Open` for testdb admin connections to prevent parallel migration races; completed truncated `TestRegression_CancelEmptyEdgeUUID`

## 2026-04-09 — Bin Loader Stabilization & URL Encoding

### Bug Fixes

- **Bin loader state machine**: fixed wrong UOP count, missing confirm step, and stale HMI state after load operations
- **Auto-confirm**: bin movement auto-confirm now works correctly; added claim-level auto-confirm setting for bin loader nodes
- **Node claim editor**: unlocked core node dropdown and preserved bin_loader-specific fields during edit
- **Staging skip**: bin_loader retrieve-empty orders skip staging step; added HMI refresh safety net
- **Receipt error propagation**: `ConfirmReceipt` errors now propagate correctly; added Kafka publish timeout
- **URL encoding**: fixed URL-encoding for PLC names, tag names, node names (spaces), payload manifest paths, and node children paths in Edge HTTP clients
- **HMI styling**: added background color to load-bin payload picker buttons for visibility

## 2026-04-08 — Database Migration Repairs & Node Guards

### Database

- **Migration v11**: fix `payload_bin_types` FK referencing stale `blueprints` table
- **Migration v12**: fix `payload_manifest` FK, extract shared `fixPayloadFK` helper
- **Migration v13**: fix `node_payloads` FK referencing stale `blueprints` table

### Features

- **Reparent/delete guards**: structural error classification prevents orphaning nodes; Edge notified of structural changes

### Bug Fixes

- **Payload modal crashes**: fixed null response crash in payload edit modal for manifest and bin-type fetches
- **Payload save errors**: payload template save no longer silently discards bin type and manifest errors

## 2026-04-07 — Diagnostics & Move Order Fixes

### Bug Fixes

- **Diagnostics tabs**: fixed tabs not displaying content due to CSS `hide` class conflict with tab switching logic
- **NGRP move orders**: fixed move order from NGRP source not updating bin location (`planMove` was missing group resolution)

## 2026-04-06 — Edge Cancel & Operator HMI Fixes

### Bug Fixes

- **Edge cancel notification**: fixed cancel notification delivery to edge stations
- **HMI cache busting**: added cache-busting to prevent stale operator HMI state after actions
- **CONFIRM button**: fixed operator CONFIRM button not appearing after delivery

## 2026-04-05 — Operator HMI Simplification

### UI/UX

- **HMI streamlining**: removed release-empty and release-partial actions from operator station (rarely used, caused confusion). Added manifest confirm action for bin verification at delivery.

## 2026-04-01 — Changeover Automation & Production Hardening

### Features

- **Changeover automation (Phases 1–5)**: full implementation of automated style changeover with A/B cycling and keep-staged bin handling. Core orchestrates the changeover sequence — abort in-flight orders, A/B cycle material slots, dispatch new material, and confirm completion.
- **A/B cycling UI**: operator HMI shows changeover progress and A/B cycle state
- **Keep-staged wiring**: bins staged at lineside are preserved across changeover when the payload is shared between old and new styles
- **Lot production timestamps**: production timestamped at cell completion for FIFO audit traceability

### Bug Fixes

- **TC-48**: redirect stale steps — fixed redirect patch breaking multi-wait orders
- **TC-51**: compound premature confirm — fixed compound orders confirming before all children complete
- **TC-80**: orphaned bin claims from non-atomic terminal transitions
- **TC-34/TC-49**: complex orders now fail at planning when no bin available (immediate feedback instead of silent dispatch failure)
- **TC-61**: abort pre-existing orders on affected nodes when changeover starts
- **TC-62–67**: bin lifecycle bugs and produce node automation fixes
- **TC-36**: queue order on `claim_failed` instead of permanently failing
- **Bins stuck staged**: fixed bins stuck in staged state after swap order completion
- **Fulfillment scanner race**: fixed flaky `TestFulfillmentScanner_QueueToDispatch` with event-driven scan
- **Receipt confirmation**: hardened receipt confirmation to fix double-writes; added auto-confirm timeout
- **Vehicle pinning**: pin staged order vehicle on release so RDS doesn't re-dispatch to a different robot
- **Post-delivery cancel**: fixed bin lock, SSE refresh, catalog prune, and NGRP sync (TC-68–70)
- **HMI modal buttons**: cycle buttons now shown by order state instead of all at once

### Tests

- **Changeover tests (TC-86–108)**: comprehensive unit tests for `DiffStyleClaims`, A/B produce cycles, and changeover orchestration
- **Production cycle tests (TC-55–60)**: 6 end-to-end production cycle pattern tests with fleet simulator
- **Concurrency tests**: 9 new simulator-based concurrency tests with testing infrastructure
- **Compound order tests**: cascade cancel to compound children + 13 production readiness tests + 9 strengthened existing tests
- **Test infrastructure**: extracted shared `testdb` package, split test files by domain

### Refactoring

- **Receipt confirmation**: hardened against double-writes with auto-confirm timeout
- **Test consolidation**: consolidated test mocks, compound setup helpers, bin helpers, and dispatch handler pattern
- **Wiring optimization**: cached `toStyleID`, extracted A/B predicate, added DB error logging
- **`CanAcceptOrders`/`AbortNodeOrders`**: extracted reusable abstraction for node order management

## 2026-03-30 — FIFO Retrieval & Bin Dispatch Fixes

### Features

- **Strict FIFO retrieval**: enforced across all retrieval paths. Added COST mode for NGRP lanes (closest-optimal-storage-time).
- **Buried bin reshuffle**: `planRetrieveEmpty` now detects buried empty bins and triggers a reshuffle move to make them accessible

### Bug Fixes

- **Complex order bin claims**: bins are now claimed at dispatch time, preventing races; staged bins at core nodes are claimable
- **Ghost robot dispatch**: prevented dispatch when no bin is available at source node
- **Bin claim release**: claims released on fleet-reported order failure
- **TC-25 dismissed**: staged bin poaching is a non-issue with one-bin-per-node constraint

### Documentation

- **Fleet simulator catalog**: added/updated test case writeups, restored truncated docs
- **Line ending normalization**: `.gitattributes` added, all files normalized to LF

## 2026-03-29 — Compound Order Fixes & Test Extraction

### Bug Fixes

- **Compound sibling cancellation**: cascade cancel to all compound children when parent is cancelled
- **Return order source node**: `maybeCreateReturnOrder` now correctly sets `SourceNode`
- **Multi-bin completion**: added `order_bins` junction table for complex orders that move multiple bins

### Refactoring

- **Shared testdb package**: extracted from inline helpers; test files split by domain for navigability

## 2026-03-28 — FIFO, Concurrency Testing & Bin Dispatch

### Features

- **Strict FIFO retrieval**: oldest eligible bin always retrieved first from NGRP lanes
- **COST mode**: closest-optimal-storage-time retrieval for performance-sensitive lanes
- **Concurrency testing infrastructure**: fleet simulator framework for deterministic multi-robot scenario testing; 9 initial tests

### Bug Fixes

- **TC-36**: orders re-queued on `claim_failed` instead of permanently failing
- **Buried empty bins**: detected and reshuffled when blocking retrieval
- **Complex bin claims**: staged bins at core nodes now claimable for complex orders
- **Dispatch-time claiming**: bins claimed atomically at dispatch to prevent races

## 2026-03-27 — Dispatch Safety

### Bug Fixes

- **Ghost dispatch prevention**: refuse to dispatch when no bin is available at source node
- **Claim release on failure**: bin claims released when fleet reports order failure

## 2026-03-26 — Performance, SSE Stability & UI Polish

### Performance

- **Connection pool limits**: Added `MaxOpenConns` (25), `MaxIdleConns` (10), `ConnMaxLifetime` (5m) to PostgreSQL config with sane defaults. Configurable via web UI on the Config page.
- **Cached robot lookups**: Order enrichment and robots handlers now use the in-memory robot cache instead of per-request fleet API calls. Eliminates N+1 HTTP round-trips when opening order detail modals.
- **SSE debounce**: Client-side debounce on robot-update (2s), order-update (500ms), and bin-update (500ms) event handlers to prevent DOM rebuild bursts from freezing the browser during high-frequency fleet telemetry.
- **Active orders default**: Orders page now defaults to active orders only (`ListActiveOrders`) instead of the last 100 of any status, reducing initial query size.

### SSE Stability

- **Compression exclusion**: Moved SSE `/events` endpoint outside Chi's `middleware.Compress` group. The compression layer was buffering streaming flushes, preventing the server from detecting client disconnects promptly. This caused goroutine buildup and page hang-ups on rapid navigation.
- **Client-side cleanup**: Added `beforeunload` listener to close the EventSource when navigating between pages. Browsers limit HTTP/1.1 to 6 connections per origin — without explicit cleanup, stale SSE connections consumed slots and blocked new page loads.
- **Server IdleTimeout**: Added 120s `IdleTimeout` to `http.Server` as a safety net for orphaned keep-alive connections. `WriteTimeout` intentionally left unset since SSE connections are long-lived writes.

### Bug Fixes

- **Complex order bin tasks**: Orders now specify `JackLoad`/`JackUnload` bin tasks when creating fleet blocks. Previously robots navigated to locations without actually picking up or dropping off bins.
- **Script loading order**: Moved `app.js` before the content block in `layout.html` so the `debounce` utility is defined before page-specific scripts that reference it.
- **Orders tab fixes**: Added dedicated "Active" tab; "All" tab now passes `?status=all` to show all orders instead of returning active-only after the default change.
- **Delivered vs Confirmed**: Split the "Completed" tab into Delivered (amber — robot dropped off, awaiting confirmation) and Confirmed (green — operator receipted, terminal state).
- **Fleet Explorer template**: Fixed missing closing `>` on a `<div>` tag in `rds_explorer.html` that caused a template rendering error.
- **Truncated files restored**: Fixed `test-orders.js`, `processes.js`, and `processes.html` footer that were truncated by a previous editing session.

### UI/UX

- **Dashboard tooltips**: Styled hover tooltips on all dashboard stat cards explaining each metric (Active Orders, Total Nodes, Fleet Manager, Messaging, Database, Polling Orders, Completion Anomalies, Pending Outbox).
- **Config page defaults**: Connection pool fields now display effective defaults (25/10/5m0s) instead of showing zeros when unconfigured.
- **Light mode form inputs**: Added explicit `background`/`color` using CSS variables to all form elements. The `color-scheme: light dark` meta tag was causing browsers to render dark form controls in light mode. Removed now-redundant dark theme overrides.
- **Responsive operator grid**: Auto-scaling grid columns for 7", 10", and larger displays. Fixed tile dimensions so they don't stretch to fill the screen.
- **Changeover buttons**: Added CHANGEOVER/CUTOVER controls to operator HMI header with style picker overlay.
- **SSE for config changes**: Backend now broadcasts SSE events for changeover and material config changes, eliminating manual page refreshes.
- **Dark theme fixes**: Distinct duration bar colors for dispatched vs in-transit, source/dest converted to dropdowns, swap mode field visibility logic corrected.

## 2026-03-25 — Universal Node Naming Alignment

### Transport Order Rename: pickup_node → source_node

Aligns transport order vocabulary with Derek's architecture (`OrderAck.SourceNode` precedent). Renames `pickup_node` / `PickupNode` to `source_node` / `SourceNode` across the entire codebase — protocol payloads, database schemas, Go structs, handlers, dispatch/planning logic, UI, and documentation.

- **Protocol**: `OrderRequest`, `OrderStorageWaybill`, `OrderIngestRequest`, `OrderStatusSnapshot` all use `source_node` (wire-breaking change — edge and core must deploy together)
- **Database migrations**: SQLite `ALTER TABLE orders RENAME COLUMN pickup_node TO source_node`; PostgreSQL via `migrateRenames()` for both `orders` and `mission_telemetry`
- **Store layer**: `Order.SourceNode`, `MissionTelemetry.SourceNode`, `UpdateOrderSourceNode()`
- **Dispatch/Engine**: planning service, fulfillment scanner, compound/complex orders, wiring, recovery — all updated
- **API handlers**: JSON tags and request structs on both edge and core

### Complex Order Test Form: Consistent Field Names

Renames complex order test handler fields to match style/cell config vocabulary:

- `FullPickup` → `InboundSource` (`full_pickup` → `inbound_source`)
- `StagingNode` → `InboundStaging` (`staging_node` → `inbound_staging`)
- `StagingNode2` → `OutboundStaging` (`staging_node_2` → `outbound_staging`)
- `OutgoingDest` → `OutboundDestination` (`outgoing_dest` → `outbound_destination`)

### Claim Field Rename: outbound_source → outbound_destination

The `outbound_source` field on `style_node_claims` was a misnomer — it's a dropoff destination (where outbound material goes TO), not a source. Every usage in `material_orders.go` is `buildStep("dropoff", claim.OutboundDestination)`. Renamed across Go structs, SQL, HTML, JS, and added SQLite migration.

### UI Label Updates

- "Pickup Node" → "Source Node" across all order forms and detail views
- "Full Source" → "Inbound Source", "Staging Area 1/2" → "Inbound/Outbound Staging"
- "Outgoing Destination" → "Outbound Destination"
- "Production Node" → "Core Node" (edge manual order form)

## 2026-03-24 — Queued Order Fulfillment

### Queued Orders

Orders that cannot be immediately fulfilled (no source bin, no empty bin available) are now **queued** instead of failed. Core holds them in a `queued` status and automatically fulfills them FIFO when matching inventory becomes available. This eliminates race conditions when multiple nodes compete for scarce bins and removes the need for operators to manually retry failed orders.

**New status:** `queued` — first-class member of the order lifecycle. Applies to all retrieve and retrieve_empty orders, not just bin_loader nodes.

```
pending → sourcing → [found] → dispatched → in_transit → delivered → confirmed
                   → [not found] → queued → [bin available] → dispatched → ...
                   → [not found] → queued → [cancelled] → cancelled
```

### Fulfillment Scanner (Core)

Event-driven scanner monitors queued orders and matches them to available inventory:

- **Triggers:** bin arrival at storage, manifest clear, order completion/cancellation/failure (any event that frees a bin)
- **Safety sweep:** 60-second periodic scan catches anything events missed
- **Startup recovery:** scans queued orders on Core restart
- **FIFO fairness:** oldest queued order for a matching payload gets fulfilled first
- **Atomic claims:** `ClaimBin` prevents races between concurrent fulfillment attempts
- **Node vacancy guard:** skips fulfillment if the delivery node already has an in-flight delivery
- **Fleet failure recovery:** re-queues the order if fleet dispatch fails (transient, not permanent failure)
- **Mutex-guarded:** only one scan runs at a time, events coalesced during scan

### Payload Code Persistence

`payload_code` column added to Core's orders table (migration v8). Persisted at order creation so the fulfillment scanner can match queued orders to compatible bins without re-resolving from the original request.

### Edge Visibility

- `StatusQueued` with valid transitions: `submitted → queued`, `queued → acknowledged/cancelled/failed`
- Edge handler routes `OrderUpdate` with `status=queued` to proper status transition
- Startup reconciliation handles `queued` status from Core
- **Operator station:** bin_loader tiles show "AWAITING STOCK" in amber when a queued order is active
- **Material page:** queued orders show in the orders column with queued status badge

### Core Visibility

- SSE `order-update` event with `type: "queued"` for live dashboard refresh
- Amber CSS badge (`badge-queued`) in both light and dark themes
- Queued orders visible in active orders list

### Cancellation

Operators can cancel queued orders from Edge. Core's existing cancel flow works — no vendor order to cancel, no bin to unclaim, status transitions to cancelled cleanly.

## 2026-03-24 — Bin Loader Nodes, Core Telemetry API, NodeGroup Removal

### Bin Loader Role

New `bin_loader` claim role for nodes where forklifts load untracked material into existing bins. Operators select a payload from the claim's allowed list, confirm the manifest (from Core's payload template), set UOP count, and submit. The bin's manifest is set on Core via direct HTTP — no Kafka, immediate feedback.

- **Allowed payload codes** on style_node_claims — multi-select in claim modal, restricts which payloads a loader accepts
- **Load Bin** action on operator station and material page — payload picker, manifest from template, editable UOP
- **Clear Bin** action — reset a mis-loaded bin to empty
- **Move after load** — if outbound destination is configured, a move order auto-dispatches the loaded bin to storage
- **Claim modal field gating** — bin_loader hides swap mode, staging, inbound source, reorder, changeover fields
- **NGRP bulk claim creation** — selecting a group node expands to create claims for all direct physical children

### Core Telemetry API

New lightweight HTTP endpoints for Edge to fetch real-time state from Core, replacing Kafka for synchronous operations:

| Endpoint | Purpose |
|----------|---------|
| `GET /api/telemetry/node-bins` | Bin state per node (label, type, payload, UOP, manifest, confirmed) |
| `GET /api/telemetry/payload/{code}/manifest` | Payload manifest template + UOP capacity |
| `GET /api/telemetry/node/{name}/children` | Physical children of an NGRP node |
| `POST /api/telemetry/bin-load` | Set manifest on bin at node (was Kafka `bin.load`) |
| `POST /api/telemetry/bin-clear` | Clear bin manifest at node |

Edge `CoreClient` (`engine/core_client.go`) makes on-demand HTTP calls with 3s timeout. Graceful degradation — views render without bin data if Core is unreachable. Core API URL configured in Edge settings page.

### Bin State Visibility

- **Operator station tiles** show bin label (bold), loaded payload code, and EMPTY/LOADED/NO BIN status
- **Material page** shows bin label, payload from Core, and actual UOP count for bin_loader nodes
- **View Contents** modal on material page shows full bin manifest (part numbers, quantities), bin type, and confirmation status
- **Core nodes page** refreshes via SSE on bin-load/clear events; inventory display enriched with bin type, contents, UOP, and lock/claim badges

### NodeGroup Removal

Removed `NodeGroup` field from wire protocol `ComplexOrderStep`. Core auto-detects NGRP nodes via `IsSynthetic + NodeTypeCode` and resolves them — same pattern simple orders already used. Collapsed 4 edge claim source columns (`inbound_source_node`, `inbound_source_node_group`, `outbound_source_node`, `outbound_source_node_group`) into 2 (`inbound_source`, `outbound_destination`).

### Code Quality

- Removed `enrichSingleViewBinState` wrapper (inlined at call site)
- `FetchNodeBins` error handling made consistent with other read methods (silent degradation)
- `slices.Contains` replaces hand-rolled loop in `LoadBin`
- Dead self-assignment removed from `SwitchNodeToTarget`
- Node children endpoint uses `GetNodeByDotName` for dot-notation consistency
- `bin.load` Kafka artifacts fully removed: `TypeBinLoad`, `BinLoadRequest`, `BinLoadAck`, `HandleBinLoad` from protocol, dispatcher, and core handler

## 2026-03-23 — Delivery Cycle Modes: Sequential, Single Robot, Two Robot

Adds source/destination routing to `style_node_claims`, fixes single-robot and two-robot step sequences, and introduces sequential mode.

### New Fields on `style_node_claims`

Two columns for source/destination routing, separate from staging areas:

```
InboundSource → InboundStaging → CoreNodeName → OutboundStaging → OutboundDestination
 (where from)     (temp park)      (lineside)     (temp park)       (where to)
```

| Field | Purpose |
|-------|---------|
| `inbound_source` | Pickup node or group for new material (Core auto-detects groups) |
| `outbound_destination` | Dropoff node or group for old material (Core auto-detects groups) |

Can be a specific node or a node group — Core auto-detects NGRP nodes and resolves via the group resolver. Blank = Core global fallback by payloadCode. Fully backward compatible.

### Step Sequences

#### Sequential — two robots, staggered dispatch

```
Order A (Robot 1 — removal):             Order B (Robot 2 — backfill):
┌─────────────────────────────────┐      ┌─────────────────────────────────┐
│ 1. dropoff(CoreNodeName)        │      │ 1. pickup(InboundSource)        │
│ 2. wait                         │      │ 2. dropoff(CoreNodeName)        │
│ 3. pickup(CoreNodeName)         │      └─────────────────────────────────┘
│ 4. dropoff(OutboundDestination)      │────────▶ Order B auto-created when
└─────────────────────────────────┘        Order A goes "in_transit"
```

Order A delivery_node = "" (removal, no UOP reset). Order B delivery_node = CoreNodeName (backfill, resets UOP).

#### Single Robot — 10-step swap (was 7)

```
 1. pickup(InboundSource)          — pick new from source
 2. dropoff(InboundStaging)        — park new at inbound staging
 3. dropoff(CoreNodeName)          — pre-position at lineside
 4. wait                           — operator releases
 5. pickup(CoreNodeName)           — pick up old from line
 6. dropoff(OutboundStaging)       — quick-park old nearby
 7. pickup(InboundStaging)         — grab new from staging
 8. dropoff(CoreNodeName)          — deliver new to line
 9. pickup(OutboundStaging)        — grab old from staging
10. dropoff(OutboundDestination)        — deliver old to final dest.
```

#### Two Robot — parallel swap

```
Order A (resupply):                      Order B (removal):
┌─────────────────────────────────┐     ┌─────────────────────────────────┐
│ 1. pickup(InboundSource)        │     │ 1. dropoff(CoreNodeName)        │
│ 2. dropoff(InboundStaging)      │     │ 2. wait                         │
│ 3. wait                         │     │ 3. pickup(CoreNodeName)         │
│ 4. pickup(InboundStaging)       │     │ 4. dropoff(OutboundDestination)      │
│ 5. dropoff(CoreNodeName)        │     └─────────────────────────────────┘
└─────────────────────────────────┘
```

Two-robot validation now only requires InboundStaging (not OutboundStaging) — removal robot goes direct to OutboundDestination.

### Other Changes

- `buildStep` helper sends node name; Core auto-detects groups (no `node_group` on wire protocol)
- `BuildDeliverSteps` / `BuildReleaseSteps` use source routing instead of staging fields for pickup/dropoff
- Sequential backfill wired via `EventOrderStatusChanged` → `handleSequentialBackfill` in `engine/wiring.go`
- UI: "Sequential" added to swap mode dropdown, source/destination fields added to claim modal
- `NodeGroup` field removed from `ComplexOrderStep` wire protocol — Core auto-detects NGRP nodes

### Files Changed

| File | Change |
|------|--------|
| `store/schema.go` | Migrations for source routing columns (collapsed to `inbound_source`, `outbound_destination`) |
| `store/style_node_claims.go` | Struct + SQL updated for 2 source fields |
| `engine/material_orders.go` | Step builders rewritten: buildStep helper, 10-step single, source routing on two-robot, sequential builders added |
| `engine/operator_stations.go` | Sequential case added to `requestNodeFromClaim`, two-robot validation relaxed |
| `engine/wiring.go` | `EventOrderStatusChanged` subscription + `handleSequentialBackfill` handler |
| `www/templates/processes.html` | Sequential in dropdown, source/destination fieldset in claim modal |
| `www/static/js/pages/processes.js` | Source fields wired in edit/save/display, validation updated |

## 2026-03-21 — Lifecycle, Messaging, and Recovery Hardening

### Core Reliability

- **Durable inbound dedupe:** Core now persists inbound message IDs and suppresses replayed mutating commands before they reach dispatch.
- **Completion hardening:** Delivery receipts fail closed, duplicate receipts are ignored, and completion-side state changes are safer and more atomic.
- **Outbox consistency:** Core control/data replies now use the same durable outbox-backed delivery model as dispatch replies.
- **Reconciliation:** Core now detects completion drift, stale claims, stuck orders, expired staged bins, stale edges, dead letters, and outbox backlog age.
- **Recovery actions:** Safe audited repair actions were added for completion drift, stale terminal claims, staged-bin release, dead-letter replay, and stuck-order cancellation.

### Edge Reliability

- **Confirm fail-closed:** Edge no longer transitions an order to `confirmed` if the delivery receipt cannot be durably enqueued.
- **Startup reconciliation:** Edge requests authoritative order status from Core on startup and after re-registration so local state can be corrected after restart or disconnect.
- **Diagnostics:** Edge diagnostics now expose reconciliation anomalies, dead-letter outbox messages, and replay/sync actions.
- **Messaging split clarified:** Order mutations use durable outbox delivery, while operational data traffic uses an explicit direct-send path with retry.

### Architecture Cleanup

- **Core lifecycle extraction:** Order creation, cancel, receipt, redirect, ingest/store setup, and reply transport were moved behind explicit lifecycle and reply services.
- **Core planner registry:** Dispatch planning is now routed through registered order-type planners instead of a hardcoded `switch`, making new order types much less threaded to add.
- **Core data handler extraction:** Data-subject handling was split out of `CoreHandler`, leaving the transport layer thinner.
- **Edge lifecycle extraction:** Edge order transitions and Core-status reconciliation moved behind a lifecycle service, while envelope creation/enqueue moved into a dedicated order sender.
- **Messaging consistency:** Edge heartbeats and Core-sync requests now use a shared data sender instead of separate ad hoc publish flows.

### Observability and Diagnostics

- **Dashboard/health visibility:** Reconciliation summary and severity are now surfaced in Core dashboard, diagnostics, and health endpoints.
- **Recovery history:** Recovery actions are recorded so operator/admin repairs are auditable.
- **Diagnostics UI expansion:** Core diagnostics now includes reconciliation anomalies, dead letters, replay actions, and recovery workflows.

## 2026-03-21 — Edge Production Hardening & Domain Rename

### Breaking Changes

**Domain model rename** — Edge types, DB tables, API routes, and UI labels have been renamed to align with actual usage:

| Old | New | Rationale |
|-----|-----|-----------|
| `Payload` (store) | `MaterialSlot` | Edge's "payload" was a per-line slot config, not a template. Core owns the template (`PayloadCatalog`). |
| `ProductionLine` | `Process` | UI already said "Process". Matches terminology doc. |
| `JobStyle` | `Style` | UI already said "Style". Matches terminology doc. |
| `LocationNode` | `Node` | Redundant name. Matches Core's `Node`. |
| `Resupply` / `Removal` | `PrimaryOrder` / `SecondaryOrder` | Mode-neutral naming for `OrderRequestResult`. |

**DB migration** is automatic — `ALTER TABLE RENAME` runs on startup for existing databases.

**API routes renamed:**
- `/api/payloads/*` → `/api/material-slots/*`
- `/api/lines/*` → `/api/processes/*`
- `/api/job-styles/*` → `/api/styles/*`
- `/api/location-nodes/*` → `/api/nodes/*`

**Query param:** `?line=` → `?process=`

### Production Reliability Fixes

- **Cancel safety:** Cancel message enqueued before local transition. Prevents robot continuing on a locally-cancelled order.
- **Envelope failure handling:** Orders stay in `pending` if envelope fails to build or enqueue. Prevents stuck `submitted` orders that Core never receives.
- **Two-robot half-cycle prevention:** If removal order fails after resupply succeeds, resupply is automatically cancelled.
- **Replenishing deadlock fix:** Payload slot reset from `replenishing` to `active` if order creation fails, so auto-reorder can re-trigger.
- **Changeover durability:** DB write happens before in-memory state change. Errors propagate to HTTP response instead of being swallowed.
- **Store order waybill:** Enqueued before status transition. Failure returns error to operator.
- **Redirect order:** Envelope enqueued before local DB update. Failure returns error.
- **Production reporter:** Accumulated deltas restored on outbox enqueue failure (no silent data loss).
- **Heartbeat retry:** Periodic heartbeats now use 3-attempt retry with backoff (matches startup behavior).
- **Dead-letter logging:** Outbox dead-lettered messages logged at ERROR level with debug trace.

### Code Quality

- Domain constants for slot statuses (`SlotActive`, `SlotEmpty`, `SlotReplenishing`), roles, cycle modes, and dispatch reply types — eliminates scattered string literals.
- Debug logging added to all critical paths: order completion, order failure, slot reorder, auto-confirm, payload metadata lookup, envelope failures.
- `CountActiveOrders` error handling fixed (was silently returning 0).
- DB index added on `orders.payload_id`.
- `Multiplier` field removed (was always 1).
- `ManageReportingPointTag` flattened from nested conditionals to linear flow.

### UI Changes

- **Nav restructured:** 3 public tabs (Status, Orders, Changeover) + Admin dropdown (Setup, Production, Manual Order, Operator, Messages, Logs).
- **Auth gating:** Production, Manual Order, and Operator pages moved behind admin login. Operator display/cell views remain public (shop floor monitors).
- **Login/Logout** link added to nav bar.
- **Labels cleaned up:** "LSL Node" → "Location", "UOP Total" → "Capacity", "Reorder Pt" → "Reorder At", "Define Payloads" → "Material Slots", "Location Node" → "Node", removed "ALN or PLN" jargon.
