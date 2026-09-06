# Sim timer census — which periodic loops scale with simulated time

Every direct `time.NewTicker` / `NewTimer` / `After` / `Tick` / `Sleep` /
`AfterFunc` in `shingo-core` and `shingo-edge`, classified once so the question
is not re-litigated per site. `integration/` and `shared/` have none.

**51 non-test sites.** 15 sim-relevant (converted), 34 legitimately wall-time
(left alone), 0 unresolved. 96 further sites live in `_test.go` files and are
out of scope — a test that wants a controllable clock uses `clock.Manual`.

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

## The measured ceiling

See [`sim-speed-ceiling.md`](sim-speed-ceiling.md) for what the rig can actually
sustain after this conversion, and the arithmetic the config refuses above.
