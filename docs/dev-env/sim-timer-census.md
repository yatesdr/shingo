# Sim timer census — which periodic loops scale with simulated time

Every direct `time.NewTicker` / `NewTimer` / `After` / `Tick` / `Sleep` /
`AfterFunc` in `shingo-core`, `shingo-edge` and `protocol`, classified once so
the question is not re-litigated per site. `integration/` and `shared/` have
none.

**51 non-test sites in core and edge.** 15 sim-relevant (converted), 34
legitimately wall-time (left alone), 0 unresolved. 96 further sites live in
`_test.go` files and are out of scope — a test that wants a controllable clock
uses `clock.Manual`.

**Plus two in `protocol/outbox`, and they turned out to set the rig's speed
limit.** The first census run was scoped to core and edge, which is where the
loops looked like they lived; the outbox drainer is shared code and sat outside
it. It was found by measurement rather than by reading — see
[`sim-speed-ceiling.md`](sim-speed-ceiling.md) — which is the honest order for
this kind of thing but not the cheap one. **A census is only as good as its
scope: state the scope, and when a measurement disagrees with it, widen the
scope rather than the explanation.**

## The rule

A loop takes its cadence from `clock.Default()` when the work it does is
**simulated**, and calls the standard library directly when the work is **real**.

That is not a style preference. `store/bins` releases a staged bin when
`staged_expires_at < clock.Now()` — simulated time — while the sweep that calls
it ran on a wall ticker: at N× the world produced expiries N times faster than
the loop clearing them. `engine/reconciliation_service.go` found the same seam
from the other side and its own comment says so — the *comparison* was moved to
`clock.Now()`, the *loop* driving it was not. Half a conversion is worse than
none, because it reads as done.

The reverse mistake is equally real and the tree already documents one:
`threshold_monitor_lineside.go` deliberately uses `time.Now()` because Core
compares a wall stamp the edge reporter writes. Converting that reporter alone
would make every lineside report look hours stale the moment the clock moved.
**A wall stamp and a sim stamp must never be differenced.** That is the whole
rule, in both directions.

## Bucket (i) — sim-relevant pipeline cadence · CONVERTED (15)

| Site | Loop | Why it scales |
|---|---|---|
| `shingo-core/dispatch/eta/medians.go:114` | `(*Cache).run` | rebuilds ETA medians from sim-written `order_history` |
| `shingo-core/engine/demand_reconciler.go:60` | `runDemandReconciler` | sweeps demand episodes over sim domain state |
| `shingo-core/engine/engine_background.go:23` | `robotRefreshLoop` | polls the sim fleet; drives `sweepCarriedBins` |
| `shingo-core/engine/engine_background.go:113` | `laneLivenessFloorLoop` | release floor over stuck orders and dig holds |
| `shingo-core/engine/engine_background.go:175` | `stagedBinSweepLoop` | **the sharpest case** — expiry is `clock.Now()`, sweep was wall |
| `shingo-core/engine/maintainer.go:111` | `(*Maintainer).Run` | maintained-group level keeper over live carrier counts |
| `shingo-core/engine/reconciliation_service.go:98` | `(*ReconciliationService).Loop` | auto-confirm / abandon cutoffs already use `clock.Now()` |
| `shingo-core/engine/sourceability_monitor.go:110` | `(*SourceabilityMonitor).Run` | full recompute over sim bins/orders/reservations |
| `shingo-core/fulfillment/scanner.go:114` | `StartPeriodicSweep` | sourcing retry sweep over queued/acquiring orders |
| `shingo-edge/engine/demand_reconciler.go:51` | `runDemandReconciler` | `sweepCellLevels` — the replenishment level keeper |
| `shingo-edge/engine/uop_stranded_monitor.go:162` | `(*strandedMonitor).run` | samples sim-produced `pending_uop_delta` growth |
| `shingo-edge/messaging/production_reporter.go:68` | `(*ProductionReporter).loop` | flushes sim production deltas to Core |
| `shingo-edge/plc/manager.go:256` | `warlinkPollLoop` | **the sim ingest path** — see note below |
| `shingo-edge/plc/manager.go:539` | `pollLoop` | counter deltas off the fake PLC, counting at sim rate |
| `shingo-edge/uop/accumulator.go:380` | `(*accumulator).loop` | flushes UOP deltas to the outbox |

