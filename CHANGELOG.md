# Changelog

One line per change. If a change needs a paragraph to explain, the paragraph
belongs in the commit message or in `docs/` — this file is the index.

## 2026-08-17 — Lane campaign

- The lane mouth is a durable reservation (`resource_kind='mouth'`) with a work direction — `inbound`, `outbound`, or `dig`; the rows own the hold, not an in-process lock.
- A dig's lane claim drops when the last blocker leaves the lane, handing the corridor to the order collecting the uncovered bin rather than releasing it into open contention.
- Blockers are never restocked — they lie where the unbury parked them and become ordinary findable inventory.
- Admission is one decision asked in one place, and refusing is the default; it answers lane safety only, with ordering left to the tiered-entry classifier.
- Reachability has one definition (`LaneBlockerPredicate`), replacing seven disagreeing spellings across the tree.
- Every queue cause declares what ends it, enforced total by test, backed by two 60s liveness floors (`SweepLaneWaiters`, `SweepStalledChapters`).
- A demand owns the lane it sourced from until the bin leaves by its mover, which generalized the source hold from `outbound` to `dig`.
- A dig dwells in the lane it is digging and yields to the dig already there; depth-1 lanes are exempt.
- A capacity group admits only the digs it can feed, and every order names its origin.
- Superseded compound generations become closed chapters, with `orders.open_for_children` making sealedness explicit (v84).
- The synthetic compound parent ("folder") is deleted and a five-symbol ban fence stands where it was, with no exceptions clause.
- Staging is declared in the Edge's cell config rather than inferred — a staging node is a station with no parent, and both destination gates stand down for it.
- Plant specs assert packed lanes and one lane's worth of group headroom at seed time (`plantspec/census.go`).
- Added the `occupancy` reservation kind (v76) — the in-lane hold, separate from the claim.
- Dropped `pending_lane_extensions` (v85, the expose bridge) and `pending_restocks` (v70, the restore-blockers subsystem).
- New lane-stress rigs (`plants/lane-stress.yaml`, `lane-stress-packed.yaml`) plus `soakstat`, `soak-watch.sh`, and a sharded CI suite.
- The sim rig's clock ran at two speeds; every measurement taken before the fix was against the wrong timeline.

## 2026-08-11 — CI and notification tuning

- GitHub Actions updated to Node 24 compatible versions.
- Fault notification throttle removed and the buffer reduced to one minute.
- Threaded fault/cleared emails, time-faulted display, and a test-chain button.

## 2026-08-05 — Robot localization confidence

- Confidence is sampled off the robot poll Core already makes, rather than a new feed.
- Migration 77 adds the confidence tables, with daily roll-ups kept forever and raw samples aged out.
- Roll-ups key on the geometry a reading was actually taken on, and store the distribution because percentiles do not re-aggregate.
- `confidence <= 0` is a ReflectorArea sentinel, not a robot position — excluded everywhere.
- A four-band scheme (good/fair/watch/poor, plus blind) drives the map; the HMI chip stays on the vendor's 0.8/0.3.
- Per-lane-per-robot grain (v83) lets the map filter to one AMR's world.
- An unnameable lane is quarantined rather than renamed, and an unpolled scene is not an empty one.
- The roll-up cannot hang off a 24-hour ticker on an 11-hour process.

## 2026-08-06 — Scene map and localization board

- Core fetches and versions the scene map RDS refuses to expose, via `POST /generalRobokitAPI`.
- Scene areas and reflectors that RDS throws away are parsed and stored.
- A lane is fingerprinted as one piece of floor drawn twice, so the two halves reconcile.
- Scene sync diffs before overwriting and refuses a lane it cannot name.
- The robots page becomes the localization board, drawing on an extracted scene-drawing substrate a second map can reuse.
- A map edit now has a magnitude and an owner.

## 2026-08-02 — Core owns loader replenishment

