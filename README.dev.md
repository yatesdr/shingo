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
make dev          # build the 3 sim binaries + bring up postgres, kafka, core, edge
make dev-seed     # seed the demo plant (plants/demo.yaml) into core + edge
```

Then open the UIs:

| | default (loopback) | LAN / tailnet |
|---|---|---|
| Core | http://localhost:8083 | set `SHINGO_BIND=0.0.0.0` (or a tailnet IP) before `make dev` |
| Edge | http://localhost:8081 | same |

`SHINGO_BIND` only affects the core + edge UIs; Postgres and Kafka stay
loopback-only. Tip: `echo "SHINGO_BIND=0.0.0.0" > .env` makes it stick across
`make dev` runs (Compose auto-loads `.env`; it's gitignored).

Until you run `make dev-seed`, the UIs come up **empty** — the sim engine is
alive (driver running, fake PLCs connected, counters ticking) but has no plant
to act on.

## What the demo plant gives you

Two supermarket zones (NGRP → lanes → depth-ordered slots) with bins buried at
depth 2–3, two presses, a paired A/B weld cell, a changeover-ready weld line,
two consumption lines, two bin loaders, and an unloader. Watch for:

1. **Press loop** — `PRESS-1`'s counter climbs (fake PLC) → UOP fills → auto-swap removes the full bin, delivers an empty → counter resumes.
2. **Consumption loop** — `LINE1-IN` UOP drains → reorder fires at threshold → a full bin is retrieved + delivered → consumption resumes.
3. **Loader loop** — demand → empty delivered to `LOADER-1` → sim operator auto-LOADs after `loader_auto_load` → full stored to the supermarket.
4. **Unloader loop** — full bin delivered to `UNLOADER-1` → sim operator auto-CLEARs after `unloader_auto_clear` → empty stored.
5. **Reshuffle** — requesting `PART-A` (buried at `SM-A02`/`SM-A03`) triggers an unbury → retrieve → restock compound order.

(A/B cycling and changeover auto-cutover are scaffolded; see the dev-env notes.)

## Tuning the simulation

Sim behavior lives in the dev configs (baked into the image — rebuild with
`make dev` after editing, or mount your own over `/etc/shingo`):

- **`shingo-core/shingocore.dev.yaml`** → `sim:` — `transit_time` (robot move
  time), `jitter_pct`, `fail_rate` (fault injection), `seed` (0 = derive + log;
  set it for reproducible runs).
- **`shingo-edge/shingoedge.dev.yaml`** → `sim:` — `processes[].tick_interval` /
  `uop_per_tick` (how fast presses/lines count), and `operators.loader_auto_load`
  / `unloader_auto_clear` (operator reaction delays).

## Reset / stop / wipe

| Command | Effect |
|---|---|
| `make dev-down` | stop the stack, keep data volumes |
| `make dev-reset` | stop + delete volumes → fresh empty DBs on next `make dev` |
| `make dev-seed` | (re-)seed the demo plant — idempotent, safe to re-run |
| `make dev-wipe` | *(planned)* truncate operational data + re-seed, keep topology |
| `make dev-logs` | tail core + edge logs |

**Long-run disk:** `cell_part_events` partitions and `production_tick_dedup`
grow unbounded during a soak — `make dev-reset` periodically.

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

- Architecture, phase-by-phase build notes, and the engine-API gap log live in
  `docs/dev-env-api-gaps.md` and the dev-env working docs.
- Sim seams: `shingo-core/fleet/simulator` (driver), `shingo-edge/plc/simwarlink`
  (fake PLC), `shingo-edge/engine/sim_operator.go` (auto operator),
  `shingo-core/plantspec` + `shingo-core/cmd/seeddev` (plant spec + seeder).
