# The sim speed ceiling — measured, with the per-leg profile it comes from

Measured on the houseserver dev rig, 2026-09-06, at tip `ea1aa869`, after the
timer conversion in [`sim-timer-census.md`](sim-timer-census.md). Every run is a
`down -v`, a fresh anchor, `--profile tools build`, both plants seeded, three
minutes of settle, then a six-minute sample window. Nothing is estimated.

## Headline

**The ceiling is 5×.** `clock.DefaultSimMaxSpeed` refuses more.

| | 2× | **5×** | 10× |
|---|---|---|---|
| achieved multiplier | 2.00 | **5.00** | 9.98 |
| orders finished / **wall** min | 9.83 | **21.50** | 4.65 |
| orders finished / sim min | 4.92 | **4.30** | 0.47 |
| mean order life (sim-s) | 104 | **102** | 85 \* |
| backlog growth over window | 0 | **+2** | +6 |
| open at end | 11 | **9** | 33 |
| p95 age of open orders (sim-s) | 927 | 2403 | 5567 |
| box load average | 0.39 | 0.93 | 0.44 |

\* survivors only — the orders that never finished are excluded, which flatters it.

**10× is not merely no better than 2×, it is less than half the real throughput**,
and the box is at 0.44 load while it happens. The limit was never CPU. 5× keeps
the plant economy of 2× — the same order lifetime, the same queue depth, backlog
essentially flat — and does 2.2× the work per real minute.

6× through 9× are untested and therefore not permitted. The cap is the fastest
speed observed to work, not the fastest that might.

## Where an order's life actually goes

From `order_history`, the 60 most recently confirmed orders per run. All stamps
are simulated, so **wall-ms = sim-ms ÷ speed**. p50, because leg 1 is bimodal
(an order either sources at once or waits for material, and the mean describes
neither).

| leg | governed by | 2× sim-ms | 2× **wall** | 5× sim-ms | 5× **wall** | 10× sim-ms | 10× **wall** | verdict |
|---|---|---|---|---|---|---|---|---|
| 1 queued→sourcing | `fulfillment/scanner.go:114` (sim) | 53 | 26 ms | 128 | 26 ms | 595 | 60 ms | wall-bound, then degrades |
| 2 sourcing→dispatched | in-process Core dispatch | 39 | **20 ms** | 99 | **20 ms** | 194 | **19 ms** | **wall-bound, flat** |
| 3 dispatched→in_transit | fleet driver + Kafka hop | 2315 | 1158 ms | 2312 | 462 ms | 2557 | 256 ms | **sim-scaled** |
| 4 in_transit→delivered | `driver.go` transit, `clk.After` | 79495 | 39748 ms | 78352 | 15670 ms | 60571 | 6057 ms | **sim-scaled** |
| 5 delivered→confirmed | outbox drain + `reconciliation_service.go:98` | 1174 | 587 ms | 2329 | 466 ms | 2381 | 238 ms | mixed |
| **total** | | 97068 | 48.5 s | 83133 | 16.6 s | 65362 | 6.5 s | |

How to read the verdict column: a **sim-scaled** leg holds its *sim* duration
across speeds (leg 4: 79495 → 78352 → 60571) and so its wall cost falls with
speed. A **wall-bound** leg holds its *wall* duration (leg 2: 20 → 20 → 19 ms)
and so its sim cost multiplies with speed.

Two things fall straight out:

- **Leg 4 is 76–82% of an order, and it is sim-scaled.** Robot travel already
  compresses correctly at every speed. The config comment that named "simulated
  robot travel" as a wall-time leg was wrong, and it pointed tuning at the one
  leg that was already right. Corrected in `shingocore.dev.yaml`.
- **Per-order wall latency compresses better than linearly** (48.5 s → 6.5 s for
  a 5× increase). So per-order latency is not the ceiling. The ceiling is
  throughput, and it lives outside the order's own timeline.

## The budget, and which machine sets it

At speed S, one simulated second is worth `1000/S` wall-ms:

```
2×  →  500 wall-ms per sim-second
5×  →  200 wall-ms per sim-second
10× →  100 wall-ms per sim-second
```

The wall-bound legs above spend ~46 ms per order (leg 1 + leg 2 + leg 5's
floor). That is nothing. **The dominant wall-bound constant is not in the
per-order table at all — it is the Edge outbox drain interval, 5 000 wall-ms**
(`protocol/outbox/drainer.go:170`, `outbox_drain_interval: 5s`), two orders of
magnitude larger than everything else measured. It is the backstop cadence for
every Edge→Core message, so in simulated time it costs `5 × S` seconds per hop:

```
2×  →  10 sim-s      5× →  25 sim-s      10× →  50 sim-s
```

### That attribution was tested, not asserted

Same 10×, same everything, one value changed — Edge `outbox_drain_interval`
5s → 500ms:

| 10× | 5 s drain | 500 ms drain |
|---|---|---|
| finished / **wall** min | 4.65 | **34.50** |
| open at end | 33 | 17 |
| mean order life (sim-s) | 85 | 110 |
| box load | 0.44 | 2.46 |

**A 7.4× throughput improvement from one interval**, and the load average finally
rising to 2.46 — the box was idle before because it was waiting, not working.

## Why the drain interval is not simply lowered

Because it is not one policy, it is three (`shingo_outbox_interval_three_policies`):
the drain cadence, the wake-settle coalescing window, and — as
`MaxRetries × interval` — **the dead-letter budget**. At 5 s that is a 50-second
tolerance for a real broker to come back. At 500 ms it is 5 seconds, and an
ordinary Kafka reconnect starts dead-lettering live order messages.

So the change that raises the ceiling is not a smaller number. It is **splitting
the two**: cadence on the sim clock, dead-letter budget on the wall. That is the
same shape `clock.ScaleTTL` already solves for envelope expiry, applied to the
drainer. Designed change, not a sed — which is why it is written down here and
not done.

## What limits a long run at ANY speed

Speed is not the only thing bounding this rig, and it is not the worst thing.
A `single_robot` swap self-deadlocks against the sim's position gate: two robots
are lost in the first three minutes of every seeded run, permanently. It is not
speed-related and it is sim-only (a real fleet gets position physics from the
floor, not from this gate). Write-up is an issue, not repo reference:
`ISSUE-sim-position-hold-deadlock-2026-09-06.md` at the GitHub root. Raising the
speed ceiling without fixing that just reaches the same wall sooner.

## Reproducing

`scripts/sim-anchor.sh mint` after every `down -v`, `--profile tools build`
always (the seeder carries its own migration list), three minutes of settle
before sampling, and take rates per **wall** minute. Per *sim* minute flatters a
starving rig: at 10× it reported 0.47 finished/sim-min, which looks like a tenth
of the work rather than what it was — the same box doing half as much.
