# UOP-Threshold Replenishment

Verified against: 51489c78

This document covers the continuous-review reorder levels: Core's loader L1 threshold and the Edge's cell
autoreorder.

The loader side is *C-push*: Core observes combined in-loop UOP (bins + lineside piles), compares it against
engineer-configured thresholds, and creates the L1 retrieve orders itself. A threshold of `0` means Core does not
monitor that pair; what feeds the loader then depends on its replenishment mode (see the default below). This is
the only automatic path for a loader's empties: there is no bin-count trigger and no Edge-side sizing.

See [terminology.md](terminology.md) for `Bin`, `Payload`, `UOP` and pile terminology;
[uop-architecture.md](uop-architecture.md) for how the counts themselves are kept;
[bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md) for the loader/unloader workflow this
sits in; and [sweeps-and-monitors.md](sweeps-and-monitors.md) for the full list of what re-evaluates a threshold and
when.

---

## Two thresholds

The system manages two separate threshold knobs, in series along the supply path:

### Loader L1 threshold (loop UOP)

When **total in-loop UOP for a payload** drops below this value, Core creates an L1 retrieve_empty order (directly —
no wire signal; Edge only executes).

- *In-loop UOP* is `SystemUOPForPayload`: `SUM(bins.uop_remaining)` over every bin of the payload, anywhere,
  except `flagged`, `maintenance` and `retired` bins and a carrier stamped undeclared (nothing can fetch it), plus
  `SUM(lineside_buckets.qty)` over the payload's **active** piles. A stranded pile never counts; when a pile is
  active and when it strands is [uop-architecture.md](uop-architecture.md)'s.
- *Lives at*: Core-owned loader config — `bin_loader_homes.uop_threshold` for a dedicated position, or
  `bin_loader_payloads.uop_threshold` for a shared loader's payload — derived into
  `demand_registry.replenish_uop_threshold` by `BuildDemandRegistryFromAggregate` and re-derived on every loader
  config edit (`rederive` in `service/loader_service.go`).
- *Owned by*: Core. Edge receives loader configuration on the node-list sync, not by pushing it up.
- *Default*: `0` — Core doesn't monitor this loader/payload pair. What feeds it then depends on the loader's
  replenishment mode: an operator-driven loader is stocked by the window-free push (`rePushOwnLoader` /
  `SweepPushLoaders`); a threshold-mode loader with threshold 0 is fed by NOTHING, and Core's loader page says so
  ("no threshold set — nothing will order for this loader").

### Cell autoreorder (the claim's level)

When **the line bin's UOP** reaches the claim's level, the Edge asks for material for that node.

- *Lives at*: the Edge claim, `style_node_claims.reorder_point`, with `auto_reorder` deciding whether a breach
  orders. The level is `NodeClaim.DemandLevel` (`protocol.ClaimDemandLevel`): a consume claim's reorder point; a
  produce claim's reorder point when it is above 0 and below the pack, otherwise its UOP capacity.
- *Evaluated at*: Edge, by the demand reconciler's level sweep (`sweepCellLevels` in `engine/demand_reconciler.go`)
  once a period (`demandReconcileInterval`, 60s). The PLC tick only moves the count.
- *Default*: `0` — a level of 0 is the opt-out on either role and evaluates nothing. Breach is `remaining <= level`
  on a consume claim and `remaining >= level` on a produce one — a level, not a window: a count that overshoots it
  is still breached.

The two thresholds work together: cell autoreorder pulls a fresh bin from the loader's lane; the loader L1
threshold replenishes the loader's lane.

---

## C-push pipeline (no signal — Core orders directly)

```
Edge                                      Core
─────────────────────────────────────────────────────────────────────
                                      ←   loader aggregate (bin_loaders)
  node-list sync carries the loader's       is the source of truth for
  windows, payloads and thresholds          thresholds; demand_registry
                                            is derived from it

LinesideBucketLevel                   →   lineside_buckets
  one per changed pile row per flush,       sets the (node, payload,
  and every row at Edge boot                state) row to the level;
                                            0 deletes it

bin updated, BinUOPDelta applied,     →   threshold monitor entry points:
pile level applied                          handleBinUpdated,
                                            OnBinUOPDelta, OnBucketApplied
                                            evaluatePayload(code):
                                              entries = lookup bindings
                                              uop    = SystemUOPForPayload
                                              if total < threshold:
                                                allow(debounce 15s):
                                                  fireSignalCached:
                                                    ReplenishLoader
                                                    sizes the ask, chooses
                                                    free windows, creates
                                                    one order per window
```

The orders are created on Core (`fireSignalCached` in `engine/threshold_monitor.go`); the Edge only executes them.