- Core owns each loader's replenishment settings; the Edge's half is deleted.
- Every loader empty routes through one reservation seam, and every order through one path into the orders table.
- A loader's declared carrier mix is honoured when choosing an empty.
- A full carrier only goes to a window whose job is to empty it.
- The loader form is rebuilt around the kind of loader it is.
- The order API moves behind the admin gate, with queueing split from replying.
- A child order gets its own payload, source and reason.
- `ClaimSync` is deleted on both sides — the Core handler had been unreachable the whole time.

## 2026-07-30 — Changeover cancellation and supply refusals

- A changeover cancels the orders it makes pointless, and blocks only when a robot is actually doing something.
- An acknowledged order for the old style no longer outlives the changeover.
- The cutover gate asks where the bin lands, not which slot points at it.
- A loader can refuse a supply call and take it back; the refusal reaches Core, and Core tells everyone.
- Refusals are notice-only — they never load and never gate demand.
- The loader board reds a queued call with nothing coming, and marks ACTIVE styles.
- A starving cell reads red on a dedicated home, against the configured threshold.

## 2026-07-28 — Edge identity, foreign-key repair, demand forensics

- Core mints Edge identity, making the duplicate-station failure inexpressible; enrollment becomes mandatory.
- Two Edges took turns owning one registry row, and the statement that allowed it deleted the evidence.
- A foreign-key audit on a real plant database found live defects, and the generator that was still producing them.
- Springfield stopped counting for three and a half days because PLC status is only pushed on a change.
- Open one demand and read every order it caused, with the child-order mix and orphan rate.
- Cycle time from the counter trail: one distribution per station, part and direction.
- Station display names resolve at render time and are never stored.
- The retention ladder gets its permanent rung, bounding the tables that grow forever.

## 2026-07-22 — Nodes page cleanup, map token promotion, template-var check

- Node tiles move onto the shared palette, which lets the dark-mode overrides drop and renames the undefined `var(--card-bg)`.
- The dead "Check Occupancy" block, which errored on live plants, is removed.
- Map floor-plan colours and the map JS's two local hex palettes move into `shared/tokens.css`, with no visual change.
- New `TestNoUndefinedCSSVarsInTemplates` fails CI when a template references a `var(--foo)` no stylesheet defines.

## 2026-07-22 — Inventory page rebuilt (v2), exception-first

- New `GET /api/inventory/monitor-totals` returns DB on-hand, the monitor's cached total, thresholds and description per payload.
- `ThresholdMonitor.Snapshot()` reads the monitor's in-memory totals and bindings under its lock.
- Lineside buckets carry `updated_at`, so the page can colour a bucket that has not moved in a month.
- The page leads with a Replenishment Health table, a five-tile KPI strip, and a conditional alerts banner.
- The old bins table moved to `/bins`; consumption, produced and cycle-time moved to the per-payload drill and Missions.
- The consumption drill shows window totals rather than a daily trend, because consumption is recorded per part number and does not map one-to-one to a payload.

## 2026-07-22 — Map: zoom/pan and truthful route rendering

- Wheel-zoom at the cursor and left-drag to pan, with a Recenter button that resumes auto-follow.
- Route comets point along the leg the robot is actually driving, inferred from proximity to the source and locked in at pickup.
- Empty runs draw hollow dots and loaded runs solid; a stopped robot shows a static line, a faulted one a red line.
- The map has no automated UI test — the leg inference needs a running scene to confirm visually.

## 2026-07-22 — UI refresh groundwork: tokens, icons, cleanups

- Design tokens add a five-step type scale, a 4px spacing scale, teal and diverging ramps, and motion tokens with a reduced-motion rule.
- A ~22-icon Lucide subset is vendored as one SVG sprite, inlined once into the Core layout.
- `loaders.js` uses the styled confirmation dialog instead of native `confirm()`; `diagnostics.js` colours move to theme tokens.
- Visible "UOP" becomes "UoP" across six pages, leaving code names and JSON keys alone.
- The chart palette is reordered so two adjacent series no longer collapse for red-weak viewers.
- New `TestNoEmojiInTemplatesAndPageJS` fails on any emoji in a template or page script.

