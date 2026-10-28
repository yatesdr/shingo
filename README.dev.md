# Shingo Local Dev Environment

**LOCAL DEV / SIMULATION ONLY — never run this against production data or hosts.**

A self-contained Shingo system — core + edge + Postgres + Kafka in Docker, with
**simulated** robots, PLCs, and operators — so you can watch the whole material
flow run unattended without a plant. Sim code is compiled behind `-tags sim` and
refuses to start unless `SHINGO_ALLOW_SIM=1`, printing a loud NOT-FOR-PRODUCTION
banner.

## Prerequisites

- Docker + Docker Compose on the host (the build runs Go *inside* the container,
  so no local Go toolchain is needed).
- ~2 GB free RAM for the stack (kafka 768m, postgres 512m, core/edge 256m each).

## Quickstart

```sh
make dev          # build the sim images + bring up postgres, kafka, core, edge
make dev-seed     # seed the rig plant (plants/demo-sim.yaml) with Core stopped across it
```

`make` is not installed on the Windows dev host: run the same steps by hand from
[`docs/dev-env/sim.md`](docs/dev-env/sim.md), "Seed with Core STOPPED". Seed
`plants/demo-sim.yaml` (compose's default `SHINGO_PLANT`), not `plants/demo.yaml`:
demo.yaml is the planner golden's plant and gives the unloader no Core `auto_push`,
so a sim seeded from it wedges within a few sim-hours behind full ASSY.

A build from a git worktree works, but its binaries report `version=dev commit=unknown`:
the worktree's `.git` points outside the build context, so Dockerfile.dev drops it. Build
from a normal clone when the version stamp matters.

Then open the UIs:

| | default (loopback) | LAN / tailnet |
|---|---|---|
| Core | http://localhost:8083 | set `SHINGO_BIND=0.0.0.0` (or a tailnet IP) before `make dev` |
| Edge 1 | http://localhost:8081 | same |
| Edge 2 | http://localhost:8082 | same; only in the two-Edge bring-up (`edge2` + `seed-edge2`, see sim.md) |

`SHINGO_BIND` only affects the core + edge UIs; Postgres and Kafka stay
loopback-only. Tip: `echo "SHINGO_BIND=0.0.0.0" >> .env` makes it stick across
`make dev` runs (Compose auto-loads `.env`; it's gitignored). Append, never `>`:
`.env` also holds `SHINGO_SIM_ANCHOR` (scripts/sim-anchor.sh) and `SHINGO_PLANT`, and
losing the anchor re-anchors the next bring-up over live volumes.

Until you run `make dev-seed`, the UIs come up **empty** — the sim engine is
alive (driver running, fake PLCs connected, counters ticking) but has no plant
to act on.

## What the demo plant gives you

A self-sustaining circle (the full map is the header of `plants/demo-sim.yaml`):

1. **Main circle** — `PRESS-1` (PANEL-A, two_robot_press_index) and `PRESS-2` (PANEL-B,
   A/B sequential) fill carriers into the mixed-fill `SYN_MARKET`; `WELD-1` (PANEL-A) and
   `WELD-2` (PANEL-B + BRKT) turn them into ASSY.
2. **Unloader** — `UNLOADER-A` at `FGN_001` drains ASSY. Core's `auto_push` re-pulls its next
   full when its window frees; the sim operator CLEARs after `unloader_auto_clear` and the
   empty goes to `SYN_PRESS_EMPTIES`.
3. **Maintained group** — `SYN_PRESS_EMPTIES` keeps a level of empties beside the presses;
   its keeper tops it up and the surplus overflows to `SYN_MARKET`.
4. **Loader legs** — `PLK_X1` (CLIP → WELD-3), `PLK_X2` (STUD → WELD-4), the two-window
   BRKT loader `PLK_W1/W2` (→ WELD-2), and the dedicated SHIM home `PLK_H1` that `WELD-5`
   swaps against. The sim operator LOADs after `loader_auto_load`.
5. **Reshuffle** — `SYN_MARKET`'s depth-3 lanes bury older bins behind newer ones, so FIFO
   picks fire unbury → retrieve → restock compounds.

Edge 1 schedules no changeovers. Edge 2's sim operator runs the ones listed under its
`sim.operators.changeovers`.

## Tuning the simulation

Sim behavior lives in the dev configs (baked into the image — rebuild with
`make dev` after editing, or mount your own over `/etc/shingo`):

- **`shingo-core/shingocore.dev.yaml`** → `sim:` — `speed` (sim-time multiplier; every
  process reads the shared anchor in `.env`), `transit_min` / `transit_max` (robot move
  time), `fleet_size`, `fail_rate` (fault injection), `seed` (0 = derive + log; set it for
  reproducible runs).
- **`shingo-edge/shingoedge.dev.yaml`** → `sim:` — `processes[].tick_interval` /
  `uop_per_tick` (how fast presses/lines count), and `operators.loader_auto_load` /
  `unloader_auto_clear` / `swap_release` (operator reaction delays) and
  `operators.changeovers` (scheduled changeovers; Edge 2's config has them).

## Reset / stop / wipe

| Command | Effect |
|---|---|
| `make dev-down` | stop the stack, keep data volumes |
| `make dev-reset` | stop + delete volumes → fresh empty DBs on next `make dev` |
| `make dev-seed` | (re-)seed `SHINGO_PLANT` (default demo-sim.yaml) — idempotent per node name; `dev-reset` first when switching plants |
| `make dev-wipe` | *(planned)* truncate operational data + re-seed, keep topology |
| `make dev-logs` | tail core + edge logs |

**The databases live in named Docker volumes — `pgdata` for Core's Postgres, `edge-data`
and `edge2-data` for the Edges' SQLite — so they survive `make dev`, an image rebuild, a container restart and
`make dev-down` alike.** That is deliberate (a rebuild should not cost you the seeded plant),
and it is also the usual reason a change appears not to have taken: `make dev` rebuilds the
binary, not the data, so a migration you just wrote runs against a database that already has
it, and a `plants/demo-sim.yaml` edit does nothing until you reseed. `make dev-reset` (`down -v`)
is the only command here that drops either volume.

**Long-run disk:** `cell_part_events` is bounded at 90 days (a partition
drop on a daily ticker; it carries its own dedup key, so there is no side
table), and the edge's `counter_snapshots` at 14 days. None of
those windows help a soak that fast-forwards weeks of production in hours —
`make dev-reset` periodically.

## Tests and coverage

`make gate` runs everything CI enforces except the docker suites. For a coverage
number, ask the toolchain rather than a checked-in map — a map is only true on
the day it was captured:

```sh
go test -tags=docker -coverprofile=cover.out ./... && go tool cover -func=cover.out
```

(Drop `-tags=docker` to skip the suites that need containers; the number falls
accordingly. `go tool cover -html=cover.out` for the per-line view.)

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Core/edge refuse to boot, log "SHINGO_ALLOW_SIM=1 not set" | The compose files set it; if running a binary by hand, `export SHINGO_ALLOW_SIM=1`. |
| Edge log: no `plc connected` lines | The fake WarLink poller didn't start — check `sim.enabled: true` and that the edge log shows "starting WarLink poller explicitly". |
| `make dev-seed` fails with "plantspec invalid" | The plant spec has a dangling reference / missing staging — the error lists every problem; fix `plants/demo.yaml`. |
| `make dev-seed` fails opening the edge SQLite | The edge container must be up + migrated first (run `make dev` before `make dev-seed`). |
| Kanban never fires a demand | Storage isn't under a LANE/NGRP parent — the seed builds this; if you hand-edit nodes, keep the zone → lane → slot hierarchy. |
| Counters never climb | Reporting-point `plc_name`/`tag_name` must exactly match `shingoedge.dev.yaml`'s `sim.processes`; the seed keeps them aligned. |

## Internals

- Architecture and the rest of the sim reference live in `docs/dev-env/sim.md`
  (what is built, the speed ceiling, which loops scale with sim time) and
  `docs/dev-env/plant-model.md` (the plant it runs).
- Sim seams: `shingo-core/fleet/simulator` (driver), `shingo-edge/plc/simwarlink`
  (fake PLC), `shingo-edge/engine/sim_operator.go` (auto operator),
  `shingo-core/plantspec` + `shingo-core/cmd/seeddev` (plant spec + seeder).
