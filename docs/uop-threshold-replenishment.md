# UOP-Threshold Replenishment

This document covers the opt-in continuous-review reorder-point system for loader L1 and cell autoreorder, layered on top of the legacy bin-count + manual-REQUEST workflow described in [bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md).

The model is *C-push*: Core observes combined in-loop UOP (bins + lineside buckets), compares against engineer-configured thresholds, and signals Edge when replenishment is needed. Edge fires L1 in response. Both thresholds default to `0`, which means "do nothing" — plants that don't configure are bit-for-bit identical to the legacy path.

See [material-flow.md](material-flow.md) for `Bin`, `Payload`, `UOP`, and bucket terminology. See [bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md) for the loader/unloader workflow this layers onto.

---

## Two thresholds

The system manages two separate threshold knobs, in series along the supply path:

### Loader L1 threshold (loop UOP)

When **total in-loop UOP for a payload** drops below this value, Core signals Edge and Edge fires an L1 retrieve_empty order.

- *In-loop UOP* = `SUM(bin.uop_remaining)` + `SUM(bucket.qty)` for that payload, across every bin in the kanban lifecycle (`available`, `staged`, in-transit) and every lineside bucket carrying captured parts of that payload. Excludes `flagged`, `maintenance`, `quality_hold`, `retired` bins.
- *Lives at*: `loader_payload_thresholds` table on Edge, keyed by `(core_node_name, payload_code)`. `core_node_name` is the canonical cross-system identifier — multi-cell plants sharing a Core loader share one threshold row.
- *Synced to*: Core's `demand_registry.replenish_uop_threshold` column, via ClaimSync's `PayloadThresholds` map. Zero values are omitted from the wire (opt-in default).
- *Default*: `0` — Core doesn't monitor this loader/payload pair. Edge's legacy bin-count fallback (`refillLoaderForPayload`) owns the L1 decision.

### Cell autoreorder (line-bin UOP)

When **this specific line bin's `uop_remaining`** drops below this value, Edge fires a retrieve_full order against the cell's owning loader.

- *Lives at*: `style_node_claims.reorder_point` column on Edge (pre-existing; the v6 work only formalized opt-in semantics).
- *Evaluated at*: Edge, on every PLC counter delta in `wiring_counter_delta.go`.
- *Default*: `0` — silent-inert. The firing condition is `claim.AutoReorder && newRemaining <= reorder_point && newRemaining > 0 && reorder_point > 0`, which is unsatisfiable when `reorder_point = 0`.

The two thresholds work together: cell autoreorder pulls a fresh bin from the loader's lane; the loader L1 threshold replenishes the loader's lane.

---

## C-push signal pipeline

```
Edge                                      Core
─────────────────────────────────────────────────────────────────────
ClaimSync (extended)                  →   demand_registry +
  PayloadThresholds map                   replenish_uop_threshold
                                          per (station, node, payload)

LinesideBucketDelta (extended)        →   lineside_buckets
  PayloadCode populated by                  UPSERT applies qty delta
  capture.go at emit time                   and latches payload_code
                                            (empty incoming keeps
                                            previously-latched value)

BinUpdatedEvent OR
LinesideBucketApplied                 →   threshold_monitor subscribes
                                            to both event types.
                                            evaluatePayload(code):
                                              entries = lookup bindings
                                              uop    = SystemUOPForPayload
                                              if total < threshold:
                                                allow(debounce 15s):
                                                  fire signal

LoopBelowThresholdSignal              ←   SubjectLoopBelowThreshold,
  HandleLoopBelowThreshold:                 carries:
    loader = FindLoaderForPayload             - core_node_name
    refillLoaderForPayload(loader, p)         - payload_code
      countLoaderInFlightEmptyIn               - current_uop / threshold
      → fires L1 if 0                          - reason
```

The signal subject is `demand.loop_below_threshold`. Edge's `EdgeHandler.HandleData` decodes and routes to `HandleLoopBelowThreshold`.

### Debounce policy

15 seconds per `(station, core_node_name, payload)` tuple. The state is in-memory on Core — lost on restart. That's intentional: the startup sweep handles the restart case by re-evaluating every monitored binding with debounce bypassed.

`OnRegistryChanges` resets the debounce timer (and warm-up counter) for any binding whose threshold value changed during a SyncRegistry round, so an engineer-applied threshold engages on the next inventory event rather than waiting out a debounce window from a previous firing.

### Startup sweep

On `Run()`, the monitor waits a brief grace period (3s — gives a reconnecting Edge time to drain `uop_backfill` deltas through the inventory_delta_dedup pipeline), then walks every binding with `threshold > 0`, computing `SystemUOPForPayload` once per distinct payload and signaling any binding currently under threshold with `reason="warm_up_startup_sweep"`. The first signal per binding bypasses debounce; subsequent firings during the warm-up window respect a per-binding counter (currently floor `2`, "at least 2 signals on cold start").

