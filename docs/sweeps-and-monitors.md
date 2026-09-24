# Sweeps, monitors and re-evaluation paths

The inventory of every recurring or event-driven thing that can cause
replenishment to be re-evaluated, or that sweeps state on a timer. This is the
canonical list: `[[terminology]]` defines what a floor is and points here for the
passes, `[[lanes]]` points here for their order, and
`[[uop-threshold-replenishment]]` points here for what re-evaluates a threshold.

Tickers unrelated to replenishment (SSE keepalives, reconnect jitter, partition
maintenance, PLC polling, backups, retention) are out of scope; none couples to
this machinery.

## Core — threshold decision

| Mechanism | Where | Started by | Cadence | What it does |
|---|---|---|---|---|
| `startupSweep` | `engine/threshold_monitor.go` | boot | one-shot, 3s after Start | rebuilds threshold map, rehydrates episodes, evaluates every binding |
| `checkBindings` + debounce | `engine/threshold_monitor.go` | every eval path | per pass, 15s debounce | the single fire gate; emits the below-threshold signal |
| `evaluatePayload` | `engine/threshold_monitor.go` | the callers below | per event | re-reads the authoritative sum (`decisionTotalFor` = `SystemUOPForPayload`), funnels to `checkBindings` |
| `OnBinUOPDelta` | `engine/threshold_monitor.go` | Edge UOP delta | per delta | → `evaluatePayload` |
| `OnBucketApplied` | `engine/threshold_monitor.go` | bucket delta | per delta | → `evaluatePayload` |
| `handleBinUpdated` | `engine/threshold_monitor.go` | `EventBinUpdated` | every bin move | → `evaluatePayload` |
| `NoteSwapRequestContradiction` | `engine/threshold_monitor.go` | complex order received | per order | contradiction re-check |
| `OnThresholdChanges` | `engine/threshold_monitor.go` | loader config edit | per registry change | clears debounce so a new threshold takes effect at once |
| `Resync` | `engine/threshold_monitor.go` | station resync | per resync | clears the station's timers and evaluates its payloads |
| `startupSweep` | `engine/threshold_monitor.go` | boot | once | reads the open-episode set once for the pass; no rehydrate — the monitor keeps no copy |
| `reconcileThresholdBindings` | `engine/threshold_episodes.go` | demand reconciler | reconcile interval | close-only: closes episodes whose place has no binding left |

The threshold monitor reads `demand_registry` and `demand_origins` on every evaluation and holds only timers. A read error at any path decides nothing — no open, no close, no order.

**The plant-claims snapshot is a safety net, not the delivery mechanism.** Changes reach Core via `PublishChanged` on every style/claim edit, and a full snapshot goes out on every registration — including the re-register Core asks for after it restarts. The ticker only has to catch a change whose publish was lost outright, which is why it moved from 5 minutes to 60: at 5 it was ~65 messages an hour of unchanged config and 66% of everything Core discarded for expiry.

**Every fire path reads Core's count, and the Edge's lineside report decides nothing.** The report is a per-carrier checksum: `HandleLinesideLevelReport` (`messaging/lineside_report_handler.go`) upserts it and, when a row moved, `service/lineside_divergence.go` compares it against Core's replica and opens or closes `report_divergence` episodes in `bin_uop_exception` (listed on `/inventory`). It triggers no evaluation. From 2026-07-24 until the seat-count ruling of 2026-09-23 the report did decide (`lineside_decision_mode`, default `edge_reports`, in `engine/threshold_monitor_lineside.go`); the knob and the file are deleted, and rolling back is the previous build.

## Core — sweeps

| Mechanism | Where | Started by | Cadence |
|---|---|---|---|
| demand reconciler | `engine/demand_reconciler.go` | boot | `Demand.ReconcileInterval` (0 disables) |
| `AbandonStuckOrders` + 4 sibling passes | `engine/reconciliation_service.go` | boot | `Staging.SweepInterval` (5m) |
| `stagedBinSweepLoop` | `engine/engine_background.go` | boot | `Staging.SweepInterval` |
| fulfillment scanner | `fulfillment/scanner.go` | boot + 5 events | 60s ticker plus event triggers |
| `SourceabilityMonitor` | `engine/sourceability_monitor.go` | boot + bus | 2m full, 300ms debounce |
| `staleEdgeLoop` | `messaging/core_handler.go` | boot | 60s |
| RDS grace poller | `rds/poller.go` | boot | configured interval |
| `laneLivenessFloorLoop` — 3 passes, see below | `engine/engine_background.go` | boot | 60s (`laneLivenessFloorInterval`) |

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
of missing emitters — see `[[queued-order-fulfillment]]` for the releaser
doctrine that makes it readable.

## Core — per-event instruments (not sweeps)

These fire at a call site rather than on a ticker. They are listed here so this
page reads as the complete watchdog inventory, but nothing schedules them and
none of them will notice a problem on their own if the path is never taken.

| Instrument | Where | Fires on |
|---|---|---|
| `noteUngatedDigProposal` / `UngatedDigTally` | `dispatch/ungated_dig_tripwire.go` | a dig proposed without passing the gate |
| `noteDestNodeDrift` / `DestNodeDriftTally` | `engine/bin_state_drift.go` | an order's destination node disagreeing with its bins' |
| `refuseArrival` / `ArrivalRefusal` | `engine/arrival_guard.go` | an arrival that cannot be applied, carrying a reason and context |

## Edge

| Mechanism | Where | Started by | Cadence |
|---|---|---|---|
| `SweepPushLoaders` / `rePushOwnLoader` | `engine/operator_demand_loader.go` / `engine/wiring_completion.go` | register ack / window free (CLEAR, L2 landing) | one-shot / per event, own loader only |
| `SweepPushUnloaders` / `rePushOwnUnloader` | `engine/operator_demand_unloader.go` / `engine/wiring_completion.go` | register ack / window free (CLEAR, PUSH EMPTY, U2 pickup; U2 landing as fallback) | one-shot / per event, own unloader only |
| `MaybeCreateUnloaderFullIn` | `engine/operator_demand_unloader.go` | produce-role lineside release | per event |
| `recordL1Burst` | `engine/loader_burst.go` | every in-bin order | 60s window, >8 warns |
| stranded-carrier monitor | `engine/uop_stranded_monitor.go` | Start | 60s |
| demand reconciler | `engine/demand_reconciler.go` | Start | 60s |
| lineside reporter | `engine/lineside_reporter.go` | Start | 60s — one SELECT and one snapshot enqueue, under the accumulator's flush lock; counts stated as of each carrier's flushed seq |
| plant-claims snapshot | `messaging/plant_claims_publisher.go` | Start | **60m** (was 5m until 2026-08-22) |
| CATID monitor | `engine/plc_catid_monitor.go` | Start | 500ms |
| `restoreChangeoverState` | `engine/changeover_restore.go` | Start | boot once |
| `applyHoldAndReplay` | `engine/wiring_counter_delta.go` | counter delta with no bound bin | per tick |
| `StartupReconcile` | `engine/reconciliation.go` | boot + reconnect | per connect |

## Retired — do not go looking

`L1SideCycle`, `HandleDemandSignal`, `tryAutoRequest`, `StartupSweepManualSwap`.