## 2026-06-12 — Bin-loader multi-window refactor (C0–C4a)

- **C0** adds `domain.Loader`, a first-class Edge aggregate whose two constructors make invalid states unconstructible.
- **C0** adds typed `LoaderID` / `NodeID` / `PayloadCode` newtypes as a compile-time guard against counts keyed on the wrong node string.
- **C0** puts an explicit position `kind` (`window` | `dedicated`) on the wire, replacing the empty-payload-means-window convention.
- **C1** makes loader empty-in creation atomic per loader via `reserveLoaderEmpties`, a per-`LoaderID` mutex serialising count→fire.
- **C1** pins the re-entrancy rule: no order-event subscriber may synchronously re-enter the seam for the same loader.
- **C2** collapses the two loader-config duals behind a consumer-defined `LoaderStore` interface, resolving from an atomically-swapped immutable snapshot rather than the DB.
- **C2** fails closed on a real store error so a DB flicker drops a demand signal instead of rerouting it.
- **C3** makes the reservation seam Loader-first, retiring the `manualSwapNode` shim as the unit of resolution.
- **C4a** activates multi-window delivery behind a default-off flag: one demand of N yields exactly N empties, one per window, never 2N.

## 2026-05-17 — Test infrastructure cleanup (Phases 1–6)

- Stripped the `TC-*` test-comment scheme and renamed 25 `TestTC##_*` functions to `Test<Subject>_<Behavior>` form.
- Deleted `shingo-core/docs/test-catalog.md`; it covered 70 of 262 tests and admitted in-doc it was outpaced.
- Hoisted `Eventually` / `AssertEventually` to `protocol/testutil/` and replaced 8 sleep-based polling loops.
- Annotated 14 intentional `time.Sleep` calls with KEEP headers so the intent is unambiguous.
- Split `engine_test.go` (1734 LOC) into 7 behavior-clustered files and renamed 19 `*_coverage_test.go` files.
- Added `t.Parallel()` to ~1,600 top-level test functions, unlocking the per-test DB isolation already in place.
- Added `-race` to core, edge and protocol CI as warning-only for 30 days before gating.
- Added `testutil.MustNoErr` and migrated ~1,224 error-check sites across 139 files, for -2,342 LOC.
- Added a local-only `make test-explain` target for the bins-store EXPLAIN-plan regression harness.
- Fixed the TC-60 "10-step swap" discrepancy in the fleet-simulator docs — the code is a 9-step sequence.

## 2026-05-06 — Dispatch coverage, retrieve-algorithm collapse

- Collapsed the FIFO/COST/FAVL retrieve variants into one algorithm parameterized by sort key.
- Closed test gaps in `classifyEmptyGroup`, lane-lock acquire/release, and store-strategy routing.

## 2026-05-05 — UOP runtime cache, side-cycle guards, debug log UI

- The runtime UOP cache binds to release-click and `OrderDelivered` rather than operator confirm, which had let the HMI show stale UOP for the whole delivery window.
- A `skip_auto_confirm` flag stops reconciliation re-confirming side-cycle orders the swap dispatcher already confirmed.
- The operator station gains a RELEASE button for two-robot swap during changeover.
- `SituationAdd` delivers directly to lineside, skipping the staging hop that created phantom in-flight orders.
- Loader payload cards render NO DEMAND instead of a false QUEUED badge.
- Debug log gains colour grouping by source and level, JSON expand on click, and a plant-floor-readable palette.

## 2026-05-04 — Failed order recovery, Edge as UOP authority

- `failed` is no longer terminal: orders enter a non-terminal `faulted` status with a grace period during which the robot can resume without operator intervention.
- Bins become the source of truth for `remaining_uop` with Edge as sole writer; Core's re-deriving reconciler is deleted.
- Race-safe upserts on `process_node_runtime_states`, `style_node_claims` and `node_lineside_bucket`.
- A new Lineside Buckets page lets engineering override slot UOP capacity without a YAML edit and restart.
- Operator HMI keypad and release prompt scaled for 7" displays; failure toasts carry the actual error string.
- Stripped UTF-8 BOMs from Go sources, which Go 1.25 rejects.