The strict deploy ordering is: `uop_backfill` from a reconnecting Edge must complete before the startup sweep reads `SystemUOPForPayload`. The 3s grace period is a safety belt; production deploys document the explicit ordering as a checklist item.

---

## Dedup contract

**Two signal paths can fire L1**, and the dedup contract between them is load-bearing. Future code changes must preserve it.

1. **Legacy**: `DemandSignal` fires on every bin move. Edge's `HandleDemandSignal` → `MaybeCreateLoaderEmptyIn`. For opted-in pairs (threshold > 0), this path is **explicitly skipped** via the `hasOptInLoaderThreshold` guard. For non-opted-in pairs it's the legacy bin-count fallback (`refillLoaderForPayload` with `ReorderPoint` floor of 2).
2. **C-push**: `LoopBelowThresholdSignal` fires when Core detects threshold crossing. Edge's `HandleLoopBelowThreshold` → `refillLoaderForPayload`.

Both paths converge through `refillLoaderForPayload`, which calls `countLoaderInFlightEmptyIn(loader.node.ID, payload)` before firing L1. If both signals race (e.g. a bin event triggers Core's monitor at the same moment as Edge's `HandleDemandSignal` for a non-opted-in payload), the in-flight guard catches the duplicate — second caller sees `inflight ≥ 1` and returns.

**Do not remove or weaken this guard during refactors.** It is the single dedup point between the two paths.

---

## The calculator

Engineer-triggered. There is no nightly batch recompute — engineers see the calculation context every time they apply a value, and threshold thrash from auto-apply is impossible.

Each `(loader, payload)` row on the replenishment admin page has a **Calculate…** button that opens a modal:

```
┌──────────────────────────────────────────────────┐
│  Calculate threshold for Part A                  │
│  Loader: SPRING-LOADER-01                        │
│                                                  │
│  Date range: [Last 14 days ▾]  [Run calculation] │
│                                                  │
│  Inputs (editable, pre-filled with observed):    │
│    Cycle time:        [22.5]  engineer-supplied  │
│    L1 queue:          [  0.8] 14d, n=47          │
│    L1 transit:        [  4.2] 14d, n=47          │
│    L2 fill time:      [ 16.8] 14d, n=47          │
│    L2 transit:        [  6.1] 14d, n=45          │
│    Market→cell:       [ 22.0] 14d, n=61          │
│    Safety factor:     [  1.5] engineer-supplied  │
│                                                  │
│  Calculated:                                     │
│    Threshold:    118 UOP                         │
│    Cell reorder:  13 UOP                         │
│    Confidence:    HIGH                           │
│                                                  │
│  Current value: 120 UOP (manual)                 │
│                                                  │
│  [Apply] [Override…] [Cancel]                    │
└──────────────────────────────────────────────────┘
```

Every input is editable. Engineer edits flow through to the live-recomputed threshold + cell reorder shown below (the formula is mirrored in JS — no server round-trip per keystroke). Inputs the engineer changed away from the observed value are tracked on the threshold row in the `overridden_inputs` column and surface on the main table as `Overrides: <human-readable list>` under the source badge.

### Inputs

All lead-time inputs are derived from `order_history` state transitions on Edge via `shingo-edge/store/orders/lead_time_queries.go`. Cycle time is engineer-supplied for now (automatic peak-cycle derivation from `hourly_counts` is a later round).

| Input | Source | Helper |
|---|---|---|
| `cycle_seconds` | Engineer entry | — |
| `l1_queue_seconds` | `queued → acknowledged` mean | `AvgL1QueueSeconds` |
| `l1_transit_seconds` | `in_transit → delivered` mean (`retrieve_empty`) | `AvgL1TransitSeconds` |
| `l2_load_seconds` | `delivered → confirmed` **median** (`retrieve_empty`) | `MedianL2LoadSeconds` |
| `l2_transit_seconds` | `in_transit → delivered` mean (`store`) | `AvgL2TransitSeconds` |
| `market_to_cell_seconds` | `in_transit → delivered` **p95** (`retrieve`) | `P95MarketToCellSeconds` |

L2 load uses the median, not the mean, because the operator-fill segment is the only operator-driven step in the calculator and is exposed to long-tail outliers — end-of-shift confirms, weekend confirms, walked-away-from-station, Core's `ReconciliationService.AutoConfirmStuckDeliveredOrders` flipping stuck-delivered orders after a timeout. Median lets every outlier class fall out without filtering on a magic detail string, and is robust to outlier classes we haven't enumerated yet.

