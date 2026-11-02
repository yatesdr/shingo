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
| `edge2.yaml` | EDGE 2's line (station `edge2.line1`): the observable-window changeover plant, seeded by the `seed-edge2` service onto the same Core as `demo-sim.yaml`. |
| `order-carts.yaml` | The multi-cart fixture: the two cell shapes in which one order carries several carts (PRESS-1's press-index R2, WELD-2's single_robot relay), with demo-sim's loop around them so they run. Seeded and driven by `cmd/seeddev/order_carts_fixture_docker_test.go` (in the docker shards): the two-cart swap, the relay swap, a CANCEL with the second cart on the robot, a set-down's event withheld, and a hand move of a cart on a robot. See below. |
| `kanban-press.yaml` | **The Kanban certification rig.** A two-off press on one lineside kanban of 16 slots, ten admitting the 45x48 family and six admitting 32x30, with no sources; a weld on a kanban of its own; a plain supermarket stocked with 45x48 empties, which must never lose a cart while a family is whole. Pools 10 and 6. Runs: `docs/fleet-simulator/kanban.md`. |
| `kanban-press5.yaml` | **The same at five presses:** one shared unloader of 10 windows, one kanban over 20 positions, mix 2/2/2/2, pools 26 and 22, 48 slots. Runs: `docs/fleet-simulator/kanban.md`. |

`docker-compose.dev.yml`'s `seed` reads `SHINGO_PLANT`, default `/plants/demo-sim.yaml`
(it was `demo.yaml` until 2026-10-28; a sim seeded from that wedges, see below).

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

### The two-Edge pair's balance (2026-10-26)

The certification pair runs `demo-sim.yaml` on Edge 1 and `edge2.yaml` on Edge 2.
Two of its long waits were the plant's, in every run and on `main`'s baseline too,
so every report read them as defects or passed them over as working. Both are
rebalanced, so a long wait in the pair reads as a defect until shown otherwise:

- **PART-X asks as PART-W does** (`edge2.yaml`). L2-B claims PART-X at three
  cells, and three carriers all stood at them, so the first drained cell's swap
  waited 52-74 sim-min until the B -> A changeover cancelled it. A fourth carrier
  (lane `L2_Lane_X4`, `L2-BX-04`) served the first swap only: the PART-X loader
  was asked once, at a total of 19, because Core's in-loop total counts the
  carriers at the cells as stock, 60 against a threshold of 20. PART-X's threshold
  is now 80, above its cells as PART-W's is, and
  `TestEdge2Fixture_ThresholdCoversSeatStock` counts a part's seats under every
  style its line can change over to (it counted the seed's style alone, so PART-X
  had none). A loader holds its part's total at the threshold even while its style
  is idle, so the pool gained four empties (`L2_Lane_MT4`, `L2_Lane_MT5`): 18
  carriers, the arithmetic at the top of `edge2.yaml`.
- **No downtime model** (removed from the Edge sim, its config and Core's
  projection of its events). It gave PRESS-1, PRESS-2 and WELD-1..4 85% availability
  in clustered 5-30 min outages, and PRESS-1 is WELD-1's only feed with no PANEL-A
  full spare in the market, so every press outage became a 15-30 sim-min wait of
  WELD-1's swap pair, about every two sim-hours. Slowing WELD-1 (14s) was tried
  first and did not change it: a weld that waits for its feed draws what the feed
  makes, whatever its tick.

The baseline for the pair is one run with no rig cases at the tip, read before the
pair (soakstat's FIXTURE BALANCE and every wait over 10 sim-min by order id).

### The multi-cart fixture (`order-carts.yaml`)

An order's carts are its `order_bins` rows, one per lift (`docs/data-model.md`,
"Order carts"). Most orders carry one. This plant keeps only the two cells
whose orders carry several, so a run or a test reads their carts and nothing else:

- **PRESS-1**, `two_robot_press_index`. R2 lifts the on-deck cart off PLN_002,
  sets it on the press PLN_001, lifts an empty from SYN_PRESS_EMPTIES and sets it
  on PLN_002: two carts, one order.
- **WELD-2** at ALN_003, `single_robot`. One robot parks the fresh cart at inbound
  staging, waits at the line, lifts the spent cart and parks it at outbound
  staging, re-lifts the fresh one onto the line and the spent one away: two
  carts, four lifts, two of them relays.

PRESS-2, WELD-1, UNLOADER-A and the level keeper are demo-sim's, kept so the loop
runs. WELD-2 eats no BRKT here (the loaders are not this plant's subject), which
removes the input coupling `shingoedge.dev.yaml` notes against WELD-2's draw.

**Sizing.** `simcalc -plant plants/order-carts.yaml -carriers` passes: 24 empties
seeded against 12 required, SYN_MARKET ok, SYN_PRESS_EMPTIES level-kept at 6, dig
headroom ok. On nameplate rates `simcalc` reads PANEL-A and PANEL-B as STARVE, as
it does for `demo-sim.yaml`; both plants run on the welds' measured draw, set by
their swap cadence (about 1.8-2.0 per minute against the presses' 2.4-3.9, see
`shingoedge.dev.yaml`). The market stock is demo-sim's: 115 uop of PANEL-A and
265 of PANEL-B.

**The test.** `TestOrderCartsFixture_*` seeds this file into a fresh database,
starts an engine on the simulator, and drives each scenario through the
dispatcher's doors and the fleet's block events. After every injected state it
asks the invariant: every cart has an order on record, and it is the order that
moved it.

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

A loader claim seeds no Edge claim row, so a claim's `auto_push` seeds nothing (nor do its
`auto_reorder`, `reorder_point` or `auto_confirm`). The unloader's re-pull when its window
frees reads Core's `bin_loaders.auto_push`, which only a spec's `loader_settings` section sets. A spec that declares `auto_push` on the claim alone
gets an unloader that re-pulls only on a producer's capture release. The Stage A
soak (2026-09-26) ran that way: 22 pulls in 90 minutes, with its one window free for
up to 9.5 wall-minutes at a time. `demo-sim.yaml`, `lane-stress-packed.yaml` and
`edge2.yaml` set it; `demo.yaml` (the golden's plant) and the frozen `lane-stress.yaml` do
not. On 2026-10-28 a laptop sim seeded from `demo.yaml` wedged about three sim-hours in:
22 pulls against 46 ASSY releases, 24 full ASSY in the market and no empty left.

