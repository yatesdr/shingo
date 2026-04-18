# Changelog

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

- **Go 1.26.2**: bumped across all modules (`protocol/`, `shingo-core/`,
  `shingo-edge/`). `x/crypto` aligned to match. `go mod tidy` swept
  afterward.

### Code Quality

- **Dead-code audit**: flagged potentially-dead symbols across the
  codebase with `TODO(dead-code)` comments for a later pruning pass.
  No deletions in this commit — just annotations on the callsite-free
  functions and variables found during the sweep.

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

## 2026-04-13 — Wait Block, Operator UX & Route Visibility

### Features

- **RDS Wait block**: replaced pre-position dropoff with RDS native Wait block for wait-at-node sequences — eliminates dummy location visits
- **Load bin at node**: operators can load an empty bin already at the node without waiting for a delivery order
- **Step-by-step route display**: mission detail and test orders pages show the full block-by-block route for each order

### Bug Fixes

- **Auto-return safety**: skip auto-return for complex orders when bin position is uncertain after partial completion
- **Same-node move prevention**: refuse to dispatch a move order where source and destination resolve to the same node
- **Single-payload auto-select**: auto-select payload in load bin modal when only one option is available
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
