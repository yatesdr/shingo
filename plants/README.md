# Plant specs (local dev)

Declarative plant specifications consumed by `cmd/seeddev`.
`docker-compose.dev.yml` mounts this directory read-only into the `seed` service
at `/plants`.

**LOCAL DEV / SIMULATION ONLY.** None of these describes a real plant. Springfield
and Hopkinsville are configured through the Nodes page, not from a spec file.

## The specs

| File | What it is |
|---|---|
| `demo.yaml` | The demo plant — the general-purpose dev fixture. Presses, welds, loaders, an unloader, four swap modes. |
| `lane-stress.yaml` | A rig aimed at one seam: single-file lanes, the mouth gate, digs, and contention for a corridor. **Frozen baseline** — see below. |
| `lane-stress-packed.yaml` | The corrected re-cut of the above. **New baselines are cut here.** |

The lane-stress rigs are not models of a plant. Cell fidelity is borrowed
wholesale from `demo.yaml` — same presses, welds, loaders, unloader, swap modes
and PLC tick rates — precisely so none of that is the variable. Everything that
differs is storage.

### Why there are two, and which to use

`lane-stress.yaml` **ships with two known defects and is not corrected.** Its
census reports two air bubbles in `SYN_COMP` on every load, by name: `LSC_012`
behind `LSC_009`, and `LSC_019` behind `LSC_017`. Both are seeded rather than
dug — no bin was ever at either slot — and they are most of why that group runs
at a 2.7% working margin.

It stays broken on purpose: it is the baseline every A/B on this stream is
measured against, and correcting it would delete the comparison it exists for.

**Use `lane-stress-packed.yaml` for new work.** Reach for `lane-stress.yaml` only
when you are comparing against a figure that was taken on it.

### The census will tell you

A spec is checked at seed time (`shingo-core/plantspec/census.go`) for two
properties: partially filled lanes packed from the back, and at least one lane's
worth of free slots per group so a dig has somewhere to put blockers. A hole at
birth is a slot no robot can reach and no dig can create room in — a seeder
defect, not a plant condition. If you write a new spec, read what the census says
about it before trusting a number taken on it.

## Tooling

| Tool | What it does |
|---|---|
| `shingo-core/cmd/soakstat` | Reads a soak run and reports on it. |
| `scripts/soak-watch.sh` | Watches a running soak. |
| `shingo-core/cmd/simcalc` | Plant-spec arithmetic, including carrier counts. |
| `shingo-core/scripts/shardplan` | Plans the test shards for the sharded CI suite. |

## A caveat on numbers taken before 2026-08-16

The rig's simulated clock ran at two speeds until `169e37c5`. Any measurement
taken on this stream before that commit was against the wrong timeline. If you
are comparing against an older figure, check when it was taken.
