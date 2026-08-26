# Sweeps, monitors and re-evaluation paths

Every recurring or event-driven thing that can cause replenishment to be
re-evaluated, or that sweeps state on a timer. Compiled 2026-08-02 because the
count had grown across several eras and nobody could say what the whole set was.

Tickers unrelated to replenishment (SSE keepalives, reconnect jitter, partition
maintenance, PLC polling, backups, retention) are deliberately out of scope; they
were enumerated during the audit and none couples to this machinery.

## Core — threshold decision

| Mechanism | Where | Started by | Cadence | What it does |
|---|---|---|---|---|
| `startupSweep` | `engine/threshold_monitor.go` | boot | one-shot, 3s after Start | rebuilds threshold map, rehydrates episodes, evaluates every binding |
| `checkBindings` + debounce | `engine/threshold_monitor.go` | every eval path | per pass, 15s debounce | the single fire gate; emits the below-threshold signal |
| `evaluatePayload` | `engine/threshold_monitor.go` | six callers below | per event | re-reads the authoritative sum, audits, funnels to `checkBindings` |
| `OnBinUOPDelta` | `engine/threshold_monitor.go` | Edge UOP delta | per delta | → `evaluatePayload` |
| `OnBucketApplied` | `engine/threshold_monitor.go` | bucket delta | per delta | → `evaluatePayload` |
| `handleBinUpdated` | `engine/threshold_monitor.go` | `EventBinUpdated` | every bin move | → `evaluatePayload` |
| `OnLinesideReports` | `engine/threshold_monitor_lineside.go` | Edge report | ~60s | decides in `edge_reports` mode, audits only in `ledger` mode |
| `NoteSwapRequestContradiction` | `engine/threshold_monitor.go` | complex order received | per order | contradiction re-check |
| `OnThresholdChanges` | `engine/threshold_monitor.go` | loader config edit | per registry change | clears debounce so a new threshold takes effect at once |
| `Resync` | `engine/threshold_monitor.go` | station resync | per resync | re-engages payloads, clears debounce, fires already-below |
| `engagePayloads` | `engine/threshold_monitor.go` | config edit / Resync | per edit | the only path that deliberately fires on a read failure |
| `rehydrateThresholdEpisodes` | `engine/threshold_episodes.go` | inside `startupSweep` | boot | rebuilds open-episode maps — without it every restart doubles open demand |
| `reconcileThresholdBindings` | `engine/threshold_episodes.go` | demand reconciler | reconcile interval | closes episodes whose binding vanished |

**The plant-claims snapshot is a safety net, not the delivery mechanism.** Changes reach Core via `PublishChanged` on every style/claim edit, and a full snapshot goes out on every registration — including the re-register Core asks for after it restarts. The ticker only has to catch a change whose publish was lost outright, which is why it moved from 5 minutes to 60: at 5 it was ~65 messages an hour of unchanged config and 66% of everything Core discarded for expiry.

**The lineside read-model decides, it does not shadow.** In `edge_reports` mode — the default — the Edge reports carry the adjustment the fire gate acts on. The file was named `threshold_monitor_shadow.go` until 2026-08-22; it is now `threshold_monitor_lineside.go`.

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

`laneLivenessFloorLoop` (started at `engine_lifecycle.go:117`) runs three passes
on every tick, and **the order is load-bearing** — each one re-drives machinery
the next would otherwise misread:

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
| `SweepPushLoaders` / `MaybePushLoader` | `engine/operator_demand_loader.go` | register ack / window free | one-shot / per event |
| `SweepPushUnloaders` / `MaybePushUnloader` | `engine/operator_demand_unloader.go` | register ack / window free | one-shot / per event |
| `MaybeCreateUnloaderFullIn` | `engine/operator_demand_unloader.go` | produce-role lineside release | per event |
| `recordL1Burst` | `engine/loader_burst.go` | every in-bin order | 60s window, >8 warns |
| stranded-carrier monitor | `engine/uop_stranded_monitor.go` | Start | 60s |
| demand reconciler | `engine/demand_reconciler.go` | Start | 60s |
| lineside reporter | `engine/lineside_reporter.go` | Start | 60s |
| plant-claims snapshot | `messaging/plant_claims_publisher.go` | Start | **60m** (was 5m until 2026-08-22) |
| CATID monitor | `engine/plc_catid_monitor.go` | Start | 500ms |
| `restoreChangeoverState` | `engine/changeover_restore.go` | Start | boot once |
| `applyHoldAndReplay` | `engine/wiring_counter_delta.go` | counter delta with no bound bin | per tick |
| `StartupReconcile` | `engine/reconciliation.go` | boot + reconnect | per connect |

## Redundant pairs — candidates, not rulings

Listed so the merge/delete decision is made deliberately. Each still needs the
earn-the-abstraction test applied before anything is removed.

1. **`SweepPushUnloaders` vs `MaybePushUnloader`** — identical bodies; both call
   `pushUnloadersViaSeam()`. The sweep is the other plus a re-entrancy latch.
2. **`SweepPushLoaders` vs `MaybePushLoader`** — same walk; the sweep adds a warn
   and a count log.
3. **`OnBinUOPDelta` / `OnBucketApplied` / `handleBinUpdated`** — three
   subscriptions into one funnel. A single bin move trips at least two. The 15s
   debounce is what hides it, and the monitor's own comment says so.
4. **`Resync` vs `startupSweep`** — the same work under a different trigger.
5. **`stagedBinSweepLoop`'s orphan-claim release vs `ReapOrphanedReservations`** —
   two loops, overlapping cleanup, both off `Staging.SweepInterval`.
6. **`AdvanceStuckReshuffleParents`** runs at boot and on every tick.
7. **fulfillment ticker vs its five event triggers** — self-described safety net;
   pure redundancy when healthy.
8. **`manual_swap_recheck` vs `handleBinUpdated`** — an operator swap is evaluated
   twice inside one debounce window.

## Died with the Edge threshold path — DONE

Deleted when Core took over the decision. They existed only to service the
below-threshold signal arriving at Edge:

`HandleLoopBelowThreshold` · `parkThresholdSignalIfCold` and its
`pendingThreshold` / `loaderCacheWarmed` state · `warmLoaderCacheAndReplay` and
its call site in `core_loaders.go` · the threshold origin-id seam ·
`fireThresholdL1` and the `L1LoopThreshold` source · the wire subject and its
registration · the Edge's copy of the sizing arithmetic (both transcriptions)
and the parity sweep that compared it against Core's.

**`MisconfiguredThreshold` was on this list and should not have been.** It is
called from `SweepPushLoaders`, which the section below correctly marks as
surviving — so this document contradicted itself. It also shares its predicate
with the check that gates the entire operator push: the second half of that gate
*is* this function. It survives.

**Survives the deletion — do not remove alongside:** the push sweeps
(`SweepPushLoaders` / `SweepPushUnloaders` / `MaybePush*`) are the operator-staging
path and independent of thresholds; `recordL1Burst` is path-agnostic and also
fires on the unloader side; the stranded monitor, the Edge demand reconciler, and
the lineside reporter all stand alone. The reporter becomes *more* load-bearing,
since in `edge_reports` mode it is Core's only Edge-truth input.

**Core is unaffected** by the Edge deletion: the monitor, the sweep, the gate, all
four reason strings and the episode machinery are Core-side and change only at
the emit boundary.

## Retired — do not go looking

`L1SideCycle`, `HandleDemandSignal`, `tryAutoRequest`, `StartupSweepManualSwap`.
