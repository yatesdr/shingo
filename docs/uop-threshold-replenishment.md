# UOP-Threshold Replenishment

This document covers the continuous-review reorder-point system for loader L1 and cell autoreorder.

The model is *C-push*: Core observes combined in-loop UOP (bins + lineside buckets), compares against engineer-configured thresholds, and creates the L1 retrieve orders itself when replenishment is needed. A threshold of `0` means Core does not monitor that pair; what feeds the loader then depends on its replenishment mode (see the loader L1 threshold's default below).

**There is one automatic path.** This document used to describe two, with a dedup contract between them, and that is the single most important thing to know is no longer true. The legacy bin-count `DemandSignal` route is retired entirely (2026-08): Core no longer emits it and no handler exists on Edge. There is no bin-count fallback, no `ReorderPoint` floor of 2 for loaders, and nothing that needs to "skip opted-in pairs".

See [material-flow.md](material-flow.md) for `Bin`, `Payload`, `UOP`, and bucket terminology; [bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md) for the loader/unloader workflow this sits in; and [sweeps-and-monitors.md](sweeps-and-monitors.md) for the full list of what re-evaluates a threshold and when.

---

## Two thresholds

The system manages two separate threshold knobs, in series along the supply path:

### Loader L1 threshold (loop UOP)

When **total in-loop UOP for a payload** drops below this value, Core creates an L1 retrieve_empty order (directly — no wire signal; Edge only executes).

- *In-loop UOP* = `SUM(bin.uop_remaining)` + `SUM(bucket.qty)` for that payload, across every bin in the kanban lifecycle (`available`, `staged`, in-transit) and every lineside bucket carrying captured parts of that payload that the node's ACTIVE style still claims. Excludes `flagged`, `maintenance`, `quality_hold`, `retired` bins. Stranded buckets (captured under a prior style the node no longer consumes) are EXCLUDED — real inventory, but not available to the running style; counting them suppressed replenishment at Springfield (74576). A node with no known active style has its buckets counted: only positively-proven-stranded inventory is left out.
- *Lives at*: Core-owned loader config — `bin_loader_homes.uop_threshold` per (loader position, payload), derived into `demand_registry.replenish_uop_threshold` by `BuildDemandRegistryFromAggregate` and re-derived on every loader config edit (`service/loader_service.go` `rederive()`). The Edge `loader_payload_thresholds` table was dropped 2026-07-21; the Edge page's loader-threshold section was deleted with it.
- *Owned by*: Core. The loader aggregate (`bin_loaders` and its payload rows) is the source of truth, and `demand_registry.replenish_uop_threshold` is derived from it. Edge receives loader configuration on the node-list sync, not by pushing it up.
- *Default*: `0` — Core doesn't monitor this loader/payload pair. What feeds it then depends on the loader's replenishment mode: an operator-driven loader is stocked by the window-free push (`MaybePushLoader` / `SweepPushLoaders`); a threshold-mode loader with threshold 0 is fed by NOTHING, and the startup push logs a warning saying exactly that.

### Cell autoreorder (line-bin UOP)

When **this specific line bin's `uop_remaining`** drops below this value, Edge fires a retrieve_full order against the cell's owning loader.

- *Lives at*: `style_node_claims.reorder_point` column on Edge (pre-existing; the v6 work only formalized opt-in semantics).
- *Evaluated at*: Edge, on the PLC counter delta path in `wiring_counter_delta.go` — which is essentially the only automatic evaluation site (see "Cell autoreorder evaluation" under Operational notes for what that does and does not mean).
- *Default*: `0` — silent-inert via the explicit `ReorderPoint > 0` gate. The firing condition is `AutoReorder && ReorderPoint > 0 && newRemaining <= ReorderPoint`, including `newRemaining <= 0` — a level, not a (0, threshold] window.

The two thresholds work together: cell autoreorder pulls a fresh bin from the loader's lane; the loader L1 threshold replenishes the loader's lane.

---

## C-push pipeline (no signal — Core orders directly)

The wire signal this section used to describe (`LoopBelowThresholdSignal` on
`demand.loop_below_threshold`, Edge sizing and firing the ask) was deleted
2026-08-02 after a Springfield over-order showed the two halves counted
different things. Core now owns the whole decision:

```
Edge                                      Core
─────────────────────────────────────────────────────────────────────
                                      ←   loader aggregate (bin_loaders)
  node-list sync carries the loader's       is the source of truth for
  windows, payloads and thresholds          thresholds; demand_registry
                                            is derived from it

LinesideBucketDelta                   →   lineside_buckets
  PayloadCode populated by                  UPSERT applies qty delta
  capture.go at emit time                   and latches payload_code
                                            (empty incoming keeps
                                            previously-latched value)

BinUpdatedEvent, BinUOPDelta, or      →   threshold_monitor subscribes
LinesideBucketApplied                       to all three.
                                            evaluatePayload(code):
                                              entries = lookup bindings
                                              uop    = SystemUOPForPayload
                                              if total < threshold:
                                                allow(debounce 15s):
                                                  fireSignalCached:
                                                    size ask, resolve
                                                    free windows,
                                                    create one order
                                                    per free window
```

The orders are created on Core; the Edge only executes them. See
`fireSignalCached` in `engine/threshold_monitor.go` — the cutover comment
there records the 2026-07-31 incident that ended the split.

Three separate Core subscriptions funnel into one evaluation, so a single bin move commonly trips more than one; the 15-second debounce is what absorbs that. The monitor also mints and closes **demand episodes** per binding (`engine/threshold_episodes.go`), and fired orders are stamped with the episode's origin — the episode is what dedups Core-side (see the reservation seam above). See [sweeps-and-monitors.md](sweeps-and-monitors.md).

### Debounce policy

15 seconds per `(station, core_node_name, payload)` tuple. The state is in-memory on Core — lost on restart. That's intentional: the startup sweep handles the restart case by re-evaluating every monitored binding with debounce bypassed.

`OnThresholdChanges` resets the debounce timer (and warm-up counter) for any binding whose threshold value changed during a registry sync, so an engineer-applied threshold engages on the next inventory event rather than waiting out a debounce window from a previous firing. It also closes the binding's open demand episode and evaluates immediately (`engagePayloads`), so a config edit can itself fire.

### Startup sweep

On `Run()`, the monitor waits a brief grace period (3s — gives a reconnecting Edge time to drain `uop_backfill` deltas through the inventory_delta_dedup pipeline), then walks every binding with `threshold > 0`, computing `SystemUOPForPayload` once per distinct payload and ordering for any binding currently under threshold with `reason="warm_up_startup_sweep"`. The first order batch per binding bypasses debounce; subsequent firings during the warm-up window respect a per-binding counter (currently floor `2`, "at least 2 fires on cold start").

The strict deploy ordering is: `uop_backfill` from a reconnecting Edge must complete before the startup sweep reads `SystemUOPForPayload`. The 3s grace period is a safety belt; production deploys document the explicit ordering as a checklist item.

---

## The reservation seam

One automatic path fires an L1 — the threshold monitor on Core — but it is not
the only thing that puts a bin on a loader window. The operator's Request Empty
and Request Full buttons, the push sweeps, the unloader's U1, and the HTTP
order API all target the same windows, and every one of the Edge-side ones goes
through `withLoaderBudget` (`shingo-edge/engine/operator_demand_loader.go`).

The seam takes a per-loader mutex and, in one snapshot, counts in-flight
`retrieve_empty` orders across the loader's delivery-node set — applying both the
per-payload dedup (fire only `want − in-flight-for-payload`) and the loader-total
capacity cap (in-flight across the cluster ≤ budget, where budget = the
delivery-set cardinality, one bin per window or position). A multi-window loader's
budget is the sum of its windows, not a hardcoded `1`. Because the mutex is held
across count and create, two racing callers cannot both read `inflight = 0` and
both fire — the second waits, recounts, and sees the first.

It also refuses to fire when it cannot verify occupancy. An unreadable occupancy
check is not an empty window, and treating it as one is what produced the
2026-07-31 Springfield over-ordering incident.

**Do not remove or weaken the seam during refactors, and do not wrap it in a DB
transaction** — its correctness is the mutex plus count monotonicity, not
isolation. The seam is the count→fire atomicity point for the OPERATOR-side
writers only (push sweeps, Request Empty/Full, the unloader's U1, the HTTP
order API) — and even there the 2026-07-31 census found creators outside it
(RequestEmptyBin's simple mode; the changeover paths, unestablished). Core's
threshold replenishment does NOT pass through here: what dedups it is the
demand episode — dispatch.ReplenishLoader subtracts the episode's own
outstanding orders from the ask, and a per-window delivery budget bounds the
rest. Do not describe this seam as the single dedup point for "every path that
stocks a loader"; that claim was refuted by census and cost Springfield 241
duplicate orders while it stood.

---

## The calculator

The calculator lives on Core (`shingo-core/service/threshold_calculator.go`,
lead-time helpers in `shingo-core/store/orders/lead_time_queries.go`),
exposed at `POST /api/loader/calculate` and driven from the inventory page:
enter a cycle time, run the calculation, read the suggested UoP threshold and
confidence in the popover, then set the value on the loader config. The Edge
replenishment page retains only the cell-autoreorder half; its loader
section, calculator modal, Apply/Override/Recalculate-all controls and the
`threshold_calculated` / `overridden_inputs` / `threshold_confidence` columns
were deleted 2026-07-21 — the Edge write path terminated in a no-op stub.

Two input differences from earlier revisions of this page: L1 queue measures
`queued → dispatched` (Core never writes `acknowledged`, so the old pair was
structurally zero), and the L2 store-transit input is no longer auto-fetched
(no store orders exist) — it is an operator-editable modal input defaulting
to 0. Because L2 coverage is always zero, `scoreConfidence` can never return
HIGH or MEDIUM; every suggestion currently reports LOW, and there is no Apply
button to gate.

Engineer-triggered. There is no nightly batch recompute — engineers see the calculation context every time they apply a value, and threshold thrash from auto-apply is impossible.

The historical Edge modal this section used to document, for reference:

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

*(The rest of this subsection describes the deleted Edge modal. Kept for provenance — the Core popover reuses the same input set minus the changes noted above.)*

### Inputs

All lead-time inputs are derived from `order_history` state transitions on Core via `shingo-core/store/orders/lead_time_queries.go`. Cycle time is engineer-supplied for now (automatic peak-cycle derivation from `hourly_counts` is a later round).

| Input | Source | Helper |
|---|---|---|
| `cycle_seconds` | Engineer entry | — |
| `l1_queue_seconds` | `queued → dispatched` mean (Core never writes `acknowledged`, so the old `queued → acknowledged` pair was structurally zero) | `AvgL1QueueSeconds` |
| `l1_transit_seconds` | `in_transit → delivered` mean (`retrieve_empty`) | `AvgL1TransitSeconds` |
| `l2_load_seconds` | `delivered → confirmed` **median** (`retrieve_empty`) | `MedianL2LoadSeconds` |
| `l2_transit_seconds` | no longer auto-fetched (no store orders exist); operator-editable modal input, default 0 | — |
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

`LOW` confidence is the only label reachable today (L2 coverage is always zero — see above), so as a practical matter **every suggestion reports LOW**. The engineer can still Override and type a value; on Core the provenance is the loader config's audit trail. The HIGH/MEDIUM thresholds are conservative for the initial Springfield roll-out and will be re-tuned post-calibration.

### What gets persisted

The calculation itself is not persisted — the engineer reads the suggested value from the popover and sets the threshold on the loader config (`bin_loader_homes.uop_threshold` via the loader service), which is where the value lives and where its `updated` provenance is visible. The Edge-side persistence columns this section used to document (`threshold_calculated`, `threshold_calculated_at`, `threshold_confidence`, `overridden_inputs` on a threshold row) were deleted 2026-07-21 along with the Edge loader-threshold UI; none of them exist on Core.

### Recalculate all

The bulk **Recalculate all** control was part of the deleted Edge modal; there is no equivalent on Core today. Each calculation is run per (loader, payload) from the inventory page popover.

---

## Opt-out / opt-in semantics

| `replenish_uop_threshold` | Behavior |
|---|---|
| `0` or no row | Core never monitors. Nothing fires an L1 for that pair automatically; what feeds the loader depends on its replenishment mode (see the default above — operator-driven loaders get the push; threshold-mode loaders with 0 get nothing, loudly). |
| `> 0` | Core monitors and creates orders on crossing. C-push owns L1 firing for that pair. |

The `loader_payload_thresholds.source` audit table (and its `legacy`/`manual`/`calculated` semantics) is gone with the dropped Edge table. The surviving provenance analogue is `reorder_point_source` on the cell claim (`legacy`/`manual`/`calculated`, `shingo-edge/domain/process.go`).

---

## Operational notes

### Warm-up cap

On startup sweep, bindings below threshold get a per-binding warm-up counter seeded to `2`. The first fire happens immediately (bypassing debounce); subsequent inventory events during the warm-up window also fire (bypassing debounce, decrementing the counter) so the first L1 round drives both a bin to the supermarket and a second bin in flight. After the counter hits zero, normal debounced operation takes over.

The formula in the design brief is `max(2, ceil(threshold / C))` — the per-binding cap, not global. The implementation currently applies the `2` floor only; lifting `C` from claim config to apply the full formula is a later refinement.

### Debounce reset on threshold change

`OnThresholdChanges` is called from the loader config-edit path (`service/loader_service.go`, `rederive()` — after `SyncDemandRegistry` returns its change list). Edge (re)connect is a separate trigger and calls `Resync` instead: `HandleEdgeRegister` discards the sync's change list and re-engages the station's bindings wholesale. For every binding whose threshold value moved, the monitor `delete`s the debounce + warm-up state — so a freshly-applied threshold (engineer just clicked Apply) takes effect on the next inventory event rather than being suppressed by a residual debounce window from a previous firing under the old value.

### Cell autoreorder evaluation

`wiring_counter_delta.go` evaluates autoreorder on the consume-tick path — and
that is essentially the only automatic evaluation site. The produce-tick path
evaluates the mirror-image relief level, and `FlipABNode` re-checks the
depleted partner at the moment the pair flips. There is NO periodic
re-evaluation of a cell claim anywhere; the Edge reconciler only closes
stranded episodes, it never fires.

The firing condition is `claim.AutoReorder && claim.ReorderPoint > 0 &&
newRemaining <= claim.ReorderPoint`, evaluated only when a bin is bound, and
it includes `newRemaining <= 0`. A threshold is a level, not a window:
overshooting it (a batched consume flush that drains the bin to 0 or past it)
fires on the very tick that lands there. The `newRemaining > 0` floor an
earlier revision of this page documented is gone — it was the defect that
starved a cell that drained past zero.

Because the evaluation rides the tick, a cell that STOPS TICKING is never
re-evaluated. If `CanAcceptOrders` refuses at the crossing tick (changeover in
progress, an active/staged order in flight) or the order create errors, only
the falling edge is stamped (`style_node_claims.below_reorder_since`); no
demand episode opens and no order exists, and nothing retries until the next
tick, an A/B flip, or an operator request. A cell that then drains out and
stops cycling never asks again — the level trigger is only as live as the
tick stream that evaluates it. (Ticks are also skipped entirely on a PLC
counter `reset` anomaly.)

Three v6 additions, all still present:

1. Explicit `claim.ReorderPoint > 0` gate makes opt-in semantic explicit.
2. Diagnostic log line on every evaluation with the gate outcome, so engineers can see *why* nothing fires.
3. Symmetric log on the produce-side tick path and a debug-level log when a tick is held during the no-bin gap (see below).

### Unbound-slot gap

There's a gap between physical pickup of the old bin at the cell and delivery of the new bin to the slot during which no bin is bound (`active_bin_id` is nil) and `remaining_uop_cached` doesn't update — the new bin's UOP isn't credited until its `OrderDelivered` envelope binds it. Under the single-pointer hold-and-replay model, the bin portion of each tick during this gap accumulates in `pending_uop_delta` and replays onto the next bin when it binds; the cache value isn't touched while unbound, so autoreorder evaluation naturally doesn't fire against a stale count during the gap — firing then would over-order, since the in-flight bin lands shortly. The v6 addition is a debug log on the held-tick path.

### Backfill: there isn't one

There is no payload-code backfill for pre-existing `lineside_buckets` rows. Springfield is a fresh install; all future plants get correct `payload_code` from day 1 because `capture.go` writes it from the order context at emit time. If a plant ever upgrades from a pre-feature version with existing buckets, the right design is `bin_uop_ledger` correlation (the audit table records every `capture_reduction` operation with the bin's `order_id` and `payload_code`, so joining gives correct payload attribution). That work is deferred until a real plant needs it. Pre-existing empty `payload_code` rows are excluded from `SystemUOPForPayload` — conservative undercount, never overcount.

---

## File map

### Edge

- `engine/operator_demand_loader.go` — `withLoaderBudget` (count→fire atomicity
  for the OPERATOR-side loader/unloader writers only), the push sweeps.
- `engine/wiring_counter_delta.go` — cell autoreorder evaluation.
- `engine/replenishment_admin.go` — the one surviving wrapper,
  `UpdateCellReorder` (reorder_point / source / auto_reorder on
  style_node_claims).
- `www/handlers_api_replenishment.go` + `www/static/js/pages/replenishment.js`
  + `www/templates/replenishment.html` — cell-autoreorder admin UI only.

### Core

- `engine/threshold_monitor.go` — `ThresholdMonitor` (debounce, warm-up,
  startup sweep, demand episodes, order creation via `dispatch.ReplenishLoader`).
- `service/threshold_calculator.go` + `store/orders/lead_time_queries.go` —
  the calculator and its Postgres lead-time helpers.
- `service/inventory_system_count.go` — `SystemUOPForPayload`.
- `store/demands/` — `demand_registry` CRUD including `replenish_uop_threshold`.
- `store/loaders/` + `service/loader_service.go` — `bin_loader_homes.uop_threshold`
  (where the threshold actually lives) and the `rederive()` registry sync.
- `www/handlers_loader.go` — `POST /api/loader/calculate` (the calculator endpoint).

### Protocol

- `protocol/payloads.go` — `LinesideBucketDelta.PayloadCode`. (Threshold values now ride `LoaderInfo` on the node-list sync, not a ClaimSync payload. `LoopBelowThresholdSignal` was deleted — Core orders directly.)

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
