# The Shingo Sim (local dev environment)

Verified against: 74c3eb77

A fully-local, Dockerized Shingo stack — core + edge + Postgres + Kafka, with
simulated robots, PLCs, and operators — that runs the whole material flow
unattended without a physical plant. How to bring it up, seed it, tune it and reset it is
[README.dev.md](../../README.dev.md); this doc holds the rules the stack runs by. The plant it runs is
`plants/master.yaml` ([plants/README.md](../../plants/README.md)); writing a section of it is
[sim-plant-design.md](sim-plant-design.md).

> **Simulation only — never for production.** All sim code is compiled out of
> production builds and additionally gated at runtime. See [Safety](#safety).

---

## The pieces

Real where it matters, faked only at the hardware seams.

| Piece | Where | What it does |
|---|---|---|
| Build-tag + runtime guards | `//go:build sim`, `SHINGO_ALLOW_SIM=1` | Sim code is absent from prod builds *and* refuses to run without the env flag; prints a NOT-FOR-PRODUCTION banner. |
| Injectable clock | `protocol/clock` | All sim timing goes through an injected `Clock`; `sim.speed` scales it (the speed ceiling below). |
| Docker stack | `docker-compose.dev.yml`, `Dockerfile.dev`, `Makefile` | postgres + kafka (KRaft) + core + two edges + the one-shot seed services (`seed`, `seed-edge2`). |
| Fleet simulator | `shingo-core/fleet/simulator` | Clock-driven robot moves with a transit draw and fault injection; a finite pool of named robots when `sim.fleet_size` is set; `fleet.RobotLister` like the real fleet. |
| Fake WarLink PLC | `shingo-edge/plc/simwarlink` | Emits counter ticks per process node while the machine can cycle (`engine.SimMachineReady`); injected at `engine.New`, handshake-verified like a real PLC. |
| Sim operator | `shingo-edge/engine/sim_operator.go` | What a person at the station does: RELEASE a staged swap, the receipt for a delivered swap leg, LOAD / CLEAR at `manual_swap` loaders and unloaders, the unloader panel's taps, and the scheduled changeovers (`sim_changeover.go`). [sim-plant-design.md](sim-plant-design.md) §4 has each. |
| Plant spec + validator | `shingo-core/plantspec` | Declarative plant schema (zones, stations, processes, styles, claims, bins, demands, kanbans, …) + validation. |
| The plant | `plants/master.yaml` | The one plant (compose's default `SHINGO_PLANT`), in tagged sections: the demo loop, the 4x2-shaped cell and the lane-stress corridor, the kanban cells, line 2 on Edge 2. `plants/README.md` maps each section to the scenario family that proves it. |
| Kanban rig | `plants/master.yaml` `[kanban]`/`[kanban5]`, `scripts/kanban-rig.sh` | The Kanban certification cells, their runs and their walk ([kanban-rig.md](kanban-rig.md)), on the master; the operator works their unloaders' panels (`sim.operators.unloader_taps`). |
| Seed tool | `shingo-core/cmd/seeddev` | Loads + validates a plant spec, seeds core (Postgres, via store accessors) + edge (SQLite, raw SQL), cross-validates. |
| Dev configs | `shingocore.dev.yaml`, `shingoedge.dev.yaml`, `shingoedge.edge2.yaml` | Sim knobs (`sim.enabled`, speed, transit time, fleet, fault rate, per-process tick intervals, operator delays and changeovers). Separate from prod configs. |

---

## How it works

```
            ┌─────────────────────── Docker (docker-compose.dev.yml) ───────────────────────┐
            │                                                                               │
  fake PLC ─┼─▶ shingo-edge ──counter deltas / production.ticks─▶ Kafka ──▶ shingo-core ────┼─▶ dashboards
 (simwarlink)│   (SQLite)        ◀── orders / node-list sync ──              (Postgres)      │   (HTTP/SSE)
            │      ▲                                                            │            │
 sim operator│     │ RELEASE / LOAD / CLEAR                    fleet simulator ─┘            │
            │      │                                          (clock-driven robot moves)     │
            └───────────────────────────────────────────────────────────────────────────────┘
```

- **Real vs faked.** Postgres and Kafka run for real in Docker; core and edge are
  the real binaries. Only the *hardware seams* are simulated: the PLC counters
  (fake WarLink), the operator at the station (sim operator), and the robot fleet
  (fleet simulator). Everything between them is production code.
- **The injected clock** drives all sim timing (robot transit, counter ticks,
  operator delays), so the whole sim can be sped up.
- **The seed** (`make dev-seed`) loads `SHINGO_PLANT` (default `plants/master.yaml`), validates it, writes the
  core topology (nodes, bins, payloads, demand registry, every Edge it names enrolled) and each
  Edge's topology (processes, styles, claims, runtime states; `seeddev --edge` picks the Edge),
  and cross-checks the two.
- **The material-flow loops** then run on their own: produce → finalize → swap;
  consume → drain → reorder; loader infeed; unloader / customer outfeed.
- **The data flow** worth validating end-to-end: a fake PLC tick → edge counter
  delta (applied to the bound bin's UOP) → `production.ticks` over Kafka → Core, which writes
  `cell_part_events` → the dashboard HTTP/SSE surfaces.

---

## Seed with Core STOPPED

`make dev-seed` stops Core across the seed and starts it afterwards. Seeding behind a running Core is wrong
for a bring-up you intend to measure, because Core's engines are writing to the
same tables the seeder is rewriting: the startup reconciliation sweep, the threshold monitor and the
station service all run against a half-seeded topology and record what they find
— adopted process nodes, orphaned rows, anomalies — which then sits in the run
you are about to read as if the plant had produced it.

For a clean run from scratch (no `make` on the Windows host, so these are the steps
by hand). Both Edges: the one plant runs line 2 on Edge 2, and `seed-edge2` seeds it
from the same file with `--edge edge2.line1`:

```sh
docker compose -f docker-compose.dev.yml down -v
bash scripts/sim-anchor.sh mint                       # only ever alongside down -v
docker compose -f docker-compose.dev.yml --profile tools build
SHINGO_BIND=0.0.0.0 docker compose -f docker-compose.dev.yml up -d postgres kafka core edge edge2
docker compose -f docker-compose.dev.yml stop core     # <- the rule
docker compose -f docker-compose.dev.yml run --no-deps --rm seed
docker compose -f docker-compose.dev.yml run --no-deps --rm seed-edge2
docker compose -f docker-compose.dev.yml start core
docker compose -f docker-compose.dev.yml restart edge edge2
```

Core is brought up FIRST and then stopped rather than never started, because
Edge migrates `/data/shingoedge.db` at boot and the seeder writes into a
migrated schema; `--no-deps` is what stops `compose run` restarting Core through
the dependency chain (seed → edge → core). Starting Core at the end runs its
startup sweeps against the seeded registry, and restarting the Edges reloads the
runtime states the seed wrote behind their startup cache; `dev-seed` does the
same.

## Reading the Edge database: copy the WAL too

Edge SQLite runs in WAL mode, so **the main database file on its own is a stale
snapshot**. Copying only `shingoedge.db` out of the container hands you rows
that are self-consistent and minutes old — a state you drove through the API
"did not happen", and nothing errors. Take all three files:

```sh
docker compose -f docker-compose.dev.yml cp edge:/data/shingoedge.db     /tmp/e.db
docker compose -f docker-compose.dev.yml cp edge:/data/shingoedge.db-wal /tmp/e.db-wal
docker compose -f docker-compose.dev.yml cp edge:/data/shingoedge.db-shm /tmp/e.db-shm
```

## The sim operator releases everything on a timer

`sim.operators.swap_release` defaults to **3 s of sim time**, which at
`sim.speed: 10` is 0.3 s of wall clock. That is a good imitation of a person and
a poor instrument: any scenario that needs to observe a HELD release — or to
beat the sim operator to a release door and click it — has a third of a second
to do it. Widen it (`swap_release: 300s` ≈ 30 s wall) and **rebuild the edge
image**, since the config is baked in; restore it afterwards. Edge 2 runs 60 s
(`shingoedge.edge2.yaml`), which is what makes line 2 the observable window. The knob's
own comment (`swapReleaseDelay` in `sim_operator.go`) says the same thing.

## The speed ceiling is 10×

`clock.DefaultSimMaxSpeed` (`protocol/clock/sim_clock.go`) refuses more, and
Core and Edge cap at the same const so their fast-forward clocks cannot drift. Above 10× is untested.

`sim.speed` is set in the dev configs, 10 in `shingocore.dev.yaml` and every Edge's, and it must be
identical in all of them: change it there, never through `POST /api/sim/speed`, which re-anchors one
process and breaks the Core-Edge clock sync the shared anchor (`SHINGO_SIM_ANCHOR`,
`scripts/sim-anchor.sh`) holds. Changing it is a `make dev-reset`, not a restart: sim-now is
`epoch + speed × (wallNow − anchor)`, so a new speed retimes the world while the rows already written keep
their stamps, and an in-flight order left stamped in the future never moves again.

To measure a speed change, take rates per **wall** minute after three minutes of
settle — per *sim* minute flatters a starving rig, reporting less work as if it
were more.

## Which loops scale with sim time

A periodic loop takes its cadence from `clock.Default()` when the work it paces
is **simulated**, and calls `time` directly when the work is **real**. Read the
clock at call time, never cached into a struct field at construction — sim
startup installs the SimClock during boot, and a clock captured before that is
the real one forever.

Wall-time loops stay wall by *kind*, not by exception: SSE keepalives to a live
browser, Kafka reconnect backoff, retention and rotation, HTTP to real external
systems (SEER RDS, CMS), panic-restart and rebind supervision, debounce
protecting something real, and anything paired with a SQL `NOW()` or a wall stamp
written elsewhere. **A wall stamp and a sim stamp must never be differenced** —
converting one half of such a pair is the defect, not the fix.

The same split runs one layer down, in Postgres: a column is on sim time if the
Go insert passes `clock.Now()` and on wall time if it lets the DDL's `DEFAULT
now()` fire. That is invisible on a plant, where both clocks are wall, and bites
only on the rig — `bin_uop_ledger.applied_at` against `orders.created_at`
returns clock drift, not a duration.

## There is no downtime model

The sim gives no machine random outages. A machine stops only when it is starved, full or holding no
carrier (`engine.SimMachineReady`), off shift (when `sim.calendar` is enabled), or stopped by a case. A
wait in a sim run is therefore something the code or the plant did, never a simulated breakdown.

## The 4x2-shaped cell, end to end

`integration/scenarios/catid_4x2_*_test.go` (`-tags docker`) run Core and an Edge in one process on a
five-position, two-style weld (RUN and ALT share three parts and differ at two mirror positions, CATIDs
1001/1002 pinned) whose PLC is `simwarlink.Line`, turned one machine cycle and one Edge poll pass at a
time. Each shape checks the carts against the line's loads, the holds ledger, and the production record
(counter_snapshots, hourly, what Core received), and that every order lands confirmed: S1 a steady run on
the load station's counter and on the cell counter, S2 the late cutover (the declaration on a green gate,
the counted-part force on a red one), S3 the early cutover, S4 a jump with no changeover (nothing moves; the
wrong-part item offers the changeover, untapped and tapped), S5 an unmapped part (Charge and Write off), S6
a mixed cell, S7 a restart, S8 the PLC moving its count to a sibling MES slot (the slot watch's item, the
click that re-points and counts the strokes since the move), S9 the hand-placed cart; and
`TestPinChangeover_ACommonPositionIsResuppliedWhileOpen` a common position supplied through an open
changeover. `make dev-sim-4x2 SHAPE=S3` (`scripts/sim-4x2.sh`; `VARIANT=tap` taps S4's and S9's item) runs
one shape on the dev stack for a person to watch and prints the same ledger line, and beside it
`lost at the lift N` (Core's stale drops on a cart released in the window and the draws a release's count
undid), asserted 0 and never summed.

```bash
cd integration
go test -tags docker -count=1 -run Catid4x2 ./scenarios/
```

---

## Safety

- All sim files carry `//go:build sim`, so they are **absent from production
  builds** entirely.
- At runtime, sim mode additionally requires `SHINGO_ALLOW_SIM=1`; otherwise it
  refuses to start. A NOT-FOR-PRODUCTION banner prints on startup.
- The sim reads only the dev configs (`shingocore.dev.yaml`, `shingoedge.dev.yaml`,
  `shingoedge.edge2.yaml`) and **never touches production config files**.
- `-race` builds need cgo (WSL / CI); the Windows dev box runs the non-race suite.