## 2026-05-03 — Coverage reporting, fuzz targets, changeover floor

- CI coverage reporting plumbed through all three modules with merged HTML uploaded as an artifact.
- Store tests moved from the facade into the per-aggregate sub-packages so coverage numbers mean something.
- Protocol fuzz targets added for envelope decode, order validation, and waybill round-trip.
- Mode-aware changeover step builders consolidated so phase 2 reuses phase 1 rather than diverging.

## 2026-05-02 — UOP bin-as-truth refactor

- Bins gain `remaining_uop_cached` so HMI rendering and dispatch read one column, retiring the compute-from-claim path from hot paths.

## 2026-05-01 — Bin transit state, forklift-scale loader, migration hardening

- `bin_transit_state` gives Core explicit visibility into the delivery cycle (`parked → claimed → en_route → at_destination → released`), replacing inference from order status plus bin node.
- A UOP audit at each transition flags drift before it reaches the operator.
- The bin-loader board is redesigned for tablet-on-fork use, with large tap targets and demand grouped by payload.
- Each migration runs in its own transaction, so a partial failure leaves the DB at the previous version.
- Startup self-heal repairs migrations that left broken state from before the per-version wrapping.
- Complex orders bypass the dropoff-capacity gate, which was designed for simple deliveries and blocked them spuriously.

## 2026-04-30 — Press-index, typed constants, two-robot release polish

- An optional `second_paired_core_node` supports press cells with a back-side material position.
- Two-robot release auto-picks the correct bin from the manifest when a lane holds mixed payloads.
- Release is accepted on `in_transit` orders so two-robot fan-out can release Robot B while Robot A is still moving.
- Order status, order type, claim role and bin status string literals migrate to typed constants.
- The Plan/Apply pattern is formalized for swap dispatch — `Plan` resolves without side effects, `Apply` commits.
- `/static/*` is cache-busted per restart via ETag, so operators no longer hard-refresh after a deploy.

## 2026-04-29 — Side-cycle relocation, replenishment, press-index

- The L1 side-cycle trigger moves from `OrderDelivered` to release-click, making it a decision the operator can abort.
- The delivery envelope carries the bin's `uop_remaining`, so replenishment uses the arrived UOP rather than a later lookup.
- New `two_robot_press_index` mode pre-stages material at the back while the previous cycle finishes at the front.
- `NODE_GROUP` and its direct physical children are treated uniformly as storage slots by the dispatcher.

## 2026-04-28 — Side-cycle hardening, floor-jam guards, JS cleanup

- `payload_bin_types` becomes advisory rather than strict-enforce, matching how the plant actually uses it.
- The L2 outbound move always auto-confirms, instead of waiting at `delivered` for a click that was not coming.
- A stale-bundle guard refuses to dispatch a swap bundle when an underlying order went terminal after the bundle was built.
- Half-built complex orders are rejected at build time rather than reaching the dispatcher.
- Operational queries skip retired bins, which had been producing phantom matches.
- `operator.js` and `nodes.js` are split into ES modules; `location.reload` is replaced by DOM patching on SSE paths.
- Dead code deleted: the `ConfirmNodeManifest` stack, `forceTransitionOrder`, `DeleteLinesideBucket`, `operator-canvas/`.
- Release endpoints require `called_by`, and unhandled dispositions log at WARN with the resolved mode.

## 2026-04-27 — Architecture finishing pass, side-cycle bin loader

- The depguard ratchet is drained: import rules tightened to the post-Stages-1-9 architecture and remaining violations fixed.
- Kanban auto-request is deleted entirely; side-cycle subsumes it and surfaces line demand on the loader UI.
- A node never appears as a source for an order whose destination is itself.
- Two-robot release gates on Robot B staged then fans out to both, removing the Robot-A-first ordering that created stuck cycles.
- Confirm binds to the actual delivery receipt rather than order status, fixing the two-robot teleport.
- SEER battery level (0.0–1.0) is scaled at the mapper for 0–100 display.