`plc/manager.go:256` deserves its own line. `warlink.mode` is `poll` on the dev
stack, so `ReadTag` serves from a cache only this loop refreshes — it is not a
health check, it is the ingest. Left at wall rate, a rising multiplier packs
more counts into each sample until one crosses `CalculateDelta`'s
`JumpThreshold` and the delta is suppressed as an operator-gated jump. The
failure gets strictly more likely as speed rises, which is the shape of a defect
that only appears when you finally crank the rig.

## Bucket (ii) — legitimately wall-time · UNTOUCHED (34)

Real browsers, real sockets, real files, real people.

**SSE keepalives to a live browser** — `shingo-core/www/sse.go:499`,
`shingo-edge/www/sse.go:236`. A browser's socket times out in wall seconds.

**Kafka client behaviour** — `shingo-core/messaging/client.go:259,357`,
`shingo-edge/messaging/client.go:225`, `shingo-edge/messaging/data_sender.go:33`,
`shingo-edge/cmd/shingoedge/main.go:778`. Reconnect backoff against a real broker.

**Retention, rotation and partition housekeeping** —
`shingo-core/cmd/shingocore/main.go:494,556`,
`shingo-core/messaging/core_data_service.go:154,857`,
`shingo-edge/cmd/shingoedge/main.go:801,894`,
`shingo-edge/messaging/plant_claims_publisher.go:83`,
`shingo-edge/backup/service.go:192`. Disk and rows age in real time.

**Real external systems** — `shingo-core/rds/poller.go:160,168` (SEER RDS HTTP;
sim swaps the adapter out entirely), `shingo-core/cms/poster/poster.go:214,221`
(CMS middleware HTTP), `shingo-core/engine/engine_map_sync.go:54` (vendor
`.smap`), `shingo-edge/plc/sse.go:180,270,434` (a real socket — and inert under
sim, which selects poll mode).

**Wall-paired by design** — `shingo-core/messaging/core_handler.go:188` and
`shingo-edge/messaging/heartbeat.go:228` both pair with a SQL `NOW()`
comparison; `shingo-edge/engine/lineside_reporter.go:38` pairs with
`threshold_monitor_lineside.go`, whose comment forbids the conversion by name.
Converting one half of a pair is the defect, not the fix.

**Real resources and supervision** — `engine_connection.go:76` (DB ping),
`heartbeat.go:255` (panic restart backoff), `main.go:236` (listener rebind),
`poller.go:160` (shutdown drain), `testdb.go:412` (container readiness).

**Rate limits and debounce protecting something real** —
`sourceability_monitor.go:146` (300 ms event coalescer bounding CPU),
`backup/service.go:222` (10 s debounce over operator edits),
`handlers_config.go:157` (spacing two test emails to a real inbox),
`threshold_monitor.go:367` (one-shot boot grace ordered against `uop_backfill`).

## The two that needed a decision

Both were flagged unclear by the census and both resolve to **wall-time**:

- **`shingo-core/engine/wiring.go:663`** — the one-minute fault-email buffer.
  The recipient is a person and the window means "a minute of real time before
  we page somebody", which does not compress just because the plant is
  simulated. Notifications are also not enabled on the dev stack, so the site is
  inert there regardless. Stays wall.

- **`shingo-edge/engine/plc_catid_monitor.go:166`** — 500 ms poll, 2 s debounce,
  60 s stability window. Those numbers model **electrical signal noise on a real
  PLC**, not changeover cadence; noise does not speed up with the sim. It is
  additionally inert under the dev fixture: `plants/demo.yaml` uses
  `PRESS-1_COUNTER`-style tags, which `deriveIdentityTag` rejects (it wants
  `MES_*.<leaf>`), so `prime()` registers nothing and the loop polls an empty
  map. Stays wall.

## How to convert one, if a future site belongs in (i)