`P95MarketToCellSeconds` returns the 95th-percentile retrieve duration, not the mean — reshuffle outliers (one-off long retrieves from blocked lanes) would otherwise pull the mean upward and oversize the cell reorder.

### Formula

```
l1_threshold = ceil(((l1_queue + l1_transit + l2_load + l2_transit)
                     / cycle_seconds) * safety_factor)

cell_reorder = ceil((market_to_cell / cycle_seconds) * safety_factor)
```

No floor and no ceiling — the calculator returns the formula result verbatim. Safety/advisory concerns layered on top of the math (minimum-stock floors, over-capacity callouts) belong in the UI, not in the calculation. The Calculate modal and the loader-threshold row both render an informational **`≈ N bins`** annotation next to the threshold using the loader claim's bin capacity (`N = ceil(threshold / C)`); the annotation is suppressed when bin capacity is unresolvable. **Override** is the escape hatch for any value the engineer judges un-supportable.

### Confidence

A coverage score over the data the calculator was able to observe in the date range:

| Label | Condition |
|---|---|
| `HIGH` | window ≥ 14 days AND ≥ 20 completed L1 cycles AND ≥ 20 completed retrieves |
| `MEDIUM` | window ≥ 7 days AND ≥ 10 of each |
| `LOW` | anything below MEDIUM |

`LOW` confidence **disables the Apply button**. The engineer can still click Override and type a value; the threshold row records `source='manual'` so the provenance is captured. The HIGH/MEDIUM thresholds are conservative for the initial Springfield roll-out and will be re-tuned post-calibration.

### What gets persisted

There is no per-calculate audit table. The engineer can see the current threshold's `source` / `updated_at` / `updated_by` on the threshold row for "what's the current value based on", and re-runs Calculate any time they want a fresh look at the inputs.

`Apply` and `Override` stamp the threshold row directly:

- `replenish_uop_threshold` — the value Core compares against.
- `source` — `'calculated'` (Apply) or `'manual'` (Override).
- `threshold_calculated` — the calculator's suggested output, even on Override. Lets the threshold row answer "calculator said X, engineer chose Y" without joining anything.
- `threshold_calculated_at` — the `computed_at` echoed back from the Calculate response.
- `threshold_confidence` — `HIGH` / `MEDIUM` / `LOW` from the Calculate response.
- `overridden_inputs` — comma-separated list of input field names the engineer overrode in the modal; surfaces on the main table as `Overrides: <human list>` under the source badge.

### Recalculate all

The **Recalculate all** button at the process level enumerates every `(loader, payload)` binding on active processes and runs `Calculate` for each, returning a summary table — one row per binding showing the calculator output and confidence. The engineer reviews the summary, closes the modal, and clicks **Apply** on individual rows in the main threshold table for the ones that look right. Bulk-apply on the summary is intentionally absent; the brief calls for engineer review per row.

---

## Opt-out / opt-in semantics

| `replenish_uop_threshold` | Behavior |
|---|---|
| `0` or no row | Core never monitors. Edge's `MaybeCreateLoaderEmptyIn` runs the legacy bin-count fallback. Cell autoreorder is silent-inert. Identical to pre-v6 behavior. |
| `> 0` | Core monitors. Edge's `hasOptInLoaderThreshold` guard skips the legacy bin-count for that pair. C-push owns L1 firing. |

A row with `threshold = 0` and `source = 'manual'` is semantically equivalent to no row at all from the runtime's perspective — it exists so the UI can show "engineer considered this and opted out" in the source audit. `DeleteLoaderThreshold` and "save threshold = 0" are both supported entry points to the opted-out state.

### Source field

`loader_payload_thresholds.source` tracks provenance:

| Source | Meaning |
|---|---|
| `legacy` | Default for rows that exist but have never been touched. Loader falls back to bin-count; cell autoreorder silent-inert. |
| `manual` | Engineer typed a value directly OR clicked Override on a Calculate run. |
| `calculated` | Engineer applied a Calculate run's output unchanged. |

The replenishment admin UI shows a source badge per row.

---

## Operational notes

### Warm-up cap

On startup sweep, bindings below threshold get a per-binding warm-up counter seeded to `2`. The first signal fires immediately (bypassing debounce); subsequent inventory events during the warm-up window also fire (bypassing debounce, decrementing the counter) so the first L1 round drives both a bin to the supermarket and a second bin in flight. After the counter hits zero, normal debounced operation takes over.

The formula in the design brief is `max(2, ceil(threshold / C))` — the per-binding cap, not global. The implementation currently applies the `2` floor only; lifting `C` from claim config to apply the full formula is a later refinement.