## 2026-04-26 — Stage 10: order state machine, Phase 6/7 parity

- The order state machine moves behind a typed `Lifecycle` facade with explicit pre/post-conditions on every transition.
- Compile-time exhaustive switching on the `Status` enum catches missed transitions.
- The Edge module gains the same narrow-interface treatment Core got in Stages 5–9.

## 2026-04-25 — Two-robot release consolidation

- Robot A and Robot B receive their release in a single transaction.
- A stuck-cycle guard fires after a configurable timeout when the fan-out hangs.
- An admin disposition path lets engineering unstick a cycle from diagnostics without restarting Core.

## 2026-04-24 — Dispatch skip-reason visibility

- Skip-reason logs route through the ring buffer so "why didn't this dispatch" is answerable from the web UI.

## 2026-04-23 — Two-robot release manifest, supply-bin guard

- Fixed the two-robot manifest stuck at `SMN_003` via a bin-lookup fallback plus a runtime-reset guard.
- Supply-bin status is tracked per order so partial failures roll back without dragging siblings down.
- The bin manifest binds at operator release-click rather than order creation, so payload can be reassigned up to release.

## 2026-04-22 — Lineside inventory, multi-payload manual_swap

- Edge tracks inventory per lineside slot and resets the bucket counter on release.
- A process's PLC reporting-point change updates the tag binding without a service restart.
- Every node with active demand highlights on the operator board, not just the most recent.
- Fixed multi-payload `manual_swap` loading, which rejected loads the claim's allowed list permitted.

## 2026-04-21 — Architecture overhaul Phases 1–5, demand sync

- Module-boundary cleanup, naming alignment, depguard rules, and initial shared-lifecycle extraction across core, edge and protocol.
- `core/demand_registry` reaps stale entries when an edge stops reporting, ending phantom demand from a powered-off station.
- Edge pushes claim upserts and deletes to Core on UI action rather than waiting for the next heartbeat.
- A cleared bin can satisfy a `manual_swap` request at a concrete lineside node.

## 2026-04-19 — Two-robot single-click release, toolchain downgrade

- One operator click releases both robots in a two-robot swap, resolving siblings via the durable claim pointer.
- Rolled Go 1.26.2 back to 1.25.0 across all modules — 1.26.2 rejected BOM-prefixed sources that Windows editors had introduced.

## 2026-04-18 — E-Maint robot telemetry stub

- `/api/telemetry/e-maint` and `/download` expose per-robot maintenance telemetry (odometer, runtime, jack/lift, voltage, current, controller state).
- `fleet.RobotStatus` gains 14 telemetry fields, populated by the SEER mapper.
- An E-Maint tab on Diagnostics renders from the in-memory robot cache.

## 2026-04-18 — Architecture refactor: Stages 1–9 (shingo-core)

- **Stage 1** replaces `engine.DB()` with 116 named single-purpose query methods on `EngineAccess`, so handlers stop picking arbitrary queries off a shared handle.
- **Stage 2A** lifts pure data types into a persistence-free `domain/` package, with aliases keeping every call site compiling.
- **Stage 2C** splits `engine/wiring.go` (~1050 LOC) into per-concern siblings, keeping the master registry in one place.
- **Stage 2D** decomposes flat `store/` into `bins/`, `nodes/`, `orders/`, `payloads/`, with cross-aggregate methods staying at the outer level.
- **Stage 3** pilots the service layer with `BinService`, moving validation and mutation out of the bins handler.
- **Stage 4** follows with `OrderService` and `NodeService`, consolidating the duplicated node-assignment flow.
- **Stage 5** extracts `dispatch/binresolver/` behind a 14-method Store interface, with 19 fake-backed tests and no DB fixtures.
- **Stage 6** extracts `material/` (CMS transactions), taking `cms_transactions.go` from 246 to 74 LOC at 89.6% coverage.
- **Stage 7** splits `engine.go` into eight files and extracts `fulfillment/` behind a consumer-side Store interface.
- **Stage 8** collapses a three-line dedup guard copy-pasted across eight handlers into an `InboxDedup` decorator.
- **Stage 9** adds consumer-side narrow interfaces, extracts `scenesync/`, and gates 39 Postgres-backed test files behind `//go:build docker`.
- `protocol.RawHeader` gains `Src`, letting routing identify the sender without a full payload decode.

