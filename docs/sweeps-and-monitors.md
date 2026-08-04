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
| `OnLinesideReports` | `engine/threshold_monitor_shadow.go` | Edge report | ~60s | decides in `edge_reports` mode, audits only in `ledger` mode |
| `NoteSwapRequestContradiction` | `engine/threshold_monitor.go` | complex order received | per order | contradiction re-check |
| `OnThresholdChanges` | `engine/threshold_monitor.go` | loader config edit | per registry change | clears debounce so a new threshold takes effect at once |
| `Resync` | `engine/threshold_monitor.go` | station resync | per resync | re-engages payloads, clears debounce, fires already-below |
| `engagePayloads` | `engine/threshold_monitor.go` | config edit / Resync | per edit | the only path that deliberately fires on a read failure |
| `rehydrateThresholdEpisodes` | `engine/threshold_episodes.go` | inside `startupSweep` | boot | rebuilds open-episode maps — without it every restart doubles open demand |
| `reconcileThresholdBindings` | `engine/threshold_episodes.go` | demand reconciler | reconcile interval | closes episodes whose binding vanished |

**The shadow file is no longer a shadow.** In `edge_reports` mode it decides.

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

## Edge

| Mechanism | Where | Started by | Cadence |
|---|---|---|---|
| `HandleLoopBelowThreshold` | `engine/operator_demand_loader.go` | Core signal | per signal |
| `parkThresholdSignalIfCold` / `warmLoaderCacheAndReplay` | `engine/operator_demand_loader.go` | signal / node sync | while cache cold, then once |
| `SweepPushLoaders` / `MaybePushLoader` | `engine/operator_demand_loader.go` | register ack / window free | one-shot / per event |
| `SweepPushUnloaders` / `MaybePushUnloader` | `engine/operator_demand_unloader.go` | register ack / window free | one-shot / per event |
| `MaybeCreateUnloaderFullIn` | `engine/operator_demand_unloader.go` | consume signal, release | per event |
| `recordL1Burst` | `engine/loader_burst.go` | every in-bin order | 60s window, >8 warns |
| stranded-carrier monitor | `engine/uop_stranded_monitor.go` | Start | 60s |
| demand reconciler | `engine/demand_reconciler.go` | Start | 60s |
| lineside reporter | `engine/lineside_reporter.go` | Start | 60s |
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