### Debounce reset on threshold change

`OnRegistryChanges` is called from `CoreDataService.handleClaimSync` after `SyncDemandRegistry` returns its change list. For every binding whose threshold value moved, the monitor `delete`s the debounce + warm-up state — so a freshly-applied threshold (engineer just clicked Apply) takes effect on the next inventory event rather than being suppressed by a residual debounce window from a previous firing under the old value.

### Cell autoreorder evaluation

`wiring_counter_delta.go` evaluates autoreorder on every PLC counter tick, with three v6 additions:

1. Explicit `claim.ReorderPoint > 0` gate makes opt-in semantic explicit (the old condition `newRemaining <= ReorderPoint` was unsatisfiable when `ReorderPoint = 0`, but the path still ran every tick).
2. Diagnostic log line on every evaluation with the gate outcome, so engineers can see *why* nothing fires.
3. Symmetric log on the produce-side tick path and a debug-level log when a tick is held during the no-bin gap (see below).

### Unbound-slot gap

There's a gap between physical pickup of the old bin at the cell and delivery of the new bin to the slot during which no bin is bound (`active_bin_id` is nil) and `remaining_uop_cached` doesn't update — the new bin's UOP isn't credited until its `OrderDelivered` envelope binds it. Under the single-pointer hold-and-replay model, the bin portion of each tick during this gap accumulates in `pending_uop_delta` and replays onto the next bin when it binds; the cache value isn't touched while unbound, so autoreorder evaluation naturally doesn't fire against a stale count during the gap — firing then would over-order, since the in-flight bin lands shortly. The v6 addition is a debug log on the held-tick path.

### Backfill: there isn't one

There is no payload-code backfill for pre-existing `lineside_buckets` rows. Springfield is a fresh install; all future plants get correct `payload_code` from day 1 because `capture.go` writes it from the order context at emit time. If a plant ever upgrades from a pre-feature version with existing buckets, the right design is `bin_uop_audit` correlation (the audit table records every `capture_reduction` operation with the bin's `order_id` and `payload_code`, so joining gives correct payload attribution). That work is deferred until a real plant needs it. Pre-existing empty `payload_code` rows are excluded from `SystemUOPForPayload` — conservative undercount, never overcount.

---

## File map

### Edge

- `engine/operator_demand.go` — `MaybeCreateLoaderEmptyIn`, `HandleLoopBelowThreshold`, `refillLoaderForPayload`, `countLoaderInFlightEmptyIn`, `hasOptInLoaderThreshold`.
- `engine/wiring_counter_delta.go` — cell autoreorder evaluation.
- `engine/replenishment_admin.go` — admin-page engine wrappers (`UpsertLoaderThreshold`, `CalculateThresholdForLoader`, `ApplyCalculatedThreshold`, `OverrideCalculatedThreshold`, `ListLoaderClaimsForRecalculate`).
- `service/threshold_calculator.go` — pure formula (`CalculateThresholds`) + date-range driver (`ThresholdCalculatorService.Calculate`).
- `store/loader_payload_thresholds.go` — CRUD for the threshold table.
- `store/orders/lead_time_queries.go` — `order_history`-derived lead-time helpers.
- `www/handlers_api_replenishment.go` + `www/static/js/pages/replenishment.js` + `www/templates/replenishment.html` — admin UI.

### Core

- `engine/threshold_monitor.go` — `ThresholdMonitor` (debounce, warm-up, startup sweep, signal dispatch).
- `service/inventory_system_count.go` — `SystemUOPForPayload`.
- `store/demands/` — `demand_registry` CRUD including `replenish_uop_threshold`.

### Protocol

- `protocol/payloads.go` — `ClaimSyncEntry.PayloadThresholds`, `LinesideBucketDelta.PayloadCode`, `LoopBelowThresholdSignal`.

---

## Out of scope

Tracked separately; not part of the v6 work:

- **Statistical formulas** (mixed variability, z-scores) — Phase 3.
- **EPEI / Run Frequency** for shared cells — Phase 3.
- **Signal kanban** — Phase 3.
- **Capacity feasibility check** with OEE — Phase 3.
- **Poisson formula** for low-volume styles — Phase 3.
- **FG-out kanban** — Phase 3.
- **U1/U2 unloader-side** thresholds — Springfield doesn't run unloaders; defer.
- **R3 iterate-all-claims** — Springfield is Case A (loader claim lists all payloads).
- **Queued-retrieve safety net at Edge** — redundant with C-push.
- **Diagnostics UI live stream** — structured logs are emitted; UI rendering deferred to Phase 1.5.
- **Automatic peak-cycle derivation from `hourly_counts`** — engineer-supplied cycle time for now.