## 2026-04-17 — Toolchain bump, dead-symbol flagging

- Go 1.25.0 and x/crypto v0.48.0 pinned uniformly across all three modules.
- New `protocol/version_test.go` fails if Go or x/crypto versions drift apart again.
- Six callsite-free symbols annotated `TODO(dead-code)` for a later pruning pass rather than removed.

## 2026-04-16 — Traffic page, fire alarm toggle

- Operators configure heartbeat/PLC bindings and count-groups from a Traffic tab instead of editing YAML.
- Core count-group changes persist to `shingocore.yaml` and hot-reload the Runner without a restart.
- The fire alarm feature and its auto-resume default are togglable from the web UI.

## 2026-04-15 — Count-group light alerts, fire alarm pass-through

- Core polls RDS per count group and emits Kafka commands Edge translates into PLC tag writes, for advanced-zone safety lighting.
- N-of-M hysteresis (on 2, off 3) prevents flicker, with ON biased to commit faster than OFF.
- A fail-safe timeout forces lights ON after sustained RDS communication failure.
- Stale-group warnings escalate when a group never reports occupied (WARN at 5m, ERROR at 30m).
- Fire alarm activate/clear relays to RDS via `/isFire` and `/fireOperations`; RDS owns all robot logic.
- Both features are config-gated, with an empty group list starting no polling goroutine.

## 2026-04-14 — Order failure hardening, bin protection

- Refuse to create or move bins onto already-occupied physical nodes, preventing stacking.
- Lineside bins are protected from poaching by staging logic.
- Edge is notified on order failure; the broken auto-return is disabled pending redesign.
- Async order failures persist as a sticky toast on the operator HMI instead of disappearing.

## 2026-04-13 — Wait block, operator UX, route visibility

- Replaced pre-position dropoff with the RDS native Wait block, eliminating dummy location visits.
- Operators can load an empty bin already at the node without waiting for a delivery order.
- Mission detail and test-order pages show the full block-by-block route.
- New consumer-side `EngineAccess` interface (26 methods) plus an `EventBus()` accessor; all 14 `.Events.Emit()` calls migrated.
- `ListPendingOutbox` and `ListDeadLetterOutbox` were missing `sent_at` in SELECT and Scan, misaligning row scans.
- seerrds mappers import status constants from `protocol` rather than `dispatch`, removing an adapter-to-orchestration import.

## 2026-04-12 — Cross-module deduplication

- Duplicated types and helpers extracted into shared packages across core, edge and protocol.
- Inline test assertions replaced with `testdb` helpers across core tests.

## 2026-04-11 — Structural refactoring

- Characterization tests added to lock behavior before five structural refactors across core and edge.
- Shared helpers extracted from changeover and demand code.

## 2026-04-10 — Bin loader/unloader multi-order queue

- Loader and unloader nodes support queued multi-order workflows with automatic kanban-style demand generation.
- Fixed bin arrival on delivery, a cancel guard, and transition idempotency, all found in plant testing.

## 2026-04-09 — Bin loader stabilization, URL encoding

- Fixed wrong UOP count, a missing confirm step, and stale HMI state after load operations.
- Bin movement auto-confirm works, with a claim-level auto-confirm setting for loader nodes.
- `bin_loader` retrieve-empty orders skip the staging step.
- Fixed URL encoding for PLC names, tag names, node names with spaces, and manifest paths in Edge HTTP clients.

