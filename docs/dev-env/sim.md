# The Shingo Sim (local dev environment)

A fully-local, Dockerized Shingo stack — core + edge + Postgres + Kafka, with
simulated robots, PLCs, and operators — that runs the whole material flow
unattended without a physical plant. `make dev && make dev-seed` brings it up and
seeds a demo plant.

> **Simulation only — never for production.** All sim code is compiled out of
> production builds and additionally gated at runtime. See [Safety](#safety).

Companion doc: [`plant-model.md`](plant-model.md) — the plant the sim runs.

---

## Goals — why it exists

The sim is a general-purpose dev/test tool *and* a data source. Its jobs:

1. **Local dev environment** — run the full material flow on a laptop / server,
   no plant required.
2. **Test refactors** — a safe, repeatable place to confirm a change didn't break
   the flow end-to-end (a regression safety net you can actually watch run).
3. **Find general bugs** — exercising the whole system flushes out integration
   bugs that unit tests miss (the 2026-06-07 deep dive surfaced several real ones).
4. **Validate data flows** — confirm data moves correctly end-to-end:
   PLC tick → edge → core → Kafka → DB → dashboards.
5. **Feed dashboarding** — generate realistic, sustained data for *all* dashboards
   (shop-floor + exec), which otherwise have nothing to show.
6. **Foundation for deterministic-simulation testing (DST)** — the injected clock
   + seeded PRNG are the groundwork for future high-speed deterministic runs.

The common thread: every one of these needs a **realistic, sustained,
metric-rich plant**. Making the demo plant deliver that is the active work — see
[`plant-model.md`](plant-model.md).

---

## What was built

Built across the `local-dev-env` phases. Real where it matters, faked only at the
hardware seams.

| Piece | Where | What it does |
|---|---|---|
| Build-tag + runtime guards | `//go:build sim`, `SHINGO_ALLOW_SIM=1` | Sim code is absent from prod builds *and* refuses to run without the env flag; prints a NOT-FOR-PRODUCTION banner. |
| Injectable clock | `protocol/clock` | All sim timing goes through an injected `Clock` (real today; the seam for the speed knob + DST). |
| Docker stack | `docker-compose.dev.yml`, `Dockerfile.dev`, `Makefile` | postgres + kafka (KRaft) + core + edge + a one-shot seed service. |
| Fleet simulator | `shingo-core/fleet/simulator` | Clock-driven robot moves with jitter + fault injection, monotonic IDs, eviction; `RobotLister` parity with the real fleet. |
| Fake WarLink PLC | `shingo-edge/plc/simwarlink` | Emits counter ticks per process node; injected at `engine.New`, handshake-verified like a real PLC. |
| Sim operator | `shingo-edge/engine/sim_operator.go` | Performs the operator's LOAD / CLEAR on a configurable delay when a bin is delivered to a `manual_swap` node. |
| Plant spec + validator | `shingo-core/plantspec` | Declarative plant schema (zones, stations, processes, styles, claims, bins, demands, …) + validation. |
| Demo plant | `plants/demo.yaml` | The seeded plant (being rebuilt to a realistic model — see `plant-model.md`). |
| Seed tool | `shingo-core/cmd/seeddev` | Loads + validates a plant spec, seeds core (Postgres, via store accessors) + edge (SQLite, raw SQL), cross-validates. |
| Dev configs | `shingocore.dev.yaml`, `shingoedge.dev.yaml` | Sim knobs (`sim.enabled`, transit time, jitter, fault rate, per-process tick intervals, operator delays). Separate from prod configs. |

---

## How it works

```
            ┌─────────────────────── Docker (docker-compose.dev.yml) ───────────────────────┐
            │                                                                               │
  fake PLC ─┼─▶ shingo-edge ──counter deltas / production.tick──▶ Kafka ──▶ shingo-core ────┼─▶ dashboards
 (simwarlink)│   (SQLite)        ◀── orders / node-list sync ──              (Postgres)      │   (HTTP/SSE)
            │      ▲                                                            │            │
 sim operator│     │ LOAD/CLEAR on delivery                    fleet simulator ─┘            │
 (manual_swap)     │                                          (clock-driven robot moves)     │
            └───────────────────────────────────────────────────────────────────────────────┘
```

- **Real vs faked.** Postgres and Kafka run for real in Docker; core and edge are
  the real binaries. Only the *hardware seams* are simulated: the PLC counters
  (fake WarLink), the operator's manual swaps (sim operator), and the robot fleet
  (fleet simulator). Everything between them is production code.
- **The injected clock** drives all sim timing (robot transit, counter ticks,
  operator delays), so the whole sim can be sped up or — eventually — stepped
  deterministically.
- **The seed** (`make dev-seed`) loads `plants/demo.yaml`, validates it, writes the
  core topology (nodes, bins, payloads, demand registry) and the edge topology
  (processes, styles, claims, runtime states), and cross-checks the two.
- **The material-flow loops** then run on their own: produce → finalize → swap;
  consume → drain → reorder; loader infeed; unloader / customer outfeed.
- **The data flow** worth validating end-to-end: a fake PLC tick → edge counter
  delta (applied to the bound bin's UOP) → `production.tick` / `cell_part_events`
  over Kafka → core → Postgres → the dashboard HTTP/SSE surfaces.

---

## Running it

```sh
make dev          # build images + bring up postgres, kafka, core, edge
make dev-seed     # seed the demo plant (idempotent; --build to pick up edits)
make dev-logs     # tail core + edge
make dev-down     # stop (keep data volumes)
make dev-reset    # stop + drop volumes (fresh DBs next up)
```

UIs (when bound to `0.0.0.0` via the dev configs): core on `:8083`
(`/heartbeat`, dashboards), edge on `:8081`.

### Reading the Edge database: copy the WAL too

Edge SQLite runs in WAL mode, so **the main database file on its own is a stale
snapshot**. Copying only `shingoedge.db` out of the container hands you rows
that are self-consistent and minutes old — a state you drove through the API
"did not happen", and nothing errors. Take all three files:

```sh
docker compose -f docker-compose.dev.yml cp edge:/data/shingoedge.db     /tmp/e.db
docker compose -f docker-compose.dev.yml cp edge:/data/shingoedge.db-wal /tmp/e.db-wal
docker compose -f docker-compose.dev.yml cp edge:/data/shingoedge.db-shm /tmp/e.db-shm
```

(Cost one wrong reading during the 2026-08-24 sim run: a 4 MB `-wal` sat beside
a main file that had last been checkpointed minutes earlier.)

### The sim operator releases everything on a timer

`sim.operators.swap_release` defaults to **3 s of sim time**, which at
`sim.speed: 10` is 0.3 s of wall clock. That is a good imitation of a person and
a poor instrument: any scenario that needs to observe a HELD release — or to
beat the sim operator to a release door and click it — has a third of a second
to do it. Widen it (`swap_release: 300s` ≈ 30 s wall) and **rebuild the edge
image**, since the config is baked in; restore it afterwards. The knob's own
comment in `sim_operator.go` says the same thing.

Pacing knobs live in the dev configs — `sim.transit_time` (core),
`sim.processes[].tick_interval` + `sim.operators.*` (edge). `sim.speed` scales
every sim duration (transit, ticks, operator / downtime / changeover delays) for
fast-forward dev loops. An optional **finite-fleet** model (`sim.fleet_size`,
plus `sim.transit_min`/`sim.transit_max` for realistic minutes-long crossings)
lets the sim answer "how many robots does this plant need?" — orders queue for a
free robot and the driver tracks utilization. It is off by default, so the demo
runs as the legacy infinite fleet (one robot per active order). A live dev-mode
top-strip is on the roadmap.

---

## Use cases

- **Validate a refactor.** Bring the stack up, seed, watch the loops keep cycling
  and the data keep flowing. If the flow stalls or the numbers go wrong, the
  refactor broke something.
- **Reproduce / flush a bug.** Run it sustained and inspect the live DBs (see the
  2026-06-07 deep dive for the pattern). The sim reproduces real integration bugs.
- **Validate a data flow.** Trace a PLC tick all the way to a dashboard chart and
  confirm each hop is correct.
- **Generate dashboard data.** A sustained, metric-rich run populates the
  shop-floor + exec dashboards; running it *fast* yields time-series for trends.
- **(future) Deterministic tests.** A manual clock + seeded PRNG for repeatable,
  high-speed scenario runs.

---

## Safety

- All sim files carry `//go:build sim`, so they are **absent from production
  builds** entirely.
- At runtime, sim mode additionally requires `SHINGO_ALLOW_SIM=1`; otherwise it
  refuses to start. A NOT-FOR-PRODUCTION banner prints on startup.
- The sim reads only `*.dev.yaml` configs and **never touches production config
  files**.
- `-race` builds need cgo (WSL / CI); the Windows dev box runs the non-race suite.

---

## Status

The sim *infrastructure* (above) is built and verified end-to-end on a real Docker
host. The **demo plant** is being rebuilt from a sterile, jam-prone fixture into a
realistic, sustained, metric-rich plant — that work, plus the data features
(downtime / changeover / quality / customer-demand), the `sim.speed` knob, and the
sub-line → main-line WIP hierarchy, is documented in
[`plant-model.md`](plant-model.md) as it lands.