Three entry points funnel into one evaluation, so a single bin move commonly trips more than one; the 15-second
debounce is what absorbs that. The monitor also mints and closes **demand episodes** per binding
(`engine/threshold_episodes.go`), and fired orders are stamped with the episode's origin — the episode is what
dedups Core-side (see the loader decision below). See [sweeps-and-monitors.md](sweeps-and-monitors.md).

### Debounce policy

15 seconds (`thresholdDebounceWindow`) per place — `(core_node_name, payload)`, the key `placeKey` gives
everything the monitor keys, so two registry rows for one place share one debounce budget. The state is in-memory
on Core — lost on restart. That's intentional: the startup sweep handles the restart case by re-evaluating every
monitored binding with debounce bypassed.

`OnThresholdChanges` resets the debounce timer (and warm-up counter) for any binding whose threshold value changed
during a registry sync, so an engineer-applied threshold engages on the next inventory event rather than waiting out
a debounce window from a previous firing. It also closes the binding's open demand episode and evaluates immediately
(`evaluatePayload`), so a config edit can itself fire. A failed read there evaluates nothing.

### Startup sweep

On `Run()`, the monitor waits a brief grace period (3s — gives a reconnecting Edge time to drain `uop_backfill`
deltas through the inventory_delta_dedup pipeline), then walks every binding with `threshold > 0`, computing
`SystemUOPForPayload` once per distinct payload and ordering for any binding currently under threshold with
`reason="warm_up_startup_sweep"`. The first order batch per binding bypasses debounce; subsequent firings during the
warm-up window respect a per-binding counter (see [Warm-up cap](#warm-up-cap)).

The ordering rule: `uop_backfill` from a reconnecting Edge must complete before the startup sweep reads
`SystemUOPForPayload`. The 3s grace period is a safety belt, not the guarantee.

---

## The loader decision

Every carrier a loader or unloader takes — the threshold monitor's L1s, the operator's Request Empty and Request
Full, the push sweeps, the unloader's U1, the manual retrieve at a loader window — is decided on Core by one door,
`dispatch.ReplenishLoader`, under the loader's lock, failing closed on any read it cannot make. The threshold origin
is Core's own and never comes over the wire. How it sizes, subtracts and chooses windows, and the one caveat (an
order a claim sends straight to a window is still the Edge's), are
[bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md)'s. **Do not move any part of this
decision back to the Edge, and do not split the read from the insert.**

---

## The calculator

The calculator lives on Core (`shingo-core/service/threshold_calculator.go`, lead-time helpers in
`shingo-core/store/orders/lead_time_queries.go`), exposed at `POST /api/loader/calculate`. It is **uncalibrated**:
on `/inventory` it is reached only from the UoP threshold column's ⓘ ("calculator (uncalibrated)"), and no row shows
its number. It opens on the position with the shortest margin, with a picker for the others. Its suggestion is
computed for every position from one read (`LeadTimesByPayload`: the same four inputs and two coverage counts,
grouped by payload, pinned against the per-payload helpers) and `CalculateThresholds`, with the cycle taken from the
14-day median running rate (the median of the payload's positive `tte_samples` rates, kind threshold). There is no
suggestion until the position has a completed L1 cycle in the window — a threshold `retrieve_empty` delivered to the
position (any window of a shared loader) and confirmed — counted per position in the bindings read; a lead time from
an order that never completed is not a lead time. **A suggestion is never one click into the live threshold**: it is
prefilled in the calculator, the cycle and safety factor open to change and the calculation re-run through the
endpoint, and one Confirm saves it (source `calculated`, or `manual` if the value was typed over). The window ends at
Core's clock (`clock.Now()`), the one `order_history` is stamped with. The Edge replenishment page holds only the
cell-autoreorder half.

Engineer-triggered. There is no nightly batch recompute — engineers see the calculation context every time they apply
a value, and threshold thrash from auto-apply is impossible.

### Inputs

All lead-time inputs are derived from `order_history` state transitions on Core via
`shingo-core/store/orders/lead_time_queries.go`. Cycle time is supplied with the request; the `/inventory` page
supplies 1 / the median of the payload's positive plant-wide consumption rates (`tte_samples.rate_per_sec`, kind
threshold, in the window; not the latest: a sample's rate is a sliding window's count, and after a stop it decays to
one unit per window before it reads 0), and its modal lets the engineer type another.

| Input | Source | Helper |
|---|---|---|
| `cycle_seconds` | The request: on `/inventory`, 1 / the median measured rate; editable in its modal | `LeadTimesByPayload` (`RatePerSec`) |
| `l1_queue_seconds` | `queued → dispatched` mean (Core never writes `acknowledged` on these orders) | `AvgL1QueueSeconds` |
| `l1_transit_seconds` | `in_transit → delivered` mean (`retrieve_empty`) | `AvgL1TransitSeconds` |
| `l2_load_seconds` | `delivered → confirmed` **median** (`retrieve_empty`) | `MedianL2LoadSeconds` |
| `l2_transit_seconds` | not fetched (no store orders exist): 0, and the endpoint takes none | — |
| `market_to_cell_seconds` | `in_transit → delivered` **p95** (`retrieve`) | `P95MarketToCellSeconds` |

L2 load uses the median, not the mean, because the operator-fill segment is the only operator-driven step in the
calculator and is exposed to long-tail outliers — end-of-shift confirms, weekend confirms, walked-away-from-station,
Core's `ReconciliationService.AutoConfirmStuckDeliveredOrders` flipping stuck-delivered orders after a timeout. Median
lets every outlier class fall out without filtering on a magic detail string.

`P95MarketToCellSeconds` returns the 95th-percentile retrieve duration, not the mean — reshuffle outliers (one-off
long retrieves from blocked lanes) would otherwise pull the mean upward and oversize the cell reorder.

### Formula

```
l1_threshold = ceil(((l1_queue + l1_transit + l2_load + l2_transit)
                     / cycle_seconds) * safety_factor)

cell_reorder = ceil((market_to_cell / cycle_seconds) * safety_factor)
```

No floor and no ceiling — the calculator returns the formula result verbatim. Safety and advisory concerns layered on
top of the math belong in the UI, not in the calculation; the engineer's typed value is the escape hatch for any
result judged un-supportable.

### Confidence

A coverage score over the data the calculator was able to observe in the date range (`scoreConfidence`):

| Label | Condition |
|---|---|
| `HIGH` | window ≥ 14 days AND ≥ 20 each of completed L1 cycles, L2 samples and completed retrieves |
| `MEDIUM` | window ≥ 7 days AND ≥ 10 of each |
| `LOW` | anything below MEDIUM |

The L2 sample count is always 0 (no store orders exist), so **every suggestion reports LOW**. The engineer can still
type a value; on Core the provenance is the uop_threshold audit_log row (see What gets persisted).

### What gets persisted

The calculation itself is not persisted — the engineer applies the suggestion (or types a value) in the `/inventory`
row, which sets the threshold on the loader config (`bin_loader_homes.uop_threshold` or
`bin_loader_payloads.uop_threshold` via the loader service's set-home / set-payload doors), which is where the value
lives. Every change of the value writes one `audit_log` row (entity `loader_home` by position node, or
`loader_payload` by loader; action `uop_threshold`; values `<payload> <old>` → `<payload> <new> <manual|calculated>`;
the actor), whichever page made it. The threshold's hover on `/inventory` says who changed it, when and from what,
with the threshold as time at the 14-day median running rate; a value set before any row existed says so.

---

## Opt-out / opt-in semantics

| `replenish_uop_threshold` | Behavior |
|---|---|
| `0` or no row | Core never monitors. Nothing fires an L1 for that pair automatically; what feeds the loader depends on its replenishment mode (see the default above — operator-driven loaders get the push; threshold-mode loaders with 0 get nothing, loudly). |
| `> 0` | Core monitors and creates orders on crossing. C-push owns L1 firing for that pair. |

The cell claim carries its own provenance: `reorder_point_source` (`legacy` / `manual` / `calculated`) on
`style_node_claims`.

---

## Operational notes

### Warm-up cap

On startup sweep, bindings below threshold get a per-binding warm-up counter seeded to `2` (`warmUpFloor`). The first
fire happens immediately (bypassing debounce); subsequent inventory events during the warm-up window also fire
(bypassing debounce, decrementing the counter) so the first L1 round drives both a bin to the supermarket and a
second bin in flight. After the counter hits zero, normal debounced operation takes over.

### Debounce reset on threshold change

`OnThresholdChanges` is called from the loader config-edit path (`service/loader_service.go`, `rederive()` — after
`store.DeriveDemandRegistry` returns its change list). The sync deletes the station's `demand_registry` rows and
re-inserts the derivation in one transaction; the change list is a diff of threshold values against the rows it
replaced. An empty derivation that skipped an input it could not resolve (a vanished home node, a shared window with
no resolvable address) is refused while the station holds rows: nothing is written, no change is reported, and the
derive line says `outcome=refused`. Edge (re)connect is a separate trigger and calls `Resync` instead:
`HandleEdgeRegister` discards the sync's change list and re-engages the station's bindings wholesale. For every
binding whose threshold value moved, the monitor `delete`s the debounce + warm-up state — so a freshly-applied
threshold takes effect on the next inventory event rather than being suppressed by a residual debounce window from a
previous firing under the old value.

### Cell autoreorder evaluation

The level sweep (`sweepCellLevels`, run by `reconcileDemand` before the episode-close pass) walks every process's
nodes each period and decides each claim on its own (`sweepNodeLevel`). The tick decides nothing about
replenishment (`wiring_counter_delta.go` is an accumulator), so a cell that has stopped ticking — run dry, or a
press at capacity — is still evaluated.

For each node, every arm fails to inaction: a read it cannot make asks for nothing, and the next pass re-decides.

1. A loader node is skipped (its count is an operator declaration, and its replenishment is Core's).
2. The evaluator records the level whatever `auto_reorder` says (`evaluateCellLevel` / `evaluateProduceLevel` stamp
   and clear `style_node_claims.below_reorder_since`), so a plant that has not armed ordering still has the record
   of a cell running short.
3. The ask is derived from the count (`remaining_uop_cached`), never from `below_reorder_since`, which records
   where the level was when something last looked. Within the hysteresis band the flag can still be set while the
   count is above the level; that orders nothing.
4. If breached and `auto_reorder` is on, the dedup is the node: any live order of the node that works the cell
   (`ListActiveOrdersByProcessNode`, filtered by `worksTheCell` so a departed leg or a keep-staged refill does not
   count) means nothing is asked.
5. `CanAcceptOrders` must admit the node (the changeover gate, an active or staged order).
6. The ask is a consume node's material request or a produce node's swap, triggered `autoreorder`. A refusal is
   logged; the level is still breached, so the next pass asks again.

A parked A/B side is evaluated on the same terms as every other node.

### Unbound-slot gap

Between physical pickup of the old bin at the cell and delivery of the new one, no bin is bound: the pickup clears
`active_bin_id` and zeroes `remaining_uop_cached` (`processes.LeaveAtPickup`), and the new bin's count arrives on its
`OrderDelivered` envelope. The bin portion of each tick during the gap accumulates in `pending_uop_delta` and
replays onto the next bin of the part when it binds. A level sweep in the gap reads the zeroed count; what keeps it
from asking is the node dedup (step 4 above), while a live order of the node is bringing the replacement.

### Core's piles are a mirror

The Edge is a pile's only writer, so Core's `lineside_buckets` is a mirror fed by levels, never by deltas: after
every change the Edge sends the row's level, and Core sets its row to it under a per-row sequence guard. Core never
writes a pile. A lost or late message cannot drift the mirror, because the next level replaces it, and the Edge
re-sends every row at boot.

---

## File map

### Edge

- `engine/demand_reconciler.go` — the level sweep (`sweepCellLevels`): cell autoreorder evaluation and the ask.
- `engine/operator_demand_loader.go`, `engine/operator_demand_unloader.go`, `engine/loader_retrieve_ask.go` — the
  push sweeps and the other askers: when to send a `loader.retrieve` ask; Core decides and creates the orders.
- `engine/replenishment_admin.go` — `UpdateCellReorder` (reorder_point / source / auto_reorder on
  style_node_claims).
- `www/handlers_api_replenishment.go` + `www/static/js/pages/replenishment.js` + `www/templates/replenishment.html`
  — cell-autoreorder admin UI.

### Core

- `engine/threshold_monitor.go` — `ThresholdMonitor` (debounce, warm-up, startup sweep, demand episodes, order
  creation via `dispatch.ReplenishLoader`).
- `dispatch/loader_replenish.go` + `dispatch/plan/loader_window_choice.go` — `ReplenishLoader` and its window
  choice, `plan.LoaderWindows`.
- `service/threshold_calculator.go` + `store/orders/lead_time_queries.go` — the calculator and its Postgres lead-time
  helpers.
- `service/inventory_loop.go` + `store/inventory/loop.go` — the `/inventory` read (positions with their thresholds,
  lines with their reorder points).
- `service/inventory_system_count.go` — `SystemUOPForPayload`.
- `store/demands/` — `demand_registry` CRUD including `replenish_uop_threshold`.
- `store/loaders/` + `service/loader_service.go` — `bin_loader_homes.uop_threshold` and
  `bin_loader_payloads.uop_threshold` (where the threshold lives) and the `rederive()` registry sync.
- `www/handlers_loader.go` — `POST /api/loader/calculate` (the calculator endpoint), and the set-home / set-payload
  doors the `/inventory` row saves through (`source` names a `calculated` save).

### Protocol

- `protocol/payloads.go` — `LinesideBucketLevel`, and `LoaderInfo`, which carries threshold values on the node-list
  sync.
