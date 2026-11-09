# Sweeps, monitors and re-evaluation paths

Verified against: ae059495

The inventory of every recurring or event-driven thing that can cause
replenishment to be re-evaluated, or that sweeps state on a timer. This is the
canonical list: [terminology.md](terminology.md) defines what a floor is and
points here for the passes, [lanes.md](lanes.md) points here for their order, and
[uop-threshold-replenishment.md](uop-threshold-replenishment.md) points here for
what re-evaluates a threshold.

The two loop tables are generated from each service's `Engine.Start` and list
every loop it starts, replenishment or not. Tickers started elsewhere and
unrelated to replenishment (SSE keepalives, reconnect jitter, partition
maintenance, backups, retention) are out of scope; none couples to this
machinery.

## Core — threshold decision

| Mechanism | Where | Started by | Cadence | What it does |
|---|---|---|---|---|
| `startupSweep` | `engine/threshold_monitor.go` | boot (`ThresholdMonitor.Run`) | one-shot, 3s after Start | evaluates every monitored place once, reading the open-episode set once for the pass; seeds the cold-start warm-up allowance. No rehydrate — the monitor keeps no copy |
| `checkBindings` + debounce | `engine/threshold_monitor.go` | every eval path | per pass, 15s debounce | the single fire gate; emits the below-threshold signal |
| `evaluatePayload` | `engine/threshold_monitor.go` | the callers below | per event | re-reads the authoritative sum (`decisionTotalFor` = `SystemUOPForPayload`), funnels to `checkBindings` |
| `OnBinUOPDelta` | `engine/threshold_monitor.go` | Edge UOP delta | per delta | → `evaluatePayload` |
| `OnBucketApplied` | `engine/threshold_monitor.go` | bucket delta | per delta | → `evaluatePayload` |
| `handleBinUpdated` | `engine/threshold_monitor.go` | `EventBinUpdated` | every bin move | → `evaluatePayload` |
| `NoteSwapRequestContradiction` | `engine/threshold_monitor.go` | complex order received | per order | contradiction re-check |
| `OnThresholdChanges` | `engine/threshold_monitor.go` | loader config edit | per registry change | clears debounce so a new threshold takes effect at once |
| `Resync` | `engine/threshold_monitor.go` | station resync | per resync | clears the station's timers and evaluates its payloads |
| `reconcileThresholdBindings` | `engine/threshold_episodes.go` | demand reconciler | reconcile interval | close-only: closes episodes whose place has no binding left |

The threshold monitor reads `demand_registry` and `demand_origins` on every evaluation and holds only timers. A read error at any path decides nothing — no open, no close, no order.

**The plant-claims snapshot is a safety net, not the delivery mechanism.** Changes reach Core via `PublishChanged` on every style/claim edit, and a full snapshot goes out on every registration — including the re-register Core asks for after it restarts. The ticker only has to catch a change whose publish was lost outright, so it is slow (60 minutes, `DefaultPlantClaimsSnapshotInterval`): a fast one would mostly resend unchanged config. It runs only while Core does not speak feeds: a Core that does quotes each process's claims digest back on every heartbeat ack, and the Edge re-publishes exactly the processes that differ (`ReconcileClaims`), so a lost publish heals within a heartbeat or two.

**Every fire path reads Core's count, and the Edge's lineside report decides nothing.** The report is a per-carrier checksum: `HandleLinesideLevelReport` (`messaging/lineside_report_handler.go`) upserts it and, when a row moved, `service/lineside_divergence.go` compares it against Core's replica and opens or closes `report_divergence` episodes in `bin_uop_exception` (listed on `/inventory`). It triggers no evaluation.

## Core — background loops

Every loop Core's `Engine.Start` starts, in the order it starts them.

<!-- generated: shingo-core/engine Engine.Start -->