`clock.Default()` returns the process clock — a `*SimClock` on a sim stack, the
real clock everywhere else, so a converted site is byte-equivalent in
production with one interface hop. Read it at **call** time, never cached into a
struct field at construction: sim startup installs the SimClock during boot, and
a clock captured before that is the real one forever.

```go
ticker := clock.Default().NewTicker(interval)
defer ticker.Stop()
for {
    select {
    case <-ticker.C():          // C() is a method on clock.Ticker, not a field
```

## `protocol/outbox` — the two sites the first scope missed

| Site | Loop | Bucket |
|---|---|---|
| `protocol/outbox/drainer.go:170` | `(*Drainer).run` ticker | **(i) by nature, left wall ON PURPOSE** |
| `protocol/outbox/drainer.go:204` | wake-settle timer (50 ms) | **(i) by nature, left wall ON PURPOSE** |

The drainer moves every order message across the Kafka seam, so its cadence
absolutely governs simulated work — it is bucket (i) by the rule above. It is
still on the wall clock, deliberately, and the reason is in
`shingo_outbox_interval_three_policies`: **the drain interval is not one policy,
it is three.** It sets the drain cadence, and the wake-settle coalescing window,
and — as `MaxRetries x interval` — the dead-letter budget. Scaling the cadence
with sim speed would divide the REAL budget a REAL broker gets to recover by the
multiplier: at 10x, a 50-second tolerance becomes 5 seconds, and a normal
reconnect starts dead-lettering live order messages.

That is the same trap `clock.ScaleTTL` exists to compensate for, approached from
the other side, and it is why this one is not a mechanical conversion. Splitting
it — cadence on the sim clock, dead-letter budget on the wall — is the change
that would raise the ceiling, and it is a designed change, not a sed.

## The measured ceiling

See [`sim-speed-ceiling.md`](sim-speed-ceiling.md) for what the rig can actually
sustain after this conversion, and the arithmetic the config refuses above.

## A second clock split, one layer down: the database

Timestamps in Core's Postgres are on TWO clocks, and which one a column gets
was never a decision — it is whether the Go insert passes `clock.Now()` or lets
the DDL's `DEFAULT now()` fire. `now()` is the DATABASE SERVER's wall clock.

Measured on the rig at 10x, wall 07:50:03 against sim 09:30:

**Simulated** — `orders.created_at`, `orders.updated_at`,
`order_history.created_at`, `bins.updated_at`, `mission_events.created_at`,
`mission_telemetry.created_at`, `sourceability_events.observed_at`,
`reservations.created_at`.

**Wall** — `bin_uop_ledger.applied_at`, `production_tick_dedup.applied_at`,
`inventory_delta_dedup.updated_at`, `downtime_event_dedup.applied_at`,
`outbox.created_at`, `inbox.processed_at`, `audit_log.created_at`,
`recovery_actions.created_at`, `nodes.updated_at`,
`edge_lineside_reports.updated_at`.

Several of those are RIGHT on wall and deliberately so — the outbox's retention
and dead-letter budgets are real-time budgets, `edge_lineside_reports` is the
wall-paired half the census names, and the dedup tables are plumbing. The
problem is not the split, it is that the split is invisible and undeclared, so
nothing stops a query from crossing it.

**`bin_uop_ledger` is the one that bites.** It is the production ledger and the
natural thing to join against `orders` — "how long after the UOP delta did the
order confirm?" — and that subtraction returns the clock drift, not a duration.
It fooled the author of this document during the run that produced it: the
ledger's newest row read 43 sim-minutes stale, which looked exactly like
production having stopped. It had not. A wall stamp was being differenced
against sim-now, which is the defect this whole unit exists to remove, wearing a
different hat.

Invisible on a plant, where both clocks are wall. It surfaces only on the rig —
the one place people go to gather evidence about the plant.

Not fixed here. Fixing it means auditing every insert against the 61 columns
carrying `DEFAULT now()` and deciding each one, which is its own unit of work
with its own migration; doing it hastily at the end of this one would be
guessing at ten answers to get one. The detection is cheap and exact, so it can
be redone in a minute: on a running sim stack compare `max(col)` for every
timestamp column against `now()` and against `/api/sim/status`.