## 2026-04-08 — Migration repairs, node guards

- Migrations v11–v13 repair `payload_bin_types`, `payload_manifest` and `node_payloads` foreign keys referencing the stale `blueprints` table.
- Reparent and delete guards use structural error classification to prevent orphaning nodes.
- Payload template save no longer silently discards bin-type and manifest errors.

## 2026-04-07 — Diagnostics and move-order fixes

- Fixed diagnostics tabs not displaying content, from a CSS `hide` class conflicting with tab switching.
- Fixed move orders from an NGRP source not updating bin location — `planMove` was missing group resolution.

## 2026-04-06 — Edge cancel, operator HMI fixes

- Fixed cancel notification delivery to edge stations.
- Added cache-busting to prevent stale operator HMI state after actions.
- Fixed the CONFIRM button not appearing after delivery.

## 2026-04-05 — Operator HMI simplification

- Removed the rarely-used release-empty and release-partial actions, adding a manifest-confirm action for verification at delivery.

## 2026-04-01 — Changeover automation, production hardening

- Automated style changeover lands (Phases 1–5): abort in-flight orders, A/B cycle material slots, dispatch new material, confirm completion.
- Bins staged at lineside are preserved across a changeover when the payload is shared between styles.
- Production is timestamped at cell completion for FIFO audit traceability.
- Fixed compound orders confirming before all children completed, and orphaned bin claims from non-atomic terminal transitions.
- Complex orders fail at planning when no bin is available, giving immediate feedback instead of a silent dispatch failure.
- Orders queue on `claim_failed` rather than failing permanently.
- Staged order vehicles are pinned on release so RDS does not re-dispatch to a different robot.

## 2026-03-30 — FIFO retrieval, bin dispatch fixes

- Strict FIFO retrieval enforced across all paths, with a COST mode added for NGRP lanes.
- `planRetrieveEmpty` detects buried empty bins and triggers a reshuffle to reach them.
- Bins are claimed at dispatch time, preventing races; staged bins at core nodes become claimable.
- `.gitattributes` added and all files normalized to LF.

## 2026-03-29 — Compound order fixes

- Cancelling a compound parent cascades to all children.
- `maybeCreateReturnOrder` correctly sets `SourceNode`.
- Added the `order_bins` junction table for complex orders moving multiple bins.

## 2026-03-28 — FIFO, concurrency testing

- Oldest eligible bin always retrieved first from NGRP lanes.
- Fleet-simulator framework added for deterministic multi-robot scenario testing, with 9 initial tests.

## 2026-03-27 — Dispatch safety

- Refuse to dispatch when no bin is available at the source node.
- Bin claims released when the fleet reports order failure.

## 2026-03-26 — Performance, SSE stability, UI polish

- PostgreSQL connection pool limits added and made configurable from the Config page.
- Order enrichment and robot handlers read the in-memory cache, eliminating N+1 fleet round-trips.
- Client-side SSE debounce prevents DOM rebuild bursts from freezing the browser during telemetry floods.
- The SSE `/events` endpoint moves outside Chi's compression group, which had been buffering flushes and delaying disconnect detection.
- A `beforeunload` listener closes the EventSource, freeing one of the browser's six per-origin connections.
- Complex orders now specify `JackLoad`/`JackUnload` bin tasks — robots had been navigating without picking up or dropping off.
- The Completed tab splits into Delivered (amber) and Confirmed (green).
- Form elements get explicit background and color, fixing dark controls rendering in light mode.

## 2026-03-25 — Universal node naming alignment

- `pickup_node` / `PickupNode` renamed to `source_node` / `SourceNode` across protocol, schema, Go structs, handlers, dispatch, UI and docs — a wire-breaking change requiring core and edge to deploy together.
- Complex-order test form fields renamed to match style/cell vocabulary (`InboundSource`, `InboundStaging`, `OutboundStaging`, `OutboundDestination`).
- `outbound_source` renamed to `outbound_destination` on `style_node_claims` — it was a dropoff destination, not a source.