| Loop | Where | Started by | Cadence | What it does |
|---|---|---|---|---|
| `afterQueue.run` | `shingo-core/engine/after.go` | `Engine.Start` | no ticker | run serves the queue until stop closes. |
| `DriverStarter.StartDriver` | `shingo-core/fleet/fleet.go` | `Engine.Start` | set by the implementation | DriverStarter is an optional interface implemented by fleet backends whose driver goroutine must launch AFTER the engine's event handlers are wired (the sim driver, which emits FINISHED events immediately, would fire into a dead handler before wireEventHandlers). |
| `Scanner.StartPeriodicSweep` | `shingo-core/fulfillment/scanner.go` | `Engine.Start` | 60s | StartPeriodicSweep runs the scanner every interval as a safety net. |
| `laneLivenessFloorLoop` | `shingo-core/engine/engine_background.go` | `Engine.Start` | 60s (`laneLivenessFloorInterval`) | laneLivenessFloorLoop is the lane liveness floor: the periodic pass over the two wait populations no other floor covers — gate-staged dwellers and compound legs Core has not yet handed to the fleet. |
| `OrderTracker.Start` | `shingo-core/fleet/tracker.go` | `Engine.Start` | set by the implementation | OrderTracker tracks active vendor orders and emits status change events. |
| `connectionHealthLoop` | `shingo-core/engine/engine_connection.go` | `Engine.Start` | 30s | connectionHealthLoop runs checkConnectionStatus every 30 seconds until the engine stops. |
| `robotRefreshLoop` | `shingo-core/engine/engine_background.go` | `startRobotPollLoops` | 2s (`robotPollInterval`) | robotRefreshLoop polls robot status every 2 seconds and emits EventRobotsUpdated only when the robot state has actually changed. |
| `cleaningLoop` | `shingo-core/engine/cleaning.go` | `startRobotPollLoops` | 60s (`cleaningTickInterval`) | cleaningLoop runs the cleaning tick every minute until the engine stops. |
| `lidarHoursLoop` | `shingo-core/engine/robot_lidar.go` | `startRobotPollLoops` | computed by `nextLidarRound` | lidarHoursLoop runs one round per UTC hour (nextLidarRound) for as long as the engine runs. |
| `claimSweepLoop` | `shingo-core/engine/engine_background.go` | `startClaimAndTerminateSweeps` | config `staging.sweep_interval`, else 5m (`config.DefaultStagingSweepInterval`) | claimSweepLoop periodically releases claims an order no longer owns. |
| `terminateChaseLoop` | `shingo-core/engine/fleet_terminate_chase.go` | `startClaimAndTerminateSweeps` | 60s (`terminateChaseInterval`) | terminateChaseLoop runs chaseUnterminatedOrders every minute. |
| `mapSyncLoop` | `shingo-core/engine/engine_map_sync.go` | `Engine.Start` | 5m (`mapSyncInterval`) | mapSyncLoop evaluates both gates on a slow ticker. |
| `runDemandReconciler` | `shingo-core/engine/demand_reconciler.go` | `startDemandReconciler` | config `demand.reconcile_interval` | runDemandReconciler runs reconcileDemandEpisodes every demand reconcile interval until the engine stops. |
| `ReconciliationService.Loop` | `shingo-core/engine/reconciliation_service.go` | `Engine.Start` | config `staging.sweep_interval`, else 5m (`config.DefaultStagingSweepInterval`) | Loop runs the reconciliation passes every interval until stopCh closes: the summary (logged when not ok), auto-confirm of stuck delivered orders and abandonment of stuck orders (each when its timeout is set), the stuck reshuffle-parent re-drive, the orphaned-reservation reap, the stranded-bin sweep and the shadow logs. |
| `Cache.run` | `shingo-core/dispatch/eta/medians.go` | `Cache.Start` | 10m (`refreshInterval`) | run refreshes the cache every refreshInterval until stopChan closes. |
| `ThresholdMonitor.Run` | `shingo-core/engine/threshold_monitor.go` | `Engine.Start` | once, after 3s | Run performs the startup sweep then returns. |
| `SourceabilityMonitor.Run` | `shingo-core/engine/sourceability_monitor.go` | `Engine.Start` | 2m (`sourceabilityFullInterval`) | Run does a startup full recompute, then runs the periodic full recompute until ctx is cancelled. |
| `KanbanKeeper.Run` | `shingo-core/engine/kanban_keeper.go` | `Engine.Start` | 30s (`keeperTickInterval`) | Run ticks until ctx is cancelled. |
| `Poster.Run` | `shingo-core/cms/poster/poster.go` | `runCMSPoster` | `Poster.cfg.PollInterval` | Run drains until ctx is done. |

<!-- /generated -->

Started outside `Engine.Start`, so not in the table above:

| Mechanism | Where | Started by | Cadence |
|---|---|---|---|
| `staleEdgeLoop` | `messaging/core_handler.go` | boot | 60s |
| RDS grace poller (the RDS backend's `OrderTracker`) | `rds/poller.go` | `OrderTracker.Start` | configured interval |

### The lane liveness floor — three passes, one tick

`laneLivenessFloorLoop` (defined in `engine/engine_background.go`, started by
`Engine.Start` in `engine/engine_lifecycle.go`) runs three passes on every tick,
and **the order is load-bearing** — each one re-drives machinery the next would
otherwise misread:

| # | Pass | Where | Acts or reports |
|---|------|-------|-----------------|
| 1 | `Dispatcher.SweepLaneWaiters` | `dispatch/lane_floor.go` | **acts** — re-drives waits an event should have released, writing a `lane_floor_release` recovery action naming the order and its cause |
| 2 | `Dispatcher.SweepMutualDigHolds` | `dispatch/dig_standoff_tripwire.go` | **reports** — digs waiting on each other in a closed loop that cannot self-clear (`dig_standoff_detected`) |
| 3 | `Dispatcher.SweepStalledChapters` | `dispatch/chapter_floor.go` | **acts** — a demand in `reshuffling` with an open leg; dissolves and re-queues, or records residue (`chapter_stalled_unresolvable`) |

The tripwire runs *after* the floor because the floor's re-drive clears waits
that only looked circular; asking first would report standoffs the next line
dissolves. The chapter watchdog runs last for the same reason. Dig admission is
supposed to make a mutual hold unreachable, so every one the tripwire reports is
a defect in the usable-capacity claim, not a routine event.

All three are silent at zero. Per-release logging is deliberately omitted — each
release writes its own `recovery_actions` row, and a periodic "released 0" line
would be exactly the cry-wolf the reconciliation sweeps warn about.

The floor interval is a **maximum wait**, not a poll interval: the events are the
primary release path and the floor is the backstop for when one does not fire.
The histogram of floor releases grouped by cause is therefore a ranked worklist
of missing emitters — see [queue-releasers.md](queue-releasers.md) for the
releaser doctrine that makes it readable.

## Core — per-event instruments (not sweeps)

These fire at a call site rather than on a ticker. They are listed here so this
page reads as the complete watchdog inventory, but nothing schedules them and
none of them will notice a problem on their own if the path is never taken.

| Instrument | Where | Fires on |
|---|---|---|
| `noteUngatedDigProposal` / `UngatedDigTally` | `dispatch/ungated_dig_tripwire.go` | a dig proposed without passing the gate |
| `refuseArrival` / `ArrivalRefusal` | `engine/arrival_guard.go` | an arrival that cannot be applied, carrying a reason and context |

## Edge — background loops

Every loop Edge's `Engine.Start` starts, in the order it starts them.

<!-- generated: shingo-edge/engine Engine.Start -->

| Loop | Where | Started by | Cadence | What it does |
|---|---|---|---|---|
| `Manager.sseLoop` | `shingo-edge/plc/sse.go` | `Manager.StartWarLinkPoller` | no ticker | sseLoop is the top-level goroutine for SSE mode. |
| `Manager.warlinkPollLoop` | `shingo-edge/plc/manager.go` | `Manager.StartWarLinkPoller` | `WarLinkConfig.PollRate`, else 2s | warlinkPollLoop is the poll-mode goroutine: one WarLink fetch now, then one each WarLink poll rate until the poller or the manager stops. |
| `Manager.pollLoop` | `shingo-edge/plc/manager.go` | `Manager.StartPolling` | config `poll_rate`, else 1s (`config.DefaultPollRate`) | pollLoop reads every polled reporting point each counter poll interval until the manager stops. |
| `Watch.Run` | `protocol/liveness/liveness.go` | `Engine.Start` | 5s (`checkEvery`) | Run reads the loops every 5 s until stop closes. |
| `strandedMonitor.run` | `shingo-edge/engine/uop_stranded_monitor.go` | `startStrandedMonitor` | 60s (`strandedScanInterval`) | run sweeps once at boot, then scans every node each strandedScanInterval until the engine stops. |
| `runDemandReconciler` | `shingo-edge/engine/demand_reconciler.go` | `startDemandReconciler` | 60s (`demandReconcileInterval`) | runDemandReconciler runs reconcileDemand every demandReconcileInterval until the engine stops. |
| `runLinesideReporter` | `shingo-edge/engine/lineside_reporter.go` | `startLinesideReporter` | 60s (`linesideReportInterval`) | runLinesideReporter sends this Edge's lineside report every linesideReportInterval until the engine stops (reportLinesideLevels). |
| `catidMonitor.run` | `shingo-edge/engine/plc_catid_monitor.go` | `startCatidMonitor` | 500ms (`plcPollInterval`) | run is the monitor's goroutine: a read every plcPollInterval on the process clock, each stamped with it, so every time the monitor writes (a read, a confirm, the declaration's window, a refusal, the verify deadline) is on the clock the engine stamps its rows and changeovers with. |
| `catidMonitor.runSlotWatch` | `shingo-edge/engine/catid_slot_watch.go` | `startCatidMonitor` | 1m (`slotWatchInterval`) | runSlotWatch is the watch's goroutine: a pass a minute of the process clock until the engine stops. |
| `startIntentWorker` | `shingo-edge/engine/release_act.go` | `Engine.Start` | 15s (`intentFloor`) | startIntentWorker runs the worker goroutine, the 15 s floor, and the boot sweep of every node holding an intent. |

<!-- /generated -->

## Edge — boot and per-event passes, and loops started elsewhere

| Mechanism | Where | Started by | Cadence |
|---|---|---|---|
| `SweepPushLoaders` / `rePushOwnLoader` | `engine/operator_demand_loader.go` / `engine/wiring_completion.go` | register ack / window free (CLEAR, L2 landing) | one-shot / per event, own loader only |
| `SweepPushUnloaders` / `rePushOwnUnloader` | `engine/operator_demand_unloader.go` / `engine/wiring_completion.go` | register ack / window free (CLEAR, PUSH EMPTY, U2 pickup; U2 landing as fallback) | one-shot / per event, own unloader only |
| `MaybeCreateUnloaderFullIn` | `engine/operator_demand_unloader.go` | produce release carrying `capture_lineside` (changeover evac at 0, sim operator, API client; not the station HMI) | per event |
| plant-claims snapshot | `messaging/plant_claims_publisher.go` | Start | **60m**; skipped while Core speaks feeds |
| `restoreChangeoverState` | `engine/changeover_restore.go` | Start | boot once |
| `applyHoldAndReplay` | `engine/wiring_counter_delta.go` | counter delta with no bound bin | per tick |
| `StartupReconcile` | `engine/reconciliation.go` | boot + reconnect | per connect |
