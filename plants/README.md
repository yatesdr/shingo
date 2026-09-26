# Plant specs (local dev)

Declarative plant specifications consumed by `cmd/seeddev`.
`docker-compose.dev.yml` mounts this directory read-only into the `seed` service
at `/plants`.

**LOCAL DEV / SIMULATION ONLY.** None of these describes a real plant. Springfield
and Hopkinsville are configured through the Nodes page, not from a spec file.

## The specs

| File | What it is |
|---|---|
| `demo.yaml` | The demo plant — the general-purpose dev fixture. Presses, welds, loaders, an unloader, four swap modes. **Also the planner golden's plant** (`cmd/seeddev` planner golden, `seed_demo_mg2_test`, `plantspec/demo_test`): a change here moves the golden. |
| `demo-sim.yaml` | The sim rig's demo: `demo.yaml` plus five PANEL-B bins (so a 40-minute run stays healthy) and `loader_settings` giving the unloader Core's `auto_push`. Keep it in step with `demo.yaml`; it differs only by the lines marked `demo-sim`. |
| `lane-stress.yaml` | A rig aimed at one seam: single-file lanes, the mouth gate, digs, and contention for a corridor. **Frozen baseline, retired from certification** — see below. |
| `lane-stress-packed.yaml` | The corrected re-cut of the above. **New baselines are cut here.** Carries two extra PANEL-B bins (2026-09-26) so a 40-minute run stays healthy. |

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

**Retired from certification (2026-09-26).** The dig-walk wedge `lane-stress.yaml`
existed to reproduce is fixed, so it now stays only as the replay fixture for that
wedge's pin. Certification pairs run on `lane-stress-packed.yaml`, `demo-sim.yaml`
and `edge2.yaml`.

### Sized to stay healthy for the run

A sim run measures the code only while the plant is healthy. `simcalc` (rates and
`-carriers`) must show a spec lasting the run's length before a number taken on it
counts: at 10x (the rig's speed since 2026-09-26), a 20-minute run is about 200
sim-minutes. On 2026-09-26 both market
specs failed that for one reason: PRESS-2 makes PANEL-B at 5/min against WELD-2's
6/min, so the PANEL-B buffer is the clock. `demo.yaml`'s 115 uop lasted 1.9 sim-h
(about 23 wall-minutes; demo starved there), and `lane-stress-packed.yaml`'s 205 uop
lasted 3.4 sim-h (about 41, with no margin). `demo-sim.yaml` (+5 bins) and the packed
re-cut (+2) hold 265 uop, 4.4 sim-h, about 53 wall-minutes, with carrier pools and
dig headroom still passing. More stock moves the clock; it does not remove the
deficit. A longer run needs the rate fixed, not more bins.

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

## The unloader's `auto_push` lives in `loader_settings`

A claim's `auto_push` seeds only the Edge's stored claim. The unloader's re-pull when
its window frees reads Core's `bin_loaders.auto_push`, which only a spec's
`loader_settings` section sets. A spec that declares `auto_push` on the claim alone
gets an unloader that re-pulls only on a producer's capture release. The Stage A
soak (2026-09-26) ran that way: 22 pulls in 90 minutes, with its one window free for
up to 9.5 wall-minutes at a time. `demo-sim.yaml` and `lane-stress-packed.yaml` set it;
`demo.yaml` (the golden's plant) and the frozen `lane-stress.yaml` do not.