## 2026-03-24 — Queued order fulfillment

- Orders that cannot be immediately fulfilled become `queued` rather than failed, and Core fulfills them FIFO as inventory frees.
- An event-driven fulfillment scanner triggers on bin arrival, manifest clear, and order completion, with a 60s safety sweep and startup recovery.
- `ClaimBin` makes fulfillment claims atomic between concurrent attempts.
- A node-vacancy guard skips fulfillment when the delivery node already has an in-flight delivery.
- Fleet dispatch failure re-queues the order as transient rather than failing it.
- `payload_code` added to Core's orders table (v8) so the scanner can match without re-resolving the original request.
- Loader tiles show AWAITING STOCK in amber while a queued order is active.

## 2026-03-24 — Bin loader nodes, Core telemetry API, NodeGroup removal

- New `bin_loader` claim role for nodes where forklifts load untracked material into existing bins.
- Allowed payload codes on `style_node_claims` restrict which payloads a loader accepts.
- Load Bin and Clear Bin actions added to the operator station and material page.
- New Core telemetry HTTP endpoints replace Kafka for synchronous reads (node bins, payload manifest, node children, bin load, bin clear).
- Edge `CoreClient` makes on-demand calls with a 3s timeout and degrades gracefully when Core is unreachable.
- `NodeGroup` removed from `ComplexOrderStep` — Core auto-detects NGRP nodes as simple orders already did.
- Four edge claim source columns collapse to two (`inbound_source`, `outbound_destination`).

## 2026-03-23 — Delivery cycle modes: sequential, single robot, two robot

- Two source/destination routing columns added to `style_node_claims`, separate from staging areas, each accepting a node or a group.
- Sequential mode staggers two robots, auto-creating Order B when Order A goes `in_transit`.
- The single-robot swap sequence corrected from 7 to 10 steps.
- Two-robot validation requires only `InboundStaging`, since the removal robot goes direct to `OutboundDestination`.

## 2026-03-21 — Lifecycle, messaging, and recovery hardening

- Core persists inbound message IDs and suppresses replayed mutating commands before they reach dispatch.
- Delivery receipts fail closed and duplicate receipts are ignored.
- Core control and data replies use the same durable outbox-backed delivery as dispatch replies.
- Reconciliation detects completion drift, stale claims, stuck orders, expired staged bins, stale edges, dead letters and outbox backlog age.
- Audited recovery actions added for completion drift, stale terminal claims, staged-bin release, dead-letter replay and stuck-order cancellation.
- Edge no longer transitions to `confirmed` if the delivery receipt cannot be durably enqueued.
- Edge requests authoritative order status from Core on startup and re-registration.
- Dispatch planning routes through registered per-order-type planners instead of a hardcoded switch.

## 2026-03-21 — Edge production hardening, domain rename

- Edge domain model renamed to match usage: `Payload`→`MaterialSlot`, `ProductionLine`→`Process`, `JobStyle`→`Style`, `LocationNode`→`Node`, `Resupply`/`Removal`→`PrimaryOrder`/`SecondaryOrder`, with automatic `ALTER TABLE RENAME` on startup.
- API routes renamed to match (`/api/payloads/*`→`/api/material-slots/*`, `/api/lines/*`→`/api/processes/*`, and so on).
- Cancel messages enqueue before the local transition, so a robot cannot continue on a locally-cancelled order.
- Orders stay `pending` if the envelope fails to build or enqueue, preventing stuck `submitted` orders Core never receives.
- If a removal order fails after resupply succeeds, the resupply is automatically cancelled.
- A payload slot resets from `replenishing` to `active` when order creation fails, so auto-reorder can re-trigger.
- Production reporter deltas are restored on outbox enqueue failure, ending silent data loss.
- Navigation restructured into three public tabs plus an Admin dropdown, with Production, Manual Order and Operator moved behind admin login.
