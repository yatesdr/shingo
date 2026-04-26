# Coverage Map — shingo-core / shingo-edge / protocol

**Baseline commit:** `36ea180` (toolchain downgrade to Go 1.25.0)
**Captured:** 2026-04-19, Go 1.25.0, `go test -tags=docker -coverprofile=coverage-*.out`
**Active branch:** `refactor/shingo-architecture-3` (Phase 6 sweep; squashed Phase 1–5 baseline at `68113fb`)

> **Phase 6 status:** This map predates the Phase 6 sweep. New sub-packages
> introduced or reshaped during 6.0a/6.0b/6.1 land without coverage entries
> below; re-capture once the full Phase 6 branch lands.
>
> **Phase 6.0a additions (core):** `store/schema/` (DDL constant +
> `Apply` / `TableExists` / `ColumnExists` / `ColumnType` introspection
> helpers, extracted from top-level `schema_postgres.go` and
> `migrations.go`). 35 type aliases on the outer `*store.DB` shim now
> carry `// Deprecated:` comments — the marker Phase 6.4 will use to
> grep callers and migrate them off `store.X` to `store/<pkg>.X`.
>
> **Phase 6.0b additions (edge):** `store/schema/` (DDL constant +
> `Apply` / `TableExists` / `TableHasColumn`, extracted from the
> 1013-line `schema.go`). The 17-version migration logic moved from
> `schema.go` to a new top-level `migrations.go`. `SetStationNodes`
> extracted from `operator_stations.go` to a new top-level
> `station_nodes.go` (cross-aggregate orchestration spanning stations
> + processes + orders, queued for service-layer extraction in 6.1).
> 25 type aliases on edge's outer `*store.DB` shim carry
> `// Deprecated:` comments. `lineside/` accepted as the eleventh
> edge sub-package (post-squash addition from `4efbab4`); the v4
> plan's "10 total" acceptance criterion is amended to 11.
>
> **Deferred from 6.0b** (will land as a follow-up before 6.3
> depguard rules write the cross-aggregate allow-list): fold
> `shifts/` into `counters/`, fold `styles/` + `claims/` +
> `changeovers/` into `processes/`, rename `outbox/` → `messaging/`,
> rename `payloads/` → `catalog/`. These are organizational moves
> with no behavior change; depguard's allow-list lands against the
> shipped names initially and gets a one-line update when the folds
> are committed.
>
> **Phase 6.1 additions:** Service-layer extraction for the seven
> cross-aggregate coordinators identified in Phase 5 closeout.
> Core: `BinService.ApplyArrival` (new method on existing service),
> `TagVerifyService` (new service). `NodeService.ListNodeStates`
> and `InventoryService.List` were already wired through services
> in Phase 3a — no change. Edge: new `shingo-edge/service/`
> package introduced (was previously absent — edge took named
> *engine.Engine methods in Phase 4) holding `StationService`
> (`SetNodes`, `BuildView`) and `ChangeoverService` (`Create`).
> Edge engine accessors `Engine.StationService()` and
> `Engine.ChangeoverService()` added. The named-method shims
> (`Engine.SetStationNodes`, `Engine.BuildOperatorStationView`)
> stay on the `EngineAccess` contract but their bodies now route
> through services. Five `*store.DB` cross-aggregate methods marked
> Deprecated — Phase 6.4's type-alias sweep retires them and
> migrates remaining test-file callers.
>
> **Phase 6 reshape (v6 plan, 2026-04-25):** Two rounds of
> architectural review reshaped the remaining Phase 6 work after
> 6.1 verified clean.
>
> Round 1 (four reviewers, "should we do 6.2 at all?"): unanimous
> "skip 6.2." Phase 6.2 framing was a Go-language constraint
> dressed up as a refactor; the receiver `*www.Handlers` cannot
> move into a sub-package without inventing new types.
>
> Round 2 (four reviewers, "what would world-class Go architects
> do with the *Handlers struct?"): unanimous "leave it alone." The
> pattern is idiomatic Go at this scale (std lib, CockroachDB,
> Hugo, Caddy, chi examples all use it). The decision is recorded
> in the implementation-plan v6 with a tripwire condition (handler
> count >50 / independent deployment / dep-set divergence) for any
> future revisit.
>
> - **Phase 6.2 (handler sub-packages) WON'T DO.** Plan framed it
>   as "pure import-path mechanics with no behavior change" — wrong.
>   Per-domain split forces ~30+ new struct types, rewrites every
>   test fixture. No architectural payoff justifies the churn.
>   Round-2 review confirmed the `*www.Handlers` struct as
>   idiomatic Go at this scale; not technical debt.
> - **Phase 6.5 (DTO extraction) CUT.** Sub-packages already own
>   their types. Adding `dto/` solves a hypothetical second-consumer
>   problem we don't have.
> - **Phase 6.6 (EngineAccess split) CUT.** Absorbed by 6.2′ —
>   killing edge passthroughs collapses the interface from ~93 to
>   ~30 methods without splitting.
> - **Phase 7 (changeover domain) CUT.** Premature decomposition
>   for a single-line plant.
> - **Phase 8 (composition root) DEFERRED.** Better answer is
>   `engine.Wire()` than file splits; defer until concrete pain.
>
> **Next commits on the branch (v6 plan):**
>
> - ✅ **6.0c housekeeping + depguard config + h.eng cleanup**
>   delivered. Edge folded `styles/`+`claims/`+`changeovers/` →
>   `processes/` (per-file inside the package: `styles.go`,
>   `claims.go`, `changeovers.go`), renamed `outbox/` →
>   `messaging/` and `payloads/` → `catalog/`, deleted the dead
>   `h.eng` field on both modules (core: pure delete; edge:
>   moved SSE wiring to use local `eng` parameter, then deleted
>   the field). `.golangci.yml` extended with the
>   `store-sub-pkg-isolation` rule forbidding sub-package-to-
>   sub-package imports inside the store, with a documented
>   exception for `reconciliation/` → `messaging/` (MaxRetries
>   constant). The originally-planned `shifts/` → `counters/`
>   fold was dropped — shifts (1st, 2nd, 3rd) is a first-class
>   manufacturing-domain concept, not a counter implementation
>   detail. Edge sub-package count after 6.0c: **11**
>   (admin, catalog, counters, lineside, messaging, orders,
>   processes, reconciliation, schema, shifts, stations; plus
>   internal/helpers).
> - ✅ **6.4a Deprecated method retirement** delivered. After
>   four-reviewer round-3 consensus that the services-as-thin-wrappers
>   shape was incoherent, the 5 orchestration bodies moved into their
>   services: BinService.ApplyArrival, TagVerifyService.VerifyTag,
>   ChangeoverService.Create, StationService.SetNodes,
>   StationService.BuildView. Five `*store.DB` methods deleted; three
>   files retired (completion.go, station_nodes.go, plus the
>   VerifyTag method body in tag_verify.go). messaging/core_data_service.go
>   migrated to TagVerifyService internally (constructor signature
>   stayed stable). Five test files relocated:
>   completion_test.go → service/bin_service_arrival_test.go;
>   tag_verify_test.go → service/tag_verify_service_test.go; five
>   tests excised from edge/store_coverage_test.go and ported to
>   service/station_service_test.go and service/changeover_service_test.go;
>   in-place substitutions in engine/operator_stations_test.go and
>   www/handlers_manualorder_changeover_test.go. New
>   shingo-edge/internal/testdb package added (mirrors core's pattern)
>   so external test packages can spin up SQLite test DBs.
>   `computeSwapReady` and `lookupLastReleaseError` exported as
>   `ComputeSwapReady` / `LookupLastReleaseError` so the service can
>   call them from outside store; their tests in
>   station_views_test.go updated. Type aliases on outer-store
>   delegate shims survive into 6.4b for the grep-and-replace sweep.
>
> - ✅ **6.2′ edge service-layer completion** delivered.
>   `shingo-edge/engine/engine_db_methods.go` deleted (was 60
>   passthrough methods). Seven new services per-file under
>   `shingo-edge/service/`: AdminService, ProcessService,
>   StyleService (includes claim CRUD), ShiftService,
>   CounterService (reporting points + snapshots + hourly
>   counts + anomalies), CatalogService, OrderService.
>   Split the existing `service/service.go` from 6.1 into
>   `station_service.go` + `changeover_service.go`, matching
>   core's per-file pattern. Engine struct grew 7 service
>   pointers + 7 accessor methods. EngineAccess interface
>   collapsed from 103 methods to ~35 (subsystem accessors +
>   service accessors + composite orchestration verbs). All
>   ~90 production handler call sites migrated from
>   `h.engine.X(...)` to `h.engine.YService().X(...)`. Test
>   stubEngine in `helpers_test.go` shrunk: 60 named-method
>   stubs replaced with 9 service-accessor stubs returning real
>   services backed by testDB. Edge is now at architectural
>   parity with core (which finished its equivalent extraction
>   in Phase 3a).
> - ✅ **6.4b production type-alias removal** delivered. AST-aware
>   substitution script swept 244 files, replacing every `store.X`
>   reference (where X was one of the 60 Deprecated aliases) with
>   the qualified `subpkg.X` form. The script detected per-file
>   collisions between sub-packages and same-named outer siblings
>   (`shingoedge/orders` ↔ `shingoedge/store/orders`,
>   `shingo{core,edge}/messaging` ↔ store/messaging) and emitted
>   aliased imports (`storeorders "shingoedge/store/orders"`) only
>   where both packages were already imported in the same file —
>   six edge files use this pattern. Outer-store delegate files
>   then had their 60 `type X = subpkg.Y` declarations and
>   `// Deprecated:` comments deleted; their methods now expose
>   the qualified `subpkg.Y` types directly. 35 outer-store files
>   that referenced the aliases without the `store.` prefix
>   (intra-package refs in test fixtures, view structs, lane
>   queries) had the appropriate sub-package imports added and
>   the bare names substituted. Three doc-only false positives
>   (substitutions inside `doc.go` docstrings) reverted.
>
>   Verification (sandbox has no Go toolchain — Windows AI dev
>   handles `go build` + tests):
>   - 0 remaining `type X = subpkg.Y` aliases in either module.
>   - 0 remaining `store.<aliased>` references anywhere.
>   - 0 stranded `store` package imports.
>   - All sub-package imports point to existing directories.
>   - Import blocks re-grouped per Go convention (stdlib /
>     third-party / internal).
>
> See `implementation-plan.md` v6 for full reshape rationale.

---

## Per-package — shingo-core

| Package              | Baseline | PR 4    | Δ        | Notes                                       |
|---|---|---|---|---|
| countgroup           | 95.2%    | 95.2%   | —        |                                              |
| domain               | 0.0%     | 100.0%  | +100.0   |                                              |
| config               | 0.0%     | 88.9%   | +88.9    |                                              |
| store/bins           | 0.0%     | 89.1%   | +89.1    |                                              |
| store/payloads       | 0.0%     | 88.3%   | +88.3    |                                              |
| material             | 89.6%    | 89.6%   | —        |                                              |
| scenesync            | 0.0%     | 89.5%   | +89.5    |                                              |
| store/nodes          | 0.0%     | 86.5%   | +86.5    |                                              |
| store/orders         | 0.0%     | 86.7%   | +86.7    |                                              |
| rds                  | 32.4%    | 83.5%   | +51.1    |                                              |
| service              | 18.1%    | 84.0%   | +65.9    |                                              |
| dispatch/binresolver | 71.0%    | 71.0%   | —        |                                              |
| fulfillment          | 70.6%    | 70.6%   | —        |                                              |
| store                | 31.6%    | 67.2%   | +35.6    | +telemetry, node groups, station lifecycle    |
| dispatch             | 65.6%    | 65.6%   | —        |                                              |
| engine               | 36.0%    | 62.8%   | +26.8    | +auto-return wiring, receive confirmation     |
| www                  | 17.5%    | 44.9%   | +27.4    | +payload templates, diagnostics, node pages   |
| messaging            | 24.9%    | 28.3%   | +3.4     |                                              |
| fleet/seerrds        | 7.4%     | 25.0%   | +17.6    |                                              |
| cmd/shingocore       | 0.0%     | 0.0%    | —        | no tests                                     |
| fleet                | 0.0%     | 0.0%    | —        | no tests                                     |
| fleet/simulator      | 0.0%     | 0.0%    | —        | no tests                                     |
| internal/testdb      | 0.0%     | 0.0%    | —        | test infrastructure only                      |

**Total: 56.8%** (up from 29.0%, +27.8 pp — from `go tool cover -func | tail -1`)

---

## Per-package — shingo-edge

Re-captured 2026-04-19 after PRs 3.2 + 3.3 landed (plus one store
bugfix from `f20ae40`).

| Package    | Coverage  | Notes                                 |
|---|---|---|
| **orders** | **83.3%** | up from 13.3% — PR 3.1                |
| **store**  | **79.4%** | up from 22.5% — PR 3.2                |
| countgroup | 73.7%     |                                       |
| plc        | 54.9%     |                                       |
| **engine** | **50.3%** | up from 40.0% — PR 3.3                |
| **www**    | **49.3%** | up from 0% — PRs 2.1–2.9              |
| backup     | 11.9%     |                                       |
| cmd/shingoedge | 0.0%  | no tests                              |
| config     | 0.0%      | no tests                              |
| messaging  | 0.0%      | no tests                              |

**Total: 48.5%** (up from 33.8%, +14.7 pp edge-wide). The three
test-heavy pillar packages (orders, store, engine) account for most
of the gain: PR 3.1 added +70 pp to orders, PR 3.2 added +56.9 pp to
store, PR 3.3 added +10.3 pp to engine.

Engine came in below the ~60% target because my coverage PR
deliberately skipped `Engine.Start()` and the PLC-polling / WarLink
lifecycle (unsafe to run from a unit test without a fake stack); those
uncovered slices live in `warlink.go` (2.5%), `operator_demand.go`
(7.4%), `hourly_tracker.go` (21.4%), `changeover_restore.go` (0%), and
`operator_bin_ops.go` (0%), which together dominate the package's
line count.

### Orders package — per-file (PR 3.1)

| File                 | Before | After  | Notes                                                                                  |
|---|---|---|---|
| manager.go           | 6.3%   | ~85%   | `forceTransitionOrder` 0% is a profile artifact (ApplyCoreStatusSnapshot uses the inline lifecycle method, not the Manager wrapper); `enqueueAndAutoSubmit` at 30% — only the retrieve path hits it in tests |
| lifecycle_service.go | 35.7%  | ~87%   |                                                                                        |
| sender.go            | 7.1%   | ~82%   |                                                                                        |
| package              | 13.3%  | 83.3%  | 36 tests in `manager_coverage_test.go`                                                 |

### Store package — per-file (PR 3.2)

`store_coverage_test.go` adds ~46 test functions exercising the data-
access layer. Per-file averages (mean of per-function coverage) from
`coverage-edge-func.txt`:

| File                     | Before | After   | Notes                                                         |
|---|---|---|---|
| process_node_runtime.go  | 0%     | 98.0%   | ensure + set + update orders + update progress                |
| payload_catalog.go       | low    | 96.4%   | upsert + prune safety + get-by-code                           |
| style_node_claims.go     | 0%     | 94.1%   | upsert (all roles, swap modes) + manual_swap validation       |
| shifts.go                | 0%     | 93.9%   | upsert + list                                                 |
| admin_users.go           | 0%     | 93.8%   | create + list + update + auth-check paths                     |
| reporting_points.go      | low    | 93.0%   | CRUD + enable/disable + duplicate detection                   |
| counter_snapshots.go     | low    | 92.6%   | insert + latest-by-rp                                         |
| styles.go                | 0%     | 91.2%   | full CRUD                                                     |
| processes.go             | 0%     | 90.3%   | CRUD + SetActiveStyle state changes                           |
| orders.go                | low    | 89.9%   | create + transitions + history (set-based assertion)          |
| process_changeovers.go   | 0%     | 89.2%   | CreateAtomic with tasks + transition states                   |
| hourly_counts.go         | low    | 88.4%   | upsert + list window + zero-rollover                          |
| operator_stations.go     | 0%     | 87.5%   | CRUD + move-up/down + SetStationNodes disables-not-deletes    |
| reconciliation.go        | 0%     | 82.4%   | 4 tests including dead-letter + critical age + stuck orders   |
| process_nodes.go         | 0%     | 81.7%   | create + list + delete                                        |
| outbox.go                | low    | 62.8%   | covered indirectly via reconciliation + engine tests          |
| station_views.go         | —      | 40.9%   | out of scope — exercised from the www layer                   |
| package                  | 22.5%  | **79.4%** | +56.9 pp                                                    |

### Engine package — per-file (PR 3.3)

`engine_coverage_test.go` adds 44 test functions covering the thin
accessor/adapter/service layer that had zero coverage before. Per-file
averages (mean of per-function coverage) from `coverage-edge-func.txt`:

| File                       | Before | After   | Notes                                                            |
|---|---|---|---|
| adapters.go                | 0%     | 100.0%  | every plcEmitter + orderEmitter method (~14 events) with both nil- and non-nil-error branches |
| changeover.go              | 0%     | 100.0%  |                                                                  |
| eventbus.go                | —      | 100.0%  |                                                                  |
| reconciliation.go          | 0%     | 100.0%  | thin Engine-level delegates                                      |
| reconciliation_service.go  | 0%     | 100.0%  | Summary + ListAnomalies + ListDeadLetterOutbox + RequeueOutbox round-trip |
| core_sync_service.go       | 0%     | 96.7%   | StartupReconcile hook-firing + RequestOrderStatusSync (no-sendFn, no-orders, success, sendFn-error) + HandleOrderStatusSnapshots (known + unknown UUID) |
| core_client.go             | 0%     | 96.0%   | Available/SetBaseURL + all HTTP methods against httptest servers — success, 404/500, bad-JSON, network-error, empty-input short-circuits |
| engine.go (accessors)      | 0%     | 85.7%   | DB/CoreAPI/AppConfig/ConfigPath/PLCManager/OrderManager/Uptime/Stop; SetCoreNodes/CoreNodes sync + event emission; SetNodeSyncFunc/SetCatalogSyncFunc/SetSendFunc/SetKafkaReconnectFunc injection + RequestNodeSync/RequestCatalogSync/SendEnvelope/ReconnectKafka with + without fn set; HandlePayloadCatalog upsert + prune + empty-safety |
| countgroup_sender.go       | 0%     | 83.3%   | SendCountGroupAck: no-sendFn, success with envelope decode assertions, sendFn-error |
| material_orders.go         | —      | 79.5%   | pre-existing coverage preserved                                  |
| operator_produce.go        | —      | 75.5%   | pre-existing                                                     |
| operator_ab_cycling.go     | —      | 70.6%   | pre-existing                                                     |
| wiring.go                  | —      | 66.1%   | pre-existing                                                     |
| operator_helpers.go        | —      | 65.5%   | pre-existing                                                     |
| operator_changeover_ops.go | —      | 61.3%   | pre-existing; deep orchestration best covered via integration    |
| operator_node_changeover.go| —      | 48.8%   | pre-existing                                                     |
| operator_stations.go       | —      | 46.0%   | pre-existing                                                     |
| hourly_tracker.go          | —      | 21.4%   | **uncovered** — requires PLC polling lifecycle                   |
| operator_demand.go         | —      | 7.4%    | **uncovered** — requires real WarLink + demand fixtures          |
| warlink.go                 | —      | 2.5%    | **uncovered** — requires WarLink stack                           |
| changeover_restore.go      | —      | 0.0%    | **uncovered** — startup-only restore path                        |
| operator_bin_ops.go        | —      | 0.0%    | **uncovered** — bin-ops orchestration deferred                   |
| package                    | 40.0%  | **50.3%** | +10.3 pp; Start()-required paths (WarLink, PLC polling, bin-ops, changeover restore) remain uncovered by design |

**Known gaps kept for a later PR:**

- `Engine.Start()` lifecycle — starts real PLC polling and WarLink, not safe as a unit test. Needs a fake PLC/WarLink stack.
- `operator_changeover_ops.HandleChangeoverCompletion` — deep cross-service orchestration best covered via an integration test.

---

## Per-package — protocol

| Package  | Coverage |
|---|---|
| backoff   | 100.0%   |
| eventbus  | 100.0%   |
| outbox    | 93.5%    |
| auth      | 80.0%    |
| root      | 69.7%    |
| debuglog  | 18.1%    |
| types     | 0.0%     |

**Total: 63.6%**

---

## Per-handler — shingo-core/www

20 handler files. 8 of 20 (40%) have test counterparts (up from 6/20 after PR 4).

| Handler file                  | Test? | Line coverage | Test file(s)                                          |
|---|---|---|---|
| handlers_bins.go              | Y     | 51.9%         | `handlers_bins_test.go`, `handlers_bins_gaps_test.go` |
| handlers_cms_transactions.go  | N     | 0.0%          |                                                       |
| handlers_config.go            | N     | 0.0%          |                                                       |
| handlers_corrections.go       | N     | 0.0%          |                                                       |
| handlers_dashboard.go         | N     | 0.0%          |                                                       |
| handlers_demand.go            | Y     | 54.2%         | `handlers_demand_test.go`                             |
| handlers_diagnostics.go       | Y     | ~40%          | `handlers_diagnostics_test.go` (PR 4)                |
| handlers_firealarm.go         | N     | 0.0%          |                                                       |
| handlers_inventory.go         | N     | 0.0%          |                                                       |
| handlers_missions.go          | N     | 0.0%          |                                                       |
| handlers_nodegroup.go         | Y     | 69.5%         | `handlers_nodegroup_test.go`                          |
| handlers_nodes.go             | Y     | 17.0%         | `handlers_nodes_test.go`                              |
| handlers_orders.go            | Y     | 19.0%         | `handlers_orders_test.go`                             |
| handlers_payload_templates.go | Y     | ~75%          | `handlers_payload_templates_test.go` (PR 4)          |
| handlers_payloads.go          | N     | 0.0%          |                                                       |
| handlers_rds_explorer.go      | N     | 0.0%          |                                                       |
| handlers_robots.go            | N     | 0.0%          |                                                       |
| handlers_telemetry.go         | N     | 0.0%          |                                                       |
| handlers_test_orders.go       | Y     | 5.5%          | `handlers_test_orders_test.go`                        |
| handlers_traffic.go           | N     | 0.0%          |                                                       |

---

## Per-handler — shingo-edge/www

14 handler files. **12 of 14 have test counterparts** as of PR 2.9. The
two remaining (`handlers_api.go` — 0 DB/engine sites, and the
template-only `handleKanbans` / `handleChangeover` handlers covered
indirectly via `buildChangeoverViewData`) are accepted gaps.

Per-handler line coverage from `coverage-edge.out` (2026-04-19,
post-PR 2.9):

| Handler file                  | API-fn range | Page-fn coverage | Test file(s)                              |
|---|---|---|---|
| handlers_admin_pages.go       | handleLogin 71.4%, handleLoginPage 75.0%, handleLogout 100% | handleConfig / handleProcesses 0% | `handlers_admin_pages_test.go` (PR 2.5) |
| handlers_api.go               | helpers 75–100% (writeJSON, writeError, parseID, writeJSONWithTrigger 75%) | — | (covered transitively)                |
| handlers_api_config.go        | 50.0–100% (apiGetCoreNodes / apiSyncCoreNodes / apiSyncPayloadCatalog 100%); PLC fns (apiListPLCs, apiWarLinkStatus, apiPLCTags, apiPLCAllTags, apiReadTag) 0% | n/a | `handlers_api_config_test.go` (PR 2.1) |
| handlers_api_orders.go        | 58.8–100%; 5 fns at 100% (Confirm/Release/Submit/Cancel/Redirect); outliers `apiCreateStoreOrder` (58.8%) / `apiCreateMoveOrder` (64.3%) miss the `process_node_id` → `GetProcessNode` source-resolve branches | n/a | `handlers_api_orders_test.go` (PR 2.3) |
| handlers_backup.go            | apiUpdateBackupConfig 94.9%; status/list/run/test/restore 15.8–75% (only nil-svc early exits) | n/a | `handlers_kanbans_backup_test.go` (PR 2.8) |
| handlers_changeover.go        | buildChangeoverViewData 81.8% | handleChangeover / handleChangeoverPartial 0% | `handlers_manualorder_changeover_test.go` (PR 2.9) |
| handlers_diagnostics.go       | apiReplayOutbox 66.7% (validation only); apiRequestOrderStatusSync 0% | handleDiagnostics 0% | `handlers_diag_manual_test.go` (PR 2.6) |
| handlers_kanbans.go           | — | handleKanbans / handleKanbansPartial 0% | (none — template-only public routes)  |
| handlers_manual_message.go    | apiSendManualMessage 73.3% | handleManualMessage 0% | `handlers_diag_manual_test.go` (PR 2.6) |
| handlers_manual_order.go      | n/a | handleManualOrder 0% (admin-gate redirect covered) | (admin-gate via PR 2.5)                |
| handlers_material.go          | buildStationViews 100%; enrichViewBinState 10.0% | handleMaterial / handleMaterialPartial 0% | `handlers_prod_material_test.go` (PR 2.7) |
| handlers_operator_stations.go | 50.0–100%; apiGetOperatorStationView 100%, handleOperatorStationDisplay 72.7%; `apiNodeChildren` / `apiPayloadManifest` 0% (nil CoreAPI stub) | n/a | `handlers_operator_stations_test.go` (PR 2.2, +release-staged in `51ac5dc`) |
| handlers_production.go        | apiSaveShifts 88.2%, apiGetHourlyCounts 84.6%, apiListShifts 60.0% | handleProduction 0% | `handlers_prod_material_test.go` (PR 2.7) |
| handlers_traffic.go           | bindings/heartbeat/add/delete 84.6–100% | handleTraffic 0% (nil PLCManager) | `handlers_traffic_test.go` (PR 2.4)   |

**Lowest-coverage testable APIs** worth a follow-up pass:
`apiStageBackupRestore` (15.8%), `apiTestBackupConfig` (23.1%),
`apiTestCoreAPI` (26.3%), `apiRunBackup` (33.3%), `apiListBackups`
(30.0%) — all need fakes or expanded validation cases.

**Auth (auth.go):** newSessionStore 88.9%, get/getUser/setUser/clear
all 100%. adminMiddleware 100%.

**SSE (sse.go):** Broadcast 50.0%; Start, Stop, register, unregister,
run, HandleSSE, SetupEngineListeners all 0% — out of scope for PR 2.

**router.go / embed.go / helpers.go:** all 0% — initialisation,
template rendering, and request-scoped helpers; not part of the handler
coverage objective.

---

## Reproduce

```powershell
cd C:\Users\stephen.brown\GitHub\shingo

# shingo-core
cd shingo-core
go clean -testcache
GOROOT="" go test -tags=docker -p 1 -coverprofile=coverage-core.out -cover ./...
GOROOT="" go tool cover -func=coverage-core.out

# shingo-edge
cd ..\shingo-edge
go clean -testcache
GOROOT="" go test -tags=docker -coverprofile=coverage-edge.out -cover ./...
GOROOT="" go tool cover -func=coverage-edge.out

# protocol
cd ..\protocol
go clean -testcache
GOROOT="" go test -tags=docker -coverprofile=coverage-protocol.out -cover ./...
GOROOT="" go tool cover -func=coverage-protocol.out
```

Note: `-p 1` is required for shingo-core to prevent testcontainers' ryuk
reaper from killing in-use Postgres containers when packages finish
concurrently.

---

## Progress log — refactor/shingo-architecture-2

Branch started from `ded0543`. Landed on top of baseline (`36ea180`):

| Commit    | PR      | Summary                                                                                     |
|---|---|---|
| `ded0543` | PR 1    | Architectural guardrails (golangci-lint rule ratcheting handlers off direct `*store.DB`)    |
| `dde7a42` | PR 2.1  | Shared test harness (`helpers_test.go`) + `handlers_api_config_test.go` (36 tests)         |
| `1b16958` | PR 2.2  | `handlers_operator_stations_test.go` (68 tests, all 19 baseline `.DB()` call sites covered) |
| `148e13d` | fixup   | Drop duplicate `seedReportingPoint` / `seedAnomalySnapshot` / `itoa` from `helpers_test.go` |
| `fc48fac` | feature | Cherry-pick of `5ef5bc0` — two-robot swap `ReleaseStagedOrders` / `/process-nodes/{id}/release-staged` |
| `51ac5dc` | test    | Stub `EngineAccess.ReleaseStagedOrders` + 2 handler tests for the new endpoint              |
| `8268b52` | PR 2.3  | `handlers_api_orders_test.go` (33 tests, per-fn 58.8–100%, all 20 DB/engine sites exercised); `stubOrderEmitter` + `seedOrder` added to harness |
| `01f19a3` | PR 2.4  | `handlers_traffic_test.go` (13 tests, 4 API endpoints + admin-gate; config file written to ConfigPath; `handleTraffic` admin-gated only because of nil PLCManager) |
| `af1d7be` | PR 2.5  | `handlers_admin_pages_test.go` (admin-gate on 5 pages + login/logout/bootstrap-first-admin; template-rendering branches skipped) |
| `fa02f0e` | PR 2.6  | `handlers_diag_manual_test.go` (all 9 `apiSendManualMessage` branches + validation; `apiReplayOutbox` validation only — RequeueOutbox needs real Reconciliation) |
| `05bcf2b` | PR 2.7  | `handlers_prod_material_test.go` (production API endpoints + `buildStationViews` / `enrichViewBinState` helpers; page handlers render templates) |
| `0e77bc9` | PR 2.8  | `handlers_kanbans_backup_test.go` (5 nil-service 501 branches + full `apiUpdateBackupConfig` matrix; kanbans template-only — accepted gap) |
| _pending_ | PR 2.9  | `handlers_manualorder_changeover_test.go` (`buildChangeoverViewData` state machine: nil process, no active, pending, switched, central tasks; page handlers render templates) |
| _pending_ | PR 3.1  | `shingo-edge/orders/manager_coverage_test.go` — 36 tests covering Manager lifecycle (CreateRetrieve/Store/Move/Complex/Ingest, Submit/Abort/Redirect/Release, HandleDelivered/ConfirmDelivery), the 8-way HandleDispatchReply switch, and ApplyCoreStatusSnapshot reconciliation; orders package 13.3% → 83.3% |
| _pending_ | PR 3.2  | `shingo-edge/store/store_coverage_test.go` — ~46 tests covering admin_users, processes, styles, shifts, payload_catalog, reporting_points, orders, operator_stations, process_nodes, process_node_runtime, counter_snapshots, hourly_counts, reconciliation (including dead-letter + critical age), style_node_claims (all swap modes + manual_swap validation), process_changeovers atomic create; bundled bugfix in `reconciliation.go` (`GetReconciliationSummary` now scans `MIN(created_at)` into `sql.NullString` to handle empty-outbox case); store package 22.5% → 79.4% |
| _pending_ | PR 3.3  | `shingo-edge/engine/engine_coverage_test.go` — 44 tests covering engine accessors, SetCoreNodes event emission, func-injection surface (SetNodeSyncFunc/SetCatalogSyncFunc/SetSendFunc/SetKafkaReconnectFunc + Request* + SendEnvelope + ReconnectKafka with/without fn), HandlePayloadCatalog upsert+prune+empty-safety, plcEmitter/orderEmitter every event branch, CoreClient against `httptest.NewServer` (success/404/500/bad-JSON/network-error/short-circuits), CoreSyncService StartupReconcile + RequestOrderStatusSync (all branches) + HandleOrderStatusSnapshots, ReconciliationService Summary/ListAnomalies/ListDeadLetterOutbox/RequeueOutbox round-trip, SendCountGroupAck envelope-build + subject assertion; engine package 40% → 50.3% (Start()-required paths in `warlink.go` 2.5%, `operator_demand.go` 7.4%, `hourly_tracker.go` 21.4%, `changeover_restore.go` / `operator_bin_ops.go` 0% remain uncovered by design) |
| _pending_ | PR 4    | shingo-core coverage PR: store/bins (audit_log, manifest JSONB), store/nodes (NGRP/LANE seeded types, station SQL fix), store (telemetry JSONB, node types/groups lifecycle), engine (auto-return wiring, ListOrders signature), www (payload templates CRUD, diagnostics replay); fixed production SQL bug in `nodes/stations.go` (unbalanced parens in `ListNodesForStation`); shingo-core total 29.0% → 56.8% |

**Test harness shape** (all in `shingo-edge/www/`):

- `helpers_test.go` — `TestMain` (ephemeral SQLite), `stubEngine` (no-op `EngineAccess`; `OrderManager()` returns a real `*orders.Manager` backed by `testDB` + `stubOrderEmitter`), routers (`newTestHandlers`, `newAdminRouter`), request helpers (`doRequest`, `authCookie`, `decodeJSON`, `assertStatus`, `assertJSONPath`), seeders (`seedProcess`, `seedStyle`, `seedOperatorStation`, `seedProcessNode`, `seedOrder`).
- `handlers_api_config_test.go` — declares `seedReportingPoint`, `seedAnomalySnapshot`, `itoa` (package-visible, reuse anywhere).
- `handlers_operator_stations_test.go` — declares `newOperatorStationsRouter`.
- `handlers_api_orders_test.go` — declares `newApiOrdersRouter`; table-drives the 5 `apiCreate*Order` decode-error cases.
- `handlers_traffic_test.go` — declares `newTrafficRouter`.
- `handlers_admin_pages_test.go` — declares `newAdminPagesRouter`, `postForm` (form-encoded POST helper for `r.FormValue`-based handlers).
- `handlers_diag_manual_test.go` — declares `newDiagnosticsManualRouter`, `sendManualPayload` (wraps the `{type, payload}` shape expected by `apiSendManualMessage`).
- `handlers_prod_material_test.go` — declares `newProdMaterialRouter`; calls `buildStationViews`/`enrichViewBinState` directly.
- `handlers_kanbans_backup_test.go` — declares `newKanbansBackupRouter`; tests cover the 5 nil-`backup` early exits with one table-driven test plus the full `apiUpdateBackupConfig` validation matrix.
- `handlers_manualorder_changeover_test.go` — calls `buildChangeoverViewData` directly via `newTestHandlers`; no router needed (page handlers render templates and are exercised only via admin-gate in PR 2.5).

**Known uncovered slices (accepted, not gaps):**

- `apiNodeChildren` / `apiPayloadManifest` in `handlers_operator_stations.go` — would require extending `stubEngine.CoreAPI()` beyond `nil`. Deferred until a PR introduces a real fake.
- PLC-health handlers in `handlers_api_config.go` — out of scope for PR 2.1.
- Template-rendering branches of any `handle*` page handler — `renderTemplate` is a no-op (h.tmpl nil) and `h.tmpl.ExecuteTemplate` direct calls panic. Coverage of these handlers comes through admin-gate redirects (where applicable) and via package-level helpers (`buildChangeoverViewData`, `buildStationViews`, `enrichViewBinState`).
- `apiReplayOutbox` RequeueOutbox path in `handlers_diagnostics.go` — needs a real `*engine.ReconciliationService`; only input validation covered.
- `handleTraffic` body — calls `PLCManager().PLCNames()` on nil; covered only via admin-gate.
- `handleConfig` / `handleProcesses` bodies — same nil-PLCManager reason; admin-gate only.
- `handleKanbans` / `handleKanbansPartial` — both render templates and are public routes (no admin-gate); no test surface in the harness.
